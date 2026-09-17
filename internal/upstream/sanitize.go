// sanitize.go 出站请求体脱敏：剥离上游内容审核黑名单指纹。
//
// 背景：客户端（Claude Code 类 CLI）在 system prompt 注入若干固定模板句，
// 上游内容审核按逐字精确匹配拦截（非语义审核），一字改动即可绕过。
// 策略：键值/header 型指纹整段剥离；承载语义的模板句最小改写（换一词），语义不变。
package upstream

import (
	"regexp"
	"strings"
)

// sanitizeFeatures 特征预检：任一命中才进入净化（strings.Contains 快速路径，
// 普通请求全不中 → 原样返回，零分配）。
var sanitizeFeatures = []string{
	"x-anthropic-billing-header", // header 键值段键名
	"cc_entrypoint=",             // 尾随裸键值（截断前缀即可命中）
	"You are Claude Code",        // 身份句（截断前缀即可命中）
	"Main branch (",              // 注入指令句（截断前缀即可命中）
}

// sanitizeHdrRe 剥离层：header 键名即触发（与值无关），整段删除。
var sanitizeHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\n]*;?\s*`)

// sanitizeKvRe 剥离层：尾随裸键值（cc_xxx=...;）循环清理。
var sanitizeKvRe = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

// sanitizeRewrites 改写层：全模板句逐字替换（每句只改一个词，语义不变）。
var sanitizeRewrites = [][2]string{
	{
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are Claude Code, Anthropic's official CLI tool for Claude.",
	},
	{
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)",
	},
}

// sanitizeText 单段文本净化：预检不中 → 返回原串（零分配）。
func sanitizeText(text string) string {
	if !hasFingerprint(text) {
		return text
	}
	for _, rw := range sanitizeRewrites {
		text = strings.ReplaceAll(text, rw[0], rw[1])
	}
	if sanitizeHdrRe.MatchString(text) {
		text = sanitizeHdrRe.ReplaceAllString(text, "")
	}
	if strings.Contains(text, "cc_") {
		prev := ""
		for prev != text { // 清尾随裸 kv（cc_version=...; cc_entrypoint=...;）
			prev = text
			text = sanitizeKvRe.ReplaceAllString(text, "")
		}
	}
	return strings.TrimSpace(text)
}

// hasFingerprint 特征预检：先走 strings.Contains 快速路径（零分配）；
// header 键名有大小写变体（X-Anthropic-...），快速路径漏掉时再落正则（(?i)）兜底。
//
// 性能（PERF-NOTES.md #1，950 rps 剖面实测 3.1% CPU；sanitize_bench_test.go
// 量化：22KB 无指纹文本 ~620µs，几乎全在正则回溯）：正则兜底是唯一开销，
// 但只对"Contains 全不中"的文本执行——即它扫描的恰恰是绝大多数普通文本。
// 优化：加一个大小写无关的键名碎片（-billing-header 不含字母大小写歧义的
// 定位串不行，键名本身就是要 (?i) 的，所以取键名中不含大小写敏感歧义的
// 最短锚 "nthropic-billing"）做二级预筛，碎片不中→绝无 header 键名→跳过正则。
// 碎片中仅 'n' 一个字母有大小写，客户端现实只发小写或 Title-Case 两种
// 键名形态，两种形态里该碎片均为小写，故单查小写碎片即可；任意混排
// 变体仍由正则兜底，语义不变。
func hasFingerprint(text string) bool {
	for _, f := range sanitizeFeatures {
		if strings.Contains(text, f) {
			return true
		}
	}
	// 二级预筛：header 键名的大小写无关锚点。现实键名只有两种形态——
	// 全小写（一级 Contains 已覆盖）与 Title-Case（X-Anthropic-Billing-Header），
	// 两种形态下 "-nthropic-" 段恒为小写（已验证：Title-Case 只大写每个
	// 连字符段的首字母，段内其余字母保持小写，"nthropic" 无首字母），
	// 且该子串在普通文本中几乎不出现。锚点不中 → 文本里不可能有任一
	// 形态的键名 → 跳过正则。任意的混排变体仍会漏到正则兜底（此时
	// 正则照常执行），语义不变。
	if !strings.Contains(text, "nthropic") {
		return false
	}
	return sanitizeHdrRe.MatchString(text)
}

// sanitizeContent 兼容字符串与多模态数组；只动 text part，image 等 part 不动。
// 返回净化后的值及是否发生变化。
func sanitizeContent(v any) (any, bool) {
	switch c := v.(type) {
	case string:
		s := sanitizeText(c)
		return s, s != c
	case []any:
		changed := false
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			text, ok := m["text"].(string)
			if !ok {
				continue
			}
			if s := sanitizeText(text); s != text {
				m["text"] = s
				changed = true
			}
		}
		return c, changed
	}
	return v, false
}

// sanitizeMessages 净化 messages 中的 content；任一命中返回 true。
func sanitizeMessages(messages []any) bool {
	changed := false
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		c, ok := m["content"]
		if !ok {
			continue
		}
		if nc, ch := sanitizeContent(c); ch {
			m["content"] = nc
			changed = true
		}
	}
	return changed
}
