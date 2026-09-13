package server

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSanitizeLogCellTruncatesByRune is the MEDIUM-005 regression: the old
// `model[:11]` sliced bytes, so a multi-byte model name produced invalid UTF-8
// in the log. Model names come straight from the request body with no allowlist.
func TestSanitizeLogCellTruncatesByRune(t *testing.T) {
	model := strings.Repeat("模", 10) // 30 bytes, 10 runes
	got := sanitizeLogCell(model, 11)
	if !utf8.ValidString(got) {
		t.Errorf("truncation produced invalid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n > 11 {
		t.Errorf("runes=%d, want <= 11", n)
	}
	if got != model {
		t.Errorf("11-rune limit should have kept all 10 runes, got %q", got)
	}
	// The old byte-slice behaviour, kept here as the thing being guarded against.
	if raw := model[:11]; utf8.ValidString(raw) {
		t.Error("precondition failed: byte-slicing no longer corrupts this sample")
	}
}

// TestSanitizeLogCellStripsControlCharacters: \r and ANSI escapes let an attacker
// rewrite or forge log lines, which is an anti-forensics primitive.
func TestSanitizeLogCellStripsControlCharacters(t *testing.T) {
	evil := "glm-5.2\x1b[2K\rFAKE ROW status=200 uid=deadbeef\nINJECT"
	got := sanitizeLogCell(evil, 0)
	for _, bad := range []string{"\r", "\n", "\x1b", "\x00", "\x7f"} {
		if strings.Contains(got, bad) {
			t.Errorf("control character %q survived: %q", bad, got)
		}
	}
	if !utf8.ValidString(got) {
		t.Errorf("result is not valid UTF-8: %q", got)
	}
	// The printable payload is preserved so the log stays useful.
	if !strings.Contains(got, "FAKE ROW") {
		t.Errorf("printable content was dropped unexpectedly: %q", got)
	}
}

// TestSanitizeLogCellHandlesInvalidUTF8 ensures pre-existing bad bytes do not
// propagate into the log stream.
func TestSanitizeLogCellHandlesInvalidUTF8(t *testing.T) {
	got := sanitizeLogCell("ab\xff\xfe"+"cd", 0)
	if !utf8.ValidString(got) {
		t.Errorf("invalid UTF-8 survived: %q", got)
	}
}

// TestSanitizeLogCellZeroLimitMeansNoTruncation documents the contract used for
// fields that are already bounded elsewhere.
func TestSanitizeLogCellZeroLimitMeansNoTruncation(t *testing.T) {
	in := strings.Repeat("a", 500)
	if got := sanitizeLogCell(in, 0); got != in {
		t.Errorf("len=%d, want %d (0 must mean unlimited)", len(got), len(in))
	}
}

// TestSanitizeLogCellEmptyStaysEmpty keeps "-" style placeholders working.
func TestSanitizeLogCellEmptyStaysEmpty(t *testing.T) {
	if got := sanitizeLogCell("", 11); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
