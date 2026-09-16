package upstream

import (
	"encoding/json"
	"testing"
)

// mustPrepareBody / mustPrepareBodyWithEfforts 是测试侧的薄包装。
// PrepareBodyOpt 现在返回 error（fail-closed），但测试关心的是成功路径的
// 输出内容，所以在这里把错误检查收敛掉，保持各用例可读。
func mustPrepareBody(t *testing.T, src []byte, sanitize bool) []byte {
	t.Helper()
	out, err := PrepareBodyOpt(src, sanitize)
	if err != nil {
		t.Fatalf("PrepareBodyOpt: %v", err)
	}
	return out
}

func mustPrepareBodyWithEfforts(t *testing.T, src []byte, sanitize bool, efforts map[string][]string) []byte {
	t.Helper()
	out, err := PrepareBodyOptWithEfforts(src, sanitize, efforts)
	if err != nil {
		t.Fatalf("PrepareBodyOptWithEfforts: %v", err)
	}
	return out
}

func TestNormalizeModelID(t *testing.T) {
	cases := map[string]string{
		"kimi-k3-1": "kimi-k3",
		"kimi-k2.7": "kimi-k2.7-code",
		"glm-5.2":   "glm-5.2",
	}
	for input, want := range cases {
		if got := NormalizeModelID(input); got != want {
			t.Errorf("NormalizeModelID(%q)=%q want %q", input, got, want)
		}
	}
}

func TestPrepareBodyNormalizesModelID(t *testing.T) {
	out, err := PrepareBodyOpt([]byte(`{"model":"kimi-k2.7","messages":[]}`), true)
	if err != nil {
		t.Fatalf("PrepareBodyOpt: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "kimi-k2.7-code" {
		t.Fatalf("model=%v want kimi-k2.7-code", body["model"])
	}
}

func TestPrepareBodyMapsDeveloperRoleToSystem(t *testing.T) {
	out := mustPrepareBody(t, []byte(`{
		"model":"glm-5.2",
		"messages":[
			{"role":"developer","content":"follow these rules"},
			{"role":"Developer","content":"case-insensitive compatibility"},
			{"role":"user","content":"hello"},
			{"role":"assistant","content":"hi"}
		]
	}`), false)

	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 4 {
		t.Fatalf("messages=%#v", body["messages"])
	}
	wantRoles := []string{"system", "system", "user", "assistant"}
	for i, want := range wantRoles {
		message, ok := messages[i].(map[string]any)
		if !ok {
			t.Fatalf("messages[%d]=%#v", i, messages[i])
		}
		if got, _ := message["role"].(string); got != want {
			t.Errorf("messages[%d].role=%q want %q", i, got, want)
		}
	}
	if got := messages[0].(map[string]any)["content"]; got != "follow these rules" {
		t.Errorf("developer content changed: %v", got)
	}
}

func TestPrepareBodyAddsSystemPromptBeforeUserMessage(t *testing.T) {
	out := mustPrepareBody(t, []byte(`{
		"model":"deepseek-v4.1-flash",
		"messages":[{"role":"user","content":"hello"}]
	}`), false)

	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("messages=%#v", body["messages"])
	}
	first, ok := messages[0].(map[string]any)
	if !ok {
		t.Fatalf("messages[0]=%#v", messages[0])
	}
	if first["role"] != "system" {
		t.Fatalf("first role=%v want system", first["role"])
	}
	if first["content"] != "You are a helpful assistant." {
		t.Fatalf("default system content=%v", first["content"])
	}
	second, _ := messages[1].(map[string]any)
	if second["role"] != "user" || second["content"] != "hello" {
		t.Fatalf("user message changed: %#v", second)
	}
}

func TestPrepareBodyKeepsExistingSystemPromptFirst(t *testing.T) {
	out := mustPrepareBody(t, []byte(`{
		"messages":[
			{"role":"system","content":"follow project rules"},
			{"role":"user","content":"hello"}
		]
	}`), false)

	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	messages := body["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages=%#v", messages)
	}
	first := messages[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "follow project rules" {
		t.Fatalf("existing system prompt changed: %#v", first)
	}
}

