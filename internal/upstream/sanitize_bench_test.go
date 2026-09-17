package upstream

// sanitize_bench_test.go：脱敏路径的量化基准（PERF-NOTES.md #1 的 before/after）。
// 复现：
//
//	go test ./internal/upstream/ -bench BenchmarkSanitize -benchmem -count=6 | Tee-Object report.txt

import (
	"strings"
	"testing"
)

// benchTexts 三种代表性输入：
//   - clean：普通用户消息（最常见路径，预检全不中）
//   - identity：含 Claude Code 身份句（预检命中）
//   - longClean：长上下文无指纹（最坏情况：Contains×4 全扫 + 正则全文回溯）
func benchTexts() map[string]string {
	return map[string]string{
		"clean":     "please help me refactor this module and add unit tests",
		"identity":  "You are Claude Code, Anthropic's official CLI tool for Claude. Please help.",
		"longClean": strings.Repeat("The quick brown fox jumps over the lazy dog. ", 500), // ~22KB
	}
}

func BenchmarkSanitizeTextClean(b *testing.B) {
	text := benchTexts()["clean"]
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sanitizeText(text)
	}
}

func BenchmarkSanitizeTextIdentity(b *testing.B) {
	text := benchTexts()["identity"]
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sanitizeText(text)
	}
}

func BenchmarkSanitizeTextLongClean(b *testing.B) {
	text := benchTexts()["longClean"]
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sanitizeText(text)
	}
}

func BenchmarkHasFingerprintLongClean(b *testing.B) {
	text := benchTexts()["longClean"]
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hasFingerprint(text)
	}
}
