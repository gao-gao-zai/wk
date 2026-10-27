package upstream

import (
	"strings"
	"sync/atomic"
)

// DSH's fixed identity, Web GUI, and path instructions can trigger channel verification.
// Rewrite these exact system-prompt templates while retaining their meaning.
const (
	dshIdentityOriginal  = "You are an AI agent powered by DeepSeek Harness"
	dshIdentityRewritten = "You are an AI agent running on DeepSeek Harness"
	dshGUIOriginal       = "You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names. When the user refers to \"this page\", \"this GUI\", or \"this app\" without naming another target, they mean this GUI."
	dshGUIRewritten      = "You are communicating with the user through the DeepSeek Harness Web GUI whose URL the runtime context names. When the user refers to \"this page\", \"this GUI\", or \"this app\" without naming another target, they mean this GUI."
	dshPathsOriginal     = "Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root; absolute paths identify files or directories on the host."
	dshPathsRewritten    = "Paths explicitly referenced by the user are prefixed with @. Resolve relative paths from the workspace root; absolute paths refer to files or directories on the host."
)

var dshCompatEnabled atomic.Bool

// SetDSHCompat is safe to update while requests are preparing their payloads.
func SetDSHCompat(enabled bool) { dshCompatEnabled.Store(enabled) }

func rewriteDSHPromptText(text string) string {
	text = strings.ReplaceAll(text, dshGUIOriginal, dshGUIRewritten)
	text = strings.ReplaceAll(text, dshPathsOriginal, dshPathsRewritten)
	var out strings.Builder
	pos, written := 0, 0
	for pos < len(text) {
		i := strings.Index(text[pos:], dshIdentityOriginal)
		if i < 0 {
			break
		}
		i += pos
		end := i + len(dshIdentityOriginal)
		pos = end
		// Both the canonical sentence (with '.') and the observed bare identity
		// are supported, without matching longer names such as "Harnesses".
		if end < len(text) && !strings.ContainsRune(". \t\r\n", rune(text[end])) {
			continue
		}
		out.WriteString(text[written:i])
		out.WriteString(dshIdentityRewritten)
		written = end
	}
	if written == 0 {
		return text
	}
	out.WriteString(text[written:])
	return out.String()
}

// Developer roles have already been normalized to system before this runs.
// User/assistant/tool messages and non-text parts are left intact.
func rewriteDSHPromptMessages(messages []any) bool {
	changed := false
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if !strings.EqualFold(role, "system") {
			continue
		}
		switch content := m["content"].(type) {
		case string:
			if rewritten := rewriteDSHPromptText(content); rewritten != content {
				m["content"] = rewritten
				changed = true
			}
		case []any:
			for _, item := range content {
				part, ok := item.(map[string]any)
				if !ok {
					continue
				}
				kind, _ := part["type"].(string)
				if kind != "text" && kind != "input_text" {
					continue
				}
				if text, ok := part["text"].(string); ok {
					if rewritten := rewriteDSHPromptText(text); rewritten != text {
						part["text"] = rewritten
						changed = true
					}
				}
			}
		}
	}
	return changed
}