func TestPrepareBodyOptWithEfforts(t *testing.T) {
	efforts := map[string][]string{
		"glm-5.2":      {"off", "low", "high"},
		"glm-5.2-mini": {"low", "medium"},
		"glm-5.2-max":  {"high", "xhigh"},
	}
	cases := []struct {
		name    string
		body    string
		efforts map[string][]string
		wantKey string // 输出应带有的 effort 字段名；空表示该字段应不存在
		wantVal string // 期望值
	}{
		{"downgrade to highest supported at or below request",
			`{"model":"glm-5.2-mini","reasoning_effort":"high"}`, efforts, "reasoning_effort", "medium"},
		{"floor to lowest when all supported above request",
			`{"model":"glm-5.2-max","reasoning_effort":"low"}`, efforts, "reasoning_effort", "high"},
		{"supported effort passes through unchanged",
			`{"model":"glm-5.2","reasoning_effort":"low"}`, efforts, "reasoning_effort", "low"},
		{"camelCase field name downgrades and keeps key",
			`{"model":"glm-5.2-mini","reasoningEffort":"high"}`, efforts, "reasoningEffort", "medium"},
		{"unknown model passes through",
			`{"model":"unknown","reasoning_effort":"max"}`, efforts, "reasoning_effort", "max"},
		{"unknown effort value passes through",
			`{"model":"glm-5.2","reasoning_effort":"ultra"}`, efforts, "reasoning_effort", "ultra"},
		{"empty cache passes through",
			`{"model":"glm-5.2","reasoning_effort":"max"}`, map[string][]string{}, "reasoning_effort", "max"},
		{"no effort field untouched",
			`{"model":"glm-5.2-mini","messages":[]}`, efforts, "", ""},
		{"nil efforts map passes through",
			`{"model":"glm-5.2","reasoning_effort":"max"}`, nil, "reasoning_effort", "max"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := PrepareBodyOptWithEfforts([]byte(c.body), false, c.efforts)
			if err != nil {
				t.Fatalf("PrepareBodyOpt: %v", err)
			}
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatalf("unmarshal: %v (body=%s)", err, out)
			}
			if c.wantKey == "" {
				if _, ok := m["reasoning_effort"]; ok {
					t.Errorf("reasoning_effort should be absent, got %v", m["reasoning_effort"])
				}
				if _, ok := m["reasoningEffort"]; ok {
					t.Errorf("reasoningEffort should be absent, got %v", m["reasoningEffort"])
				}
				return
			}
			got, ok := m[c.wantKey].(string)
			if !ok || got != c.wantVal {
				t.Errorf("%s: got %v (%T) want %q", c.wantKey, m[c.wantKey], m[c.wantKey], c.wantVal)
			}
		})
	}
}

// TestPrepareBodyStripsConversationIdentity 保证会话标识不会发往上游：
// Codex 等客户端每轮全量重发历史并复用稳定 prompt_cache_key，若该标识
// 进入上游 body（metadata.conversation_id 等），上游会按会话累积服务端
// 状态，与重发的历史叠加后触发 11148 tool_call_sequence_broken。
// 会话标识只允许驱动本地粘性路由（在 handler 层、进入本变换前提取）。
func TestPrepareBodyStripsConversationIdentity(t *testing.T) {
	out := mustPrepareBody(t, []byte(`{
		"model":"glm-5.2",
		"messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}],
		"conversation":"conv-1",
		"conversation_id":"conv-2",
		"prompt_cache_key":"agent-42",
		"prompt_cache_retention":"24h",
		"metadata":{"conversation_id":"conv-3","user_id":"u-9"}
	}`), false)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, out)
	}
	for _, key := range []string{"conversation", "conversation_id", "prompt_cache_key", "prompt_cache_retention"} {
		if _, ok := m[key]; ok {
			t.Errorf("%s leaked into upstream body: %v", key, m[key])
		}
	}
	meta, ok := m["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata dropped entirely (user_id must survive): %v", m["metadata"])
	}
	if _, ok := meta["conversation_id"]; ok {
		t.Errorf("metadata.conversation_id leaked into upstream body: %v", meta["conversation_id"])
	}
	if meta["user_id"] != "u-9" {
		t.Errorf("metadata.user_id lost: %v", meta["user_id"])
	}
	// 消息与其余字段原样保留。
	if msgs, ok := m["messages"].([]any); !ok || len(msgs) != 2 {
		t.Errorf("messages altered: %v", m["messages"])
	}
	if m["stream"] != true {
		t.Errorf("stream must stay forced true, got %v", m["stream"])
	}
}

// TestPrepareBodyDropsEmptyMetadata 剥离后 metadata 只剩会话字段时应整体移除，
// 不给上游留一个空对象。
func TestPrepareBodyDropsEmptyMetadata(t *testing.T) {
	out := mustPrepareBody(t, []byte(`{
		"model":"glm-5.2",
		"messages":[{"role":"user","content":"hi"}],
		"metadata":{"conversation_id":"conv-1"}
	}`), false)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["metadata"]; ok {
		t.Errorf("empty metadata should be dropped, got %v", m["metadata"])
	}
}
