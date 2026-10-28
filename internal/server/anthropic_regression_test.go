package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func anthropicRegressionRequest(t *testing.T, frames string, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, frames, true })
	h := newTestHandler(t, Config{AnthropicAPI: true, Pool: testPoolWith(&auth.Auth{UID: "regression-account", AccessToken: "fake-token", ExpiresAt: 9999999999}), Upstream: up})
	body := `{"model":"glm-5.2","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"get_weather","input_schema":{"type":"object","properties":{}}}]`
	if stream {
		body += `,"stream":true`
	}
	body += `}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body)
	}
	return rec
}

func anthropicRegressionResult(t *testing.T, rec *httptest.ResponseRecorder, stream bool) (string, map[string]any) {
	t.Helper()
	if !stream {
		var message map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &message); err != nil {
			t.Fatal(err)
		}
		return message["stop_reason"].(string), message["usage"].(map[string]any)
	}
	if strings.Contains(rec.Body.String(), "event: error") || !strings.Contains(rec.Body.String(), "event: message_stop") {
		t.Fatalf("stream did not complete: %s", rec.Body)
	}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil && event["type"] == "message_delta" {
			delta := event["delta"].(map[string]any)
			return delta["stop_reason"].(string), event["usage"].(map[string]any)
		}
	}
	t.Fatalf("missing message_delta: %s", rec.Body)
	return "", nil
}

func TestAnthropicToolStopReasonRegression(t *testing.T) {
	for _, tc := range []struct {
		name, finish, want string
		tools              bool
	}{
		{"plain_stop", "stop", "end_turn", false},
		{"tools_with_stop", "stop", "tool_use", true},
		{"explicit_tools", "tool_calls", "tool_use", true},
		{"legacy_function", "function_call", "tool_use", true},
		{"truncated_tools", "length", "max_tokens", true},
		{"filtered_tools", "content_filter", "refusal", true},
	} {
		for _, stream := range []bool{false, true} {
			mode := "sync"
			if stream {
				mode = "stream"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				delta := map[string]any{"content": "hello"}
				if tc.tools {
					delta["tool_calls"] = []any{map[string]any{"index": 0, "id": "call1", "type": "function", "function": map[string]any{"name": "get_weather", "arguments": "{}"}}}
				}
				chunk, err := json.Marshal(map[string]any{"id": "regression-stop", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": tc.finish}}})
				if err != nil {
					t.Fatal(err)
				}
				rec := anthropicRegressionRequest(t, "data: "+string(chunk)+"\n\ndata: [DONE]\n\n", stream)
				stop, _ := anthropicRegressionResult(t, rec, stream)
				if tc.tools && !strings.Contains(rec.Body.String(), `"type":"tool_use"`) {
					t.Fatalf("tool block lost: %s", rec.Body)
				}
				if stop != tc.want {
					t.Fatalf("stop_reason=%s; want %s", stop, tc.want)
				}
			})
		}
	}
}

func TestAnthropicSplitUsageRegression(t *testing.T) {
	for _, tc := range []struct {
		name        string
		usage       []map[string]any
		input, read int
	}{
		{
			name: "input_then_output",
			usage: []map[string]any{
				{"prompt_tokens": 10, "prompt_tokens_details": map[string]any{"cached_tokens": 6}},
				{"completion_tokens": 4},
			},
			input: 4, read: 6,
		},
		{
			name: "late_input",
			usage: []map[string]any{
				{"completion_tokens": 4},
				{"prompt_tokens": 10, "prompt_tokens_details": map[string]any{"cached_tokens": 6}},
			},
			input: 4, read: 6,
		},
		{
			name: "explicit_zero_cache",
			usage: []map[string]any{
				{"prompt_tokens": 10, "prompt_tokens_details": map[string]any{"cached_tokens": 6}},
				{"completion_tokens": 4, "prompt_tokens_details": map[string]any{"cached_tokens": 0}},
			},
			input: 10, read: 0,
		},
		{
			name: "updated_cumulative_input",
			usage: []map[string]any{
				{"prompt_tokens": 10, "prompt_tokens_details": map[string]any{"cached_tokens": 6}},
				{"prompt_tokens": 12, "completion_tokens": 4},
			},
			input: 6, read: 6,
		},
	} {
		for _, stream := range []bool{false, true} {
			mode := "sync"
			if stream {
				mode = "stream"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				frames := "data: " + `{"id":"regression-usage","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":"stop"}]}` + "\n\n"
				for _, usage := range tc.usage {
					chunk, err := json.Marshal(map[string]any{"choices": []any{}, "usage": usage})
					if err != nil {
						t.Fatal(err)
					}
					frames += "data: " + string(chunk) + "\n\n"
				}
				rec := anthropicRegressionRequest(t, frames+"data: [DONE]\n\n", stream)
				_, usage := anthropicRegressionResult(t, rec, stream)
				if usage["input_tokens"] != float64(tc.input) || usage["cache_read_input_tokens"] != float64(tc.read) || usage["output_tokens"] != float64(4) {
					t.Fatalf("usage=%v; want input=%d cache_read=%d output=4", usage, tc.input, tc.read)
				}
			})
		}
	}
}
