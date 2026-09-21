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
	body, stream, err, _ := responsesToChat([]byte(`{
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
	body, _, err, _ := responsesToChat([]byte(`{
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
	body, _, err, _ := responsesToChat([]byte(`{
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

// TestResponsesNonStreamIncludesReasoningItem 非流式：上游 chat 响应带
// reasoning_content 时，Responses 输出必须包含 type=reasoning 的 item，
// 而不是把思考内容整段丢弃。
func TestResponsesNonStreamIncludesReasoningItem(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: {\"id\":\"chatcmpl-r1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"让我想想\"}}]}\n\n" +
			"data: {\"id\":\"chatcmpl-r1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"答案是 2\"}}]}\n\n" +
			"data: {\"id\":\"chatcmpl-r1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
			"data: [DONE]\n\n", true
	})
	h := newTestHandler(t, Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := doResponsesRequest(h, `{"model":"glm-5.2","input":"1+1=?"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	output, _ := response["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output items=%d, want 2 (reasoning + message): %#v", len(output), output)
	}
	reasoning, ok := output[0].(map[string]any)
	if !ok || reasoning["type"] != "reasoning" {
		t.Fatalf("first output item should be reasoning, got %#v", output[0])
	}
	content, _ := reasoning["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("reasoning content parts=%d, want 1", len(content))
	}
	if text, _ := content[0].(map[string]any)["text"].(string); text != "让我想想" {
		t.Fatalf("reasoning text=%q want %q", text, "让我想想")
	}
	msg, ok := output[1].(map[string]any)
	if !ok || msg["type"] != "message" {
		t.Fatalf("second output item should be message, got %#v", output[1])
	}
	if response["output_text"] != "答案是 2" {
		t.Fatalf("output_text=%v", response["output_text"])
	}
}

// TestResponsesStreamEmitsReasoningEvents 流式：reasoning_content 增量要转成
// Responses 的 reasoning item 生命周期事件（added / reasoning_text.delta /
// reasoning_text.done / output_item.done），且 completed 的 output 里包含
// 完整思考 item。这是 DSH 等客户端展示思考过程所依赖的事件面。
func TestResponsesStreamEmitsReasoningEvents(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: {\"id\":\"chatcmpl-r2\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"思考A\"}}]}\n\n" +
			"data: {\"id\":\"chatcmpl-r2\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"思考B\"}}]}\n\n" +
			"data: {\"id\":\"chatcmpl-r2\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"正文\"}}]}\n\n" +
			"data: {\"id\":\"chatcmpl-r2\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
			"data: [DONE]\n\n", true
	})
	h := newTestHandler(t, Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := doResponsesRequest(h, `{"model":"glm-5.2","input":"hi","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: response.output_item.added",
		"response.reasoning_text.delta",
		"\"delta\":\"思考A\"",
		"\"delta\":\"思考B\"",
		"event: response.reasoning_text.done",
		"\"text\":\"思考A思考B\"",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream missing %q:\n%s", want, body)
		}
	}
	// added 事件里必须是 reasoning item，且先于正文 message item 出现。
	addedIdx := strings.Index(body, "\"type\":\"reasoning\"")
	msgIdx := strings.Index(body, "response.output_text.delta")
	if addedIdx < 0 || msgIdx < 0 || addedIdx > msgIdx {
		t.Fatalf("reasoning item should appear before text output:\n%s", body)
	}
	// completed 终态里包含完整 reasoning item。
	if !strings.Contains(body, "event: response.completed") {
		t.Fatalf("missing response.completed:\n%s", body)
	}
	completed := body[strings.Index(body, "event: response.completed"):]
	if !strings.Contains(completed, "\"type\":\"reasoning\"") || !strings.Contains(completed, "思考A思考B") {
		t.Fatalf("completed response missing reasoning item:\n%s", completed)
	}
}

// TestResponsesReasoningItemReplayBecomesAssistantReasoning 客户端回放
// type=reasoning 的 input item 时，转译为 assistant.reasoning_content，
// 而不是混进 user 文本污染角色序列。
func TestResponsesReasoningItemReplayBecomesAssistantReasoning(t *testing.T) {
	body, _, err, _ := responsesToChat([]byte(`{
		"model":"glm-5.2",
		"input":[
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"上一轮的思考"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"继续"}]}
		]
	}`))
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	messages := got["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages=%d, want 2 (assistant reasoning + user): %#v", len(messages), messages)
	}
	assistant, ok := messages[0].(map[string]any)
	if !ok || assistant["role"] != "assistant" {
		t.Fatalf("first message should be assistant, got %#v", messages[0])
	}
	if assistant["reasoning_content"] != "上一轮的思考" {
		t.Fatalf("reasoning_content=%v", assistant["reasoning_content"])
	}
	user, ok := messages[1].(map[string]any)
	if !ok || user["role"] != "user" {
		t.Fatalf("second message should be user, got %#v", messages[1])
	}
}

// TestResponsesReasoningItemReplayContentShape 回放 content[].text 形态
// （本网关产出的 reasoning item 形状）同样要映射到 reasoning_content。
func TestResponsesReasoningItemReplayContentShape(t *testing.T) {
	body, _, err, _ := responsesToChat([]byte(`{
		"model":"glm-5.2",
		"input":[
			{"type":"reasoning","id":"rs_2","content":[{"type":"reasoning_text","text":"思考内容"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"go on"}]}
		]
	}`))
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	messages := got["messages"].([]any)
	assistant, ok := messages[0].(map[string]any)
	if !ok || assistant["role"] != "assistant" || assistant["reasoning_content"] != "思考内容" {
		t.Fatalf("assistant reasoning mapping wrong: %#v", messages[0])
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
	body, _, err, _ := responsesToChat([]byte(`{
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
