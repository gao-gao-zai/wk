// Package promptguard manages literal system prompt rules and bounded diagnostics.
package promptguard

import (
	"encoding/json"
	"errors"
	"strings"
)

type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// Extract copies only system/developer messages. User, assistant and tool history
// never enters the diagnostic store. Content part boundaries are preserved.
func Extract(doc map[string]any) []Message {
	var out []Message
	msgs, _ := doc["messages"].([]any)
	for _, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if !strings.EqualFold(role, "system") && !strings.EqualFold(role, "developer") {
			continue
		}
		b, err := json.Marshal(msg["content"])
		if err != nil {
			continue
		}
		var content any
		if json.Unmarshal(b, &content) != nil {
			continue
		}
		out = append(out, Message{Role: role, Content: content})
	}
	return out
}

func Clone(messages []Message) []Message {
	b, _ := json.Marshal(messages)
	var out []Message
	_ = json.Unmarshal(b, &out)
	return out
}

func mapTexts(messages []Message, f func(string) string) {
	for i := range messages {
		switch content := messages[i].Content.(type) {
		case string:
			messages[i].Content = f(content)
		case []any:
			for _, raw := range content {
				part, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				typ, _ := part["type"].(string)
				if typ != "text" && typ != "input_text" {
					continue
				}
				if s, ok := part["text"].(string); ok {
					part["text"] = f(s)
				}
			}
		}
	}
}

func Text(messages []Message) string {
	var parts []string
	mapTexts(Clone(messages), func(s string) string { parts = append(parts, s); return s })
	return strings.Join(parts, "\n")
}

// ApplyToDocument runs each rule once in list order; later rules may match an
// earlier replacement. It cannot loop, and does not touch other message roles.
func ApplyToDocument(doc map[string]any, rules []Rule) error {
	if len(rules) == 0 {
		return nil
	}
	const limit = 128 << 20
	total := 0
	var rewriteErr error
	msgs, _ := doc["messages"].([]any)
	for _, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if !strings.EqualFold(role, "system") && !strings.EqualFold(role, "developer") {
			continue
		}
		m := []Message{{Role: role, Content: msg["content"]}}
		mapTexts(m, func(s string) string {
			if rewriteErr != nil {
				return s
			}
			for _, r := range rules {
				if r.Enabled && r.Match != "" {
					count := strings.Count(s, r.Match)
					delta := len(r.Replacement) - len(r.Match)
					if len(s) > limit-total || delta > 0 && count > (limit-total-len(s))/delta {
						rewriteErr = errors.New("system prompt replacement exceeds 128 MiB limit")
						return s
					}
					s = strings.ReplaceAll(s, r.Match, r.Replacement)
				}
			}
			if len(s) > limit-total {
				rewriteErr = errors.New("system prompt replacement exceeds 128 MiB limit")
				return s
			}
			total += len(s)
			return s
		})
		msg["content"] = m[0].Content
	}
	return rewriteErr
}

// IsBlocked requires a structured upstream code, never a substring in a prompt
// echoed by an unrelated error. Account risk (11140) is deliberately excluded.
func IsBlocked(status int, body []byte) bool {
	if status < 400 {
		return false
	}
	var doc struct {
		Code  json.RawMessage `json:"code"`
		Error struct {
			Code json.RawMessage `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return false
	}
	return string(doc.Code) == "11128" || string(doc.Code) == `"11128"` || string(doc.Error.Code) == "11128" || string(doc.Error.Code) == `"11128"`
}
