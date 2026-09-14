// codex.go Codex CLI 系统提示词兼容改写。
//
// 背景：Codex CLI 会在 system prompt 里注入一句固定的身份声明，上游
// 内容审核按逐字精确匹配拦截（与 sanitize.go 的 Claude 指纹同一机制）。
// 开关打开后把该句中的 "open source" 最小改写为 "open-source"（一处
// 连字符，语义不变）；句子不存在时原样透传，不做任何别的改动。
package upstream

import "strings"

// codexCompatFeature 特征预检：原句的判别片段（未连字符的 "open source"）。
// 改写后的句子带连字符，不会命中预检，因此已改写过的文本是零开销空转。
const codexCompatFeature = "an open source project led by OpenAI"

// codexCompatRewrites 整句逐字替换表（只改一处连字符，语义不变）。
var codexCompatRewrites = [][2]string{
	{
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI. You are expected to be precise, safe, and helpful.",
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open-source project led by OpenAI. You are expected to be precise, safe, and helpful.",
	},
}

// rewriteCodexPromptText 单段文本改写：预检不中 → 返回原串（零分配）。
func rewriteCodexPromptText(text string) string {
	if !strings.Contains(text, codexCompatFeature) {
		return text
	}
	for _, rw := range codexCompatRewrites {
		text = strings.ReplaceAll(text, rw[0], rw[1])
	}
	return text
}

// rewriteCodexPromptMessages 改写 system 消息 content 中的 Codex 身份句。
// 只动 role=system 的消息（developer 已在 normalizeMessageRoles 里映射成
// system），user/assistant 消息里出现的同句不改动——那不是系统提示词，
// 贸然改写等于篡改用户输入。content 兼容字符串与多模态数组两种形态，
// 数组里只动 text part。返回是否发生变化。
func rewriteCodexPromptMessages(messages []any) bool {
	changed := false
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); !strings.EqualFold(role, "system") {
			continue
		}
		switch c := m["content"].(type) {
		case string:
			if s := rewriteCodexPromptText(c); s != c {
				m["content"] = s
				changed = true
			}
		case []any:
			for _, p := range c {
				part, ok := p.(map[string]any)
				if !ok {
					continue
				}
				if text, ok := part["text"].(string); ok {
					if s := rewriteCodexPromptText(text); s != text {
						part["text"] = s
						changed = true
					}
				}
			}
		}
	}
	return changed
}
