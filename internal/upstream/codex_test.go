package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// codexIdentityOriginal / codexIdentityRewritten 是 Codex CLI 注入的完整
// 身份句与改写后的目标句。改写必须整句逐字替换（只改一处连字符），
// 不能截断或部分匹配，否则会把句子改成不通顺的残句。
const (
	codexIdentityOriginal  = "You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI. You are expected to be precise, safe, and helpful."
	codexIdentityRewritten = "You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open-source project led by OpenAI. You are expected to be precise, safe, and helpful."
)

func withCodexCompat(t *testing.T, enabled bool) {
	t.Helper()
	prev := codexCompatEnabled
	SetCodexCompat(enabled)
	t.Cleanup(func() { SetCodexCompat(prev) })
}

// TestCodexCompatRewritesSystemPrompt：开关打开时 system 消息里的身份句
// 被整句替换，其余内容原样保留。
func TestCodexCompatRewritesSystemPrompt(t *testing.T) {
	withCodexCompat(t, true)
	body := `{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + codexIdentityOriginal + ` Additional guidance."},` +
		`{"role":"user","content":"hello"}]}`
	out := mustPrepareBody(t, []byte(body), false)
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	messages := doc["messages"].([]any)
	system := messages[0].(map[string]any)
	content := system["content"].(string)
	if !strings.Contains(content, codexIdentityRewritten) {
		t.Fatalf("rewritten identity sentence not found: %q", content)
	}
	if strings.Contains(content, "an open source project") {
		t.Fatalf("original identity sentence still present: %q", content)
	}
	if !strings.Contains(content, "Additional guidance.") {
		t.Fatalf("surrounding content lost: %q", content)
	}
}

// TestCodexCompatDisabledByDefault：开关关闭（默认）时一字不改。
func TestCodexCompatDisabledByDefault(t *testing.T) {
	withCodexCompat(t, false)
	body := `{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + codexIdentityOriginal + `"},` +
		`{"role":"user","content":"hello"}]}`
	out := mustPrepareBody(t, []byte(body), false)
	if strings.Contains(string(out), "an open-source project") {
		t.Fatal("codex compat applied while disabled")
	}
	if !strings.Contains(string(out), codexIdentityOriginal) {
		t.Fatal("original sentence must be untouched while disabled")
	}
}

// TestCodexCompatIgnoresAbsentSentence：句子不存在时原样透传（有则改、
// 无则忽略），且普通请求不产生任何额外改动。
func TestCodexCompatIgnoresAbsentSentence(t *testing.T) {
	withCodexCompat(t, true)
	body := `{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"You are a helpful assistant."},` +
		`{"role":"user","content":"plain request"}]}`
	out := mustPrepareBody(t, []byte(body), false)
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	messages := doc["messages"].([]any)
	if got := messages[0].(map[string]any)["content"]; got != "You are a helpful assistant." {
		t.Fatalf("unrelated system prompt modified: %v", got)
	}
	if got := messages[1].(map[string]any)["content"]; got != "plain request" {
		t.Fatalf("user message modified: %v", got)
	}
}

// TestCodexCompatOnlyTouchesSystemMessages：user 消息里出现同一句时
// 不改动——那不是系统提示词，改写等于篡改用户输入。
func TestCodexCompatOnlyTouchesSystemMessages(t *testing.T) {
	withCodexCompat(t, true)
	body := `{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"sys"},` +
		`{"role":"user","content":"` + codexIdentityOriginal + `"}]}`
	out := mustPrepareBody(t, []byte(body), false)
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	messages := doc["messages"].([]any)
	user := messages[1].(map[string]any)
	if got := user["content"].(string); got != codexIdentityOriginal {
		t.Fatalf("user message was rewritten: %q", got)
	}
}

// TestCodexCompatMapsDeveloperRole：developer 角色先映射成 system 再改写
// （Codex 经 Responses API 下发时是 developer/instructions）。
func TestCodexCompatMapsDeveloperRole(t *testing.T) {
	withCodexCompat(t, true)
	body := `{"model":"glm-5.2","messages":[` +
		`{"role":"developer","content":"` + codexIdentityOriginal + `"},` +
		`{"role":"user","content":"hello"}]}`
	out := mustPrepareBody(t, []byte(body), false)
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	messages := doc["messages"].([]any)
	first := messages[0].(map[string]any)
	if got := first["role"]; got != "system" {
		t.Fatalf("developer not mapped to system: %v", got)
	}
	if content := first["content"].(string); !strings.Contains(content, codexIdentityRewritten) {
		t.Fatalf("identity sentence in developer message not rewritten: %q", content)
	}
}

// TestCodexCompatMultimodalContent：content 为多模态数组时只改 text part。
func TestCodexCompatMultimodalContent(t *testing.T) {
	withCodexCompat(t, true)
	body := `{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":[` +
		`{"type":"text","text":"` + codexIdentityOriginal + `"},` +
		`{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}]},` +
		`{"role":"user","content":"hello"}]}`
	out := mustPrepareBody(t, []byte(body), false)
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	messages := doc["messages"].([]any)
	parts := messages[0].(map[string]any)["content"].([]any)
	text := parts[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, codexIdentityRewritten) {
		t.Fatalf("text part not rewritten: %q", text)
	}
	img := parts[1].(map[string]any)
	if _, ok := img["image_url"]; !ok {
		t.Fatalf("image part lost: %v", img)
	}
}

// TestCodexCompatCoexistsWithSanitize：Codex 改写与 Claude 指纹脱敏
// 同时开启时两层互不干扰。
func TestCodexCompatCoexistsWithSanitize(t *testing.T) {
	withCodexCompat(t, true)
	body := `{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + codexIdentityOriginal + ` You are Claude Code, Anthropic's official CLI tool for Claude."},` +
		`{"role":"user","content":"hello"}]}`
	out := mustPrepareBody(t, []byte(body), true)
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	messages := doc["messages"].([]any)
	content := messages[0].(map[string]any)["content"].(string)
	if !strings.Contains(content, codexIdentityRewritten) {
		t.Fatalf("codex rewrite missing when sanitize enabled: %q", content)
	}
}

// TestCodexCompatRewrittenSentenceNotRewrittenAgain：已带连字符的句子
// （改写目标本身）不会再次命中改写——预检片段用的是未连字符形式。
func TestCodexCompatRewrittenSentenceNotRewrittenAgain(t *testing.T) {
	withCodexCompat(t, true)
	body := `{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + codexIdentityRewritten + `"},` +
		`{"role":"user","content":"hello"}]}`
	out := mustPrepareBody(t, []byte(body), false)
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	messages := doc["messages"].([]any)
	content := messages[0].(map[string]any)["content"].(string)
	if content != codexIdentityRewritten {
		t.Fatalf("already-rewritten sentence changed: %q", content)
	}
}
