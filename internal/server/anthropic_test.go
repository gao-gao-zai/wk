package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestAnthropicRequestConversion(t *testing.T) {
	raw := `{"model":"glm-5.2","max_tokens":100,"stream":true,"system":[{"type":"text","text":"rules"}],"messages":[{"role":"user","content":[{"type":"text","text":"look "},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}},{"type":"text","text":"here"}]},{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":{"x":1}},{"type":"tool_use","id":"b","name":"f","input":{"x":2}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"one"},{"type":"tool_result","tool_use_id":"b","content":"two"},{"type":"text","text":"continue"}]}],"tools":[{"name":"f","input_schema":{"type":"object","properties":{"x":{"type":"integer"}},"additionalProperties":false,"$defs":{"x":{"type":"integer"}}}}],"tool_choice":{"type":"none"}}`
	b, stream, _, err := anthropicToChat([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !stream {
		t.Fatal("stream lost")
	}
	var doc map[string]any
	if err = json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	msgs := doc["messages"].([]any)
	if len(msgs) != 6 {
		t.Fatalf("messages: %s", b)
	}
	parts := msgs[1].(map[string]any)["content"].([]any)
	if len(parts) != 3 || parts[1].(map[string]any)["image_url"].(map[string]any)["url"] != "data:image/png;base64,AA==" {
		t.Fatalf("parts: %v", parts)
	}
	if msgs[3].(map[string]any)["tool_call_id"] != "a" || msgs[4].(map[string]any)["tool_call_id"] != "b" || msgs[5].(map[string]any)["role"] != "user" {
		t.Fatalf("tool order: %s", b)
	}
	schema := doc["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	if schema["additionalProperties"] != false || schema["$defs"] == nil {
		t.Fatalf("schema lost: %v", schema)
	}
	if doc["tool_choice"] != "none" {
		t.Fatal(doc)
	}
}
func TestAnthropicRejectsUnsupportedBlocks(t *testing.T) {
	for _, body := range []string{`null`, `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":[{"type":"document"}]}]}`, `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"x"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`} {
		if _, _, _, err := anthropicToChat([]byte(body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}
func TestAnthropicStreamInterleavedToolsAndLateUsage(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newAnthropicStream(rec, "m")
	frames := []string{
		`{"id":"c1","choices":[{"delta":{"content":"hi"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"first","arguments":"{\"x\":"}},{"index":1,"id":"b","function":{"name":"second","arguments":"{\"y\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}},{"index":1,"function":{"arguments":"2}"}}]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":6}}}`,
	}
	for _, f := range frames {
		if _, err := s.Write([]byte("data: " + f + "\n\n")); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = s.Write([]byte("data: [DONE]\n\n"))
	s.finish()
	body := rec.Body.String()
	for _, v := range []string{`"id":"a"`, `"id":"b"`, `"partial_json":"{\"x\":1}"`, `"partial_json":"{\"y\":2}"`, `"cache_read_input_tokens":6`, `"input_tokens":4`, `"output_tokens":4`, `"stop_reason":"tool_use"`, `event: message_stop`} {
		if !strings.Contains(body, v) {
			t.Errorf("missing %s in %s", v, body)
		}
	}
}
func TestAnthropicStreamErrorDoesNotComplete(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newAnthropicStream(rec, "m")
	_, _ = s.Write([]byte("data: {\"error\":{\"message\":\"broken\"}}\n\ndata: [DONE]\n\n"))
	s.finish()
	if !strings.Contains(rec.Body.String(), "event: error") || strings.Contains(rec.Body.String(), "event: message_stop") {
		t.Fatal(rec.Body.String())
	}
}
func TestAnthropicToggleAndAuthentication(t *testing.T) {
	h, path := adminConfigTestEnv(t)
	if h.anthropicEnabled() {
		t.Fatal("default should be off")
	}
	fields := map[string]any{"checkin_hours": []int{9}, "keepalive_hours": []int{22}, "features": map[string]any{"anthropic_api": true}}
	doAdminConfig(t, h, http.MethodPost, fields)
	if !h.anthropicEnabled() {
		t.Fatal("toggle did not apply")
	}
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), `"anthropic_api": true`) {
		t.Fatalf("persist: %s %v", b, err)
	}
	fields["features"] = map[string]any{"anthropic_api": false}
	doAdminConfig(t, h, http.MethodPost, fields)
	if h.anthropicEnabled() {
		t.Fatal("toggle did not disable")
	}
	disabled := httptest.NewRecorder()
	h.ServeHTTP(disabled, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`)))
	if disabled.Code != http.StatusNotFound {
		t.Fatalf("disabled endpoint status=%d body=%s", disabled.Code, disabled.Body)
	}
	raw := NewHandler(Config{APIKey: "secret", AnthropicAPI: true})
	for _, tc := range []struct {
		key  string
		want int
	}{{"wrong", 401}, {"secret", 400}} {
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`))
		req.Header.Set("x-api-key", tc.key)
		rec := httptest.NewRecorder()
		raw.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("key=%s status=%d body=%s", tc.key, rec.Code, rec.Body)
		}
	}
}
func TestAnthropicEndpointEndToEnd(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n", true
	})
	h := newTestHandler(t, Config{AnthropicAPI: true, Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	for _, stream := range []bool{false, true} {
		body := `{"model":"glm-5.2","max_tokens":100,"messages":[{"role":"user","content":"hi"}]`
		if stream {
			body += `,"stream":true`
		}
		body += `}`
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "hello") {
			t.Fatalf("stream=%v code=%d body=%s", stream, rec.Code, rec.Body)
		}
		if stream && !strings.Contains(rec.Body.String(), "event: message_stop") {
			t.Fatal(rec.Body.String())
		}
	}
}

func TestAnthropicErrorToolResultIsPreserved(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call1","content":"failed","is_error":true}]}]}`)
	out, _, _, err := anthropicToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
	message := v["messages"].([]any)[0].(map[string]any)
	if message["content"] != "Tool error: failed" {
		t.Fatalf("tool result: %v", message)
	}
}

func TestAnthropicGroupKeyScope(t *testing.T) {
	h, vipKey, _ := newGroupTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("x-api-key", vipKey)
	rec := httptest.NewRecorder()
	h.withAnthropicAuth(func(w http.ResponseWriter, r *http.Request) {
		scope := scopeFromRequest(r)
		if scope.kind != "group" || scope.group != "vip" {
			t.Errorf("scope: %+v", scope)
		}
		allowed := h.allowSet(scope)
		if !allowed["u1"] || allowed["u2"] {
			t.Errorf("wrong group accounts: %v", allowed)
		}
		w.WriteHeader(http.StatusNoContent)
	})(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
}
