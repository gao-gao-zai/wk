package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

func withDSHCompat(t *testing.T, enabled bool) {
	t.Helper()
	old := dshCompatEnabled.Load()
	SetDSHCompat(enabled)
	t.Cleanup(func() { SetDSHCompat(old) })
}

func TestDSHCompatTextBoundariesAndIdempotency(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{dshIdentityOriginal + ".", dshIdentityRewritten + "."},
		{dshIdentityOriginal, dshIdentityRewritten},
		{"Before.\n" + dshIdentityOriginal + ".\nAfter.", "Before.\n" + dshIdentityRewritten + ".\nAfter."},
		{dshIdentityOriginal + "\n" + dshIdentityOriginal + ".", dshIdentityRewritten + "\n" + dshIdentityRewritten + "."},
		{dshIdentityOriginal + "es. " + dshIdentityOriginal + ".", dshIdentityOriginal + "es. " + dshIdentityRewritten + "."},
		{"DeepSeek Harness", "DeepSeek Harness"},
		{"You are an AI agent powered by DeepSeek Harnes.", "You are an AI agent powered by DeepSeek Harnes."},
		{dshIdentityRewritten + ".", dshIdentityRewritten + "."},
		{dshGUIOriginal, dshGUIRewritten},
		{"Before.\r\n" + dshGUIOriginal + "\r\nAfter.", "Before.\r\n" + dshGUIRewritten + "\r\nAfter."},
		{dshGUIOriginal + "\n" + dshGUIOriginal, dshGUIRewritten + "\n" + dshGUIRewritten},
		{dshIdentityOriginal + ".\n" + dshGUIOriginal, dshIdentityRewritten + ".\n" + dshGUIRewritten},
		{dshGUIOriginal[:len(dshGUIOriginal)-1], dshGUIOriginal[:len(dshGUIOriginal)-1]},
		{dshGUIRewritten, dshGUIRewritten},
		{dshPathsOriginal, dshPathsRewritten},
		{"Before.\r\n" + dshPathsOriginal + "\r\nAfter.", "Before.\r\n" + dshPathsRewritten + "\r\nAfter."},
		{dshPathsOriginal + "\n" + dshPathsOriginal, dshPathsRewritten + "\n" + dshPathsRewritten},
		{dshPathsOriginal[:len(dshPathsOriginal)-1], dshPathsOriginal[:len(dshPathsOriginal)-1]},
		{dshPathsRewritten, dshPathsRewritten},
	} {
		got := rewriteDSHPromptText(tc.input)
		if got != tc.want {
			t.Fatalf("rewrite %q: got %q want %q", tc.input, got, tc.want)
		}
		if again := rewriteDSHPromptText(got); again != got {
			t.Fatalf("rewrite not idempotent: %q", again)
		}
	}
}

func TestDSHCompatPayloadToggleAndRoles(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			withDSHCompat(t, enabled)
			var messages []any
			for _, role := range []string{"system", "developer", "user", "assistant", "tool"} {
				messages = append(messages, map[string]any{"role": role, "content": dshIdentityOriginal + ".\n" + dshGUIOriginal + "\n" + dshPathsOriginal})
			}
			raw, _ := json.Marshal(map[string]any{"model": "glm-5.3-flash", "messages": messages})
			out := mustPrepareBody(t, raw, false)
			var doc map[string]any
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatal(err)
			}
			for i, item := range doc["messages"].([]any) {
				m := item.(map[string]any)
				want := dshIdentityOriginal + ".\n" + dshGUIOriginal + "\n" + dshPathsOriginal
				if enabled && i < 2 {
					want = dshIdentityRewritten + ".\n" + dshGUIRewritten + "\n" + dshPathsRewritten
				}
				if m["content"] != want {
					t.Fatalf("message %d changed incorrectly: %v", i, m)
				}
			}
		})
	}
}

func TestDSHCompatMultimodalKeepsImages(t *testing.T) {
	withDSHCompat(t, true)
	messages := []any{map[string]any{"role": "system", "content": []any{
		map[string]any{"type": "text", "text": dshIdentityOriginal + "."},
		map[string]any{"type": "input_text", "text": dshGUIOriginal},
		map[string]any{"type": "text", "text": dshPathsOriginal + " A trailing slash marks a directory: list it when its contents matter."},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA=="}, "text": dshIdentityOriginal + ".\n" + dshGUIOriginal},
	}}}
	if !rewriteDSHPromptMessages(messages) {
		t.Fatal("text was not rewritten")
	}
	parts := messages[0].(map[string]any)["content"].([]any)
	if parts[0].(map[string]any)["text"] != dshIdentityRewritten+"." {
		t.Fatal("text part not rewritten")
	}
	if parts[1].(map[string]any)["text"] != dshGUIRewritten {
		t.Fatal("GUI text part not rewritten")
	}
	if parts[2].(map[string]any)["text"] != dshPathsRewritten+" A trailing slash marks a directory: list it when its contents matter." {
		t.Fatal("path instructions or surrounding text changed incorrectly")
	}
	image := parts[3].(map[string]any)
	if image["image_url"].(map[string]any)["url"] != "data:image/png;base64,AA==" || image["text"] != dshIdentityOriginal+".\n"+dshGUIOriginal {
		t.Fatal("non-text part changed")
	}
}

func TestDSHCompatCoexistsWithCodexAndSanitize(t *testing.T) {
	withDSHCompat(t, true)
	withCodexCompat(t, true)
	raw, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "system", "content": dshIdentityOriginal + ".\n" + dshGUIOriginal + "\n" + dshPathsOriginal + "\n" + codexIdentityOriginal + "\n" + ccIdentity}}})
	out := mustPrepareBody(t, raw, true)
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	content := doc["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(content, dshIdentityRewritten+".") || !strings.Contains(content, dshGUIRewritten) || !strings.Contains(content, dshPathsRewritten) || !strings.Contains(content, codexIdentityRewritten) {
		t.Fatal("independent compatibility rewrites interfered")
	}
	if strings.Contains(content, ccIdentity) || !strings.Contains(content, "You are Claude Code, Anthropic's official CLI tool for Claude.") {
		t.Fatal("fingerprint sanitation did not run")
	}
}
