package promptguard

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyMigrationPreservesSwitches(t *testing.T) {
	for _, flags := range []LegacyConfig{{}, {Codex: true}, {DSH: true}, {Codex: true, DSH: true}} {
		m := memory(t)
		cfg := m.Config()
		cfg.Settings.AutoLocate = true
		cfg.Rules = []Rule{{ID: "custom", Name: "已有规则", Match: "custom input", Replacement: "custom output", Enabled: true}}
		if err := m.Update(cfg); err != nil {
			t.Fatal(err)
		}
		before := m.Config()
		changed, err := m.MigrateLegacy(flags)
		if err != nil || !changed {
			t.Fatalf("migration %v: %v", flags, err)
		}
		got := m.Config()
		if got.LegacyMigrationVersion != 1 || len(got.Rules) != 5 || got.Rules[0] != before.Rules[0] || got.Settings != before.Settings {
			t.Fatalf("lost existing configuration: %+v", got)
		}
		for _, r := range got.Rules[1:] {
			want := flags.DSH
			if r.Source == "legacy_codex" {
				want = flags.Codex
			}
			if r.Enabled != want {
				t.Fatalf("%s: enabled %v want %v", r.Name, r.Enabled, want)
			}
		}
		if changed, err = m.MigrateLegacy(LegacyConfig{Codex: !flags.Codex, DSH: !flags.DSH}); err != nil || changed || !reflect.DeepEqual(got, m.Config()) {
			t.Fatal("migration repeated")
		}
	}
}

func TestLegacyMigrationEditsAndDeletesSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guard.enc")
	m, err := Open(path, "fixture-migration-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.MigrateLegacy(LegacyConfig{Codex: true, DSH: true}); err != nil {
		t.Fatal(err)
	}
	cfg := m.Config()
	cfg.Rules = cfg.Rules[:1]
	cfg.Rules[0].Replacement = "administrator replacement"
	cfg.Rules[0].Enabled = false
	cfg.LegacyMigrationVersion = 0
	if err = m.Update(cfg); err != nil {
		t.Fatal(err)
	}
	expected := m.Config()
	if expected.LegacyMigrationVersion != 1 {
		t.Fatal("API reset migration marker")
	}
	m.Close()
	reopened, err := Open(path, "fixture-migration-key")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if changed, e := reopened.MigrateLegacy(LegacyConfig{Codex: true, DSH: true}); e != nil || changed {
		t.Fatal("restart imported presets again")
	}
	if !reflect.DeepEqual(expected, reopened.Config()) {
		t.Fatal("restart overwrote edited, disabled or deleted rules")
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "administrator replacement") {
		t.Fatal("migrated rules were not encrypted")
	}
}

func TestLegacyMigrationFailureIsAtomicAndRetryable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "guard.enc")
	m, err := Open(path, "fixture-migration-key")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	before := m.Config()
	file := filepath.Join(dir, "not-a-directory")
	if err = os.WriteFile(file, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	m.path = filepath.Join(file, "guard.enc")
	if changed, e := m.MigrateLegacy(LegacyConfig{Codex: true, DSH: true}); e == nil || changed || m.LegacyMigrated() || !reflect.DeepEqual(before, m.Config()) {
		t.Fatal("failed migration partially committed")
	}
	m.path = path
	if changed, e := m.MigrateLegacy(LegacyConfig{DSH: true}); e != nil || !changed || len(m.Config().Rules) != 4 {
		t.Fatal("failed migration could not retry")
	}
}

func TestLegacyMigrationReusesExactRulesAndPreservesOrder(t *testing.T) {
	m := memory(t)
	existing := legacyRules(LegacyConfig{Codex: true})[0]
	existing.ID = "custom-codex"
	existing.Name = "管理员已有规则"
	existing.Source = ""
	cfg := m.Config()
	cfg.Rules = []Rule{existing}
	if err := m.Update(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MigrateLegacy(LegacyConfig{Codex: true}); err != nil {
		t.Fatal(err)
	}
	got := m.Config()
	if len(got.Rules) != 4 || got.Rules[0] != existing {
		t.Fatal("duplicate added or existing rule overwritten")
	}
}

func TestMigratedIdentityKeepsOriginalEndBoundary(t *testing.T) {
	m := memory(t)
	_, _ = m.MigrateLegacy(LegacyConfig{DSH: true})
	original := "You are an AI agent powered by DeepSeek Harness"
	replacement := "You are an AI agent running on DeepSeek Harness"
	for _, suffix := range []string{"", ".", " ", "\t", "\r\n", "es", "-extended", "中文"} {
		doc := map[string]any{"messages": []any{map[string]any{"role": "system", "content": original + suffix}}}
		if err := m.Apply(doc); err != nil {
			t.Fatal(err)
		}
		want := original + suffix
		if suffix == "" || strings.ContainsAny(suffix, ". \t\r\n") {
			want = replacement + suffix
		}
		if got := Text(Extract(doc)); got != want {
			t.Fatalf("suffix %q: %q want %q", suffix, got, want)
		}
	}
}
