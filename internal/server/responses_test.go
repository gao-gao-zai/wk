package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
)

func TestResponsesPreservesPromptCacheFields(t *testing.T) {
	body, stream, err := responsesToChat([]byte(`{
		"model":"glm-5.2",
		"input":"hello",
		"instructions":"be concise",
		"stream":true,
		"prompt_cache_key":"agent-42",
		"prompt_cache_retention":"24h"
	}`))
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	if !stream {
		t.Fatal("stream=false")
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("chat body is not JSON: %v", err)
	}
	if got["prompt_cache_key"] != "agent-42" || got["prompt_cache_retention"] != "24h" {
		t.Fatalf("cache fields lost: %v", got)
	}
	if got["input"] != nil || got["instructions"] != nil {
		t.Fatalf("Responses-only input fields leaked into chat body: %v", got)
	}
}

func TestResponsesInputImageConvertsToChatImageURL(t *testing.T) {
	body, _, err := responsesToChat([]byte(`{
		"model":"glm-5.2",
		"input":[{"role":"user","content":[
			{"type":"input_text","text":"describe this"},
			{"type":"input_image","image_url":"data:image/png;base64,AA==","detail":"high"}
		]}]
	}`))
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("chat body is not JSON: %v", err)
	}
	messages := got["messages"].([]any)
	content := messages[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content parts=%d, want 2: %#v", len(content), content)
	}
	image := content[1].(map[string]any)
	if image["type"] != "image_url" {
		t.Fatalf("converted image type=%v", image["type"])
	}
	imageURL := image["image_url"].(map[string]any)
	if imageURL["url"] != "data:image/png;base64,AA==" || imageURL["detail"] != "high" {
		t.Fatalf("converted image_url=%#v", imageURL)
	}
}

func TestResponsesMalformedInputImageIsRejected(t *testing.T) {
	body, _, err := responsesToChat([]byte(`{
		"model":"glm-5.2",
		"input":[{"role":"user","content":[{"type":"input_image","image_url":{}}]}]
	}`))
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	if err := validateImageParts(body); err == nil {
		t.Fatalf("malformed input_image should be rejected after conversion; body=%s", body)
	}
}

