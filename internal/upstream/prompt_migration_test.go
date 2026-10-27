package upstream

import (
	"encoding/json"
	"strings"
	"testing"

	"workbuddy2api/internal/promptguard"
)

func TestMigratedRulesMatchLegacyPipelineAndOwnEnableState(t *testing.T) {
	SetCodexCompat(true)
	SetDSHCompat(true)
	t.Cleanup(func() { SetCodexCompat(false); SetDSHCompat(false) })
	m, err := promptguard.Open("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	_, err = m.MigrateLegacy(promptguard.LegacyConfig{Codex: true, DSH: true})
	if err != nil {
		t.Fatal(err)
	}
	c := New()
	c.SanitizeFingerprints = false
	c.RewriteSystem = m.Apply
	c.LegacyPromptRulesManaged = true
	original := codexCompatRewrites[0][0] + "\n" + dshGUIOriginal + "\n" + dshPathsOriginal + "\n" + dshIdentityOriginal + ".\n" + dshIdentityOriginal + "es"
	for _, content := range []any{original, []any{map[string]any{"type": "text", "text": original}}} {
		raw, _ := json.Marshal(map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "developer", "content": content}, map[string]any{"role": "user", "content": original}}})
		legacy, e := PrepareBodyOpt(raw, false)
		if e != nil {
			t.Fatal(e)
		}
		managed, e := c.prepareBody(raw)
		if e != nil || string(legacy) != string(managed) {
			t.Fatalf("migration changed output: %v\nlegacy=%s\nmanaged=%s", e, legacy, managed)
		}
	}
	cfg := m.Config()
	for i := range cfg.Rules {
		cfg.Rules[i].Enabled = false
	}
	if err = m.Update(cfg); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "system", "content": original}}})
	managed, err := c.prepareBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	_ = json.Unmarshal(managed, &doc)
	if promptguard.Text(promptguard.Extract(doc)) != original {
		t.Fatal("old compatibility flags bypassed disabled managed rules")
	}
	cfg = m.Config()
	cfg.Rules = nil
	_ = m.Update(cfg)
	managed, err = c.prepareBody(raw)
	if err != nil || strings.Contains(string(managed), "open-source") {
		t.Fatal("old compatibility fallback resurrected deleted rules")
	}
}