func TestResponsesPromptCacheKeyKeepsAccountAffinity(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		mu.Lock()
		auths = append(auths, authz)
		mu.Unlock()
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	sess := session.New(session.Config{Available: p.AvailableUIDs})
	h := newTestHandler(t, Config{Pool: p, Upstream: up, Session: sess})

	for i := 0; i < 2; i++ {
		rec := doResponsesRequest(h, `{"model":"glm-5.2","input":"hello","stream":true,"prompt_cache_key":"agent-42"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: code=%d body=%s", i, rec.Code, rec.Body)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(auths) != 2 || auths[0] == "" || auths[0] != auths[1] {
		t.Fatalf("same prompt cache key should keep account affinity: %v", auths)
	}
}

func TestResponsesPreviousResponseIDIsNotUsedAsUnstableSessionKey(t *testing.T) {
	key := `{"previous_response_id":"resp-123"}`
	if got := session.ExtractKey([]byte(key)); got != "" {
		t.Fatalf("ExtractKey(previous_response_id)=%q", got)
	}
}

func TestResponsesCacheKeyPriorityPreservesConversationAffinity(t *testing.T) {
	body := []byte(`{"conversation_id":"conversation-1","prompt_cache_key":"cache-1"}`)
	if got := session.ExtractKey(body); got != "conversation-1" {
		t.Fatalf("conversation affinity should remain higher priority, got %q", got)
	}
	if got := session.ExtractKey([]byte(`{"conversation":"conversation-2","prompt_cache_key":"cache-2"}`)); got != "conversation-2" {
		t.Fatalf("Responses conversation should keep affinity, got %q", got)
	}
}

func TestResponsesNonStreamPreservesWorkBuddyRequestID(t *testing.T) {
	const upstreamID = "WB-Request.Mixed_123/abc"
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: {\"request_id\":\"" + upstreamID + "\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", true
	})
	h := newTestHandler(t, Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := doResponsesRequest(h, `{"model":"glm-5.2","input":"hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["id"] != upstreamID {
		t.Fatalf("response ID=%q want unchanged %q", response["id"], upstreamID)
	}
	if rec.Header().Get("X-Request-Id") != upstreamID {
		t.Fatalf("response header ID=%q", rec.Header().Get("X-Request-Id"))
	}
	if _, ok := h.responseHistory[upstreamID]; !ok {
		t.Fatalf("previous_response_id history was not keyed by upstream ID: %#v", h.responseHistory)
	}
}

func TestResponsesStreamPreservesWorkBuddyRecordID(t *testing.T) {
	const upstreamID = "WB.Record-ID_456"
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: {\"record_id\":\"" + upstreamID + "\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", true
	})
	h := newTestHandler(t, Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := doResponsesRequest(h, `{"model":"glm-5.2","input":"hello","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"id":"`+upstreamID+`"`) ||
		!strings.Contains(rec.Body.String(), `"response_id":"`+upstreamID+`"`) {
		t.Fatalf("stream replaced upstream ID: %s", rec.Body.String())
	}
	if rec.Header().Get("X-Request-Id") != upstreamID {
		t.Fatalf("response header ID=%q", rec.Header().Get("X-Request-Id"))
	}
	if _, ok := h.responseHistory[upstreamID]; !ok {
		t.Fatalf("stream history was not keyed by upstream ID: %#v", h.responseHistory)
	}
}

func doResponsesRequest(h http.Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	return rec
}

// TestResponsesStripsConversationIdentityUpstream 保证 Responses→chat 转译后
// 发往上游的 body 不携带会话标识（conversation_id/prompt_cache_key 等）：
// Codex 每轮全量重发历史并复用稳定 key，标识进上游会触发 11148。
// 本地粘性路由在 payload 变换之前提取，不受影响。
func TestResponsesStripsConversationIdentityUpstream(t *testing.T) {
	var mu sync.Mutex
	var seenBodies []string
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		seenBodies = append(seenBodies, string(raw))
		mu.Unlock()
		body := "data: {\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})
	h := newTestHandler(t, Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	for i := 0; i < 2; i++ {
		rec := doResponsesRequest(h, `{"model":"glm-5.2","input":"hello","stream":true,"prompt_cache_key":"agent-42","include":["reasoning.encrypted_content"],"store":false}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: code=%d body=%s", i, rec.Code, rec.Body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seenBodies) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(seenBodies))
	}
	for i, b := range seenBodies {
		var doc map[string]any
		if err := json.Unmarshal([]byte(b), &doc); err != nil {
			t.Fatalf("request %d: upstream body not JSON: %v (%s)", i, err, b)
		}
		for _, key := range []string{"conversation", "conversation_id", "prompt_cache_key", "prompt_cache_retention", "include", "store"} {
			if _, ok := doc[key]; ok {
				t.Errorf("request %d: %s leaked upstream: %v", i, key, doc[key])
			}
		}
		if meta, ok := doc["metadata"].(map[string]any); ok {
			if _, ok := meta["conversation_id"]; ok {
				t.Errorf("request %d: metadata.conversation_id leaked upstream: %v", i, meta["conversation_id"])
			}
		}
	}
}

// TestResponsesFunctionCallOutputWithoutCallIDBecomesUser 无 call_id 的
// function_call_output 必须降级为 user 消息而不是被静默丢弃——丢弃会留下
// 孤立的 assistant.tool_calls，直接触发上游 11148。
func TestResponsesFunctionCallOutputWithoutCallIDBecomesUser(t *testing.T) {
	body, _, err := responsesToChat([]byte(`{
		"model":"glm-5.2",
		"input":[
			{"type":"function_call","call_id":"call_1","name":"exec","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"paired result"},
			{"type":"function_call_output","output":"orphan result"}
		]
	}`))
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("chat body is not JSON: %v", err)
	}
	messages := got["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages=%d, want 3 (assistant+tool+user): %#v", len(messages), messages)
	}
	orphan := messages[2].(map[string]any)
	if orphan["role"] != "user" {
		t.Fatalf("orphan output role=%v, want user", orphan["role"])
	}
	if orphan["content"] != "orphan result" {
		t.Fatalf("orphan output content=%v", orphan["content"])
	}
	// 有 call_id 的配对不受影响。
	tool := messages[1].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" {
		t.Fatalf("paired output altered: %#v", tool)
	}
}
