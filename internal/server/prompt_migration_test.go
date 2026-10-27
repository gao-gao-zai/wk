package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/promptguard"
	"workbuddy2api/internal/upstream"
)

func TestMigratedPromptConfigRejectsOldControlsWithoutPartialSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	seed := []byte(`{"features":{"codex_compat":true,"dsh_compat":true,"passthrough":false}}`)
	if err := os.WriteFile(path, seed, 0600); err != nil {
		t.Fatal(err)
	}
	m, _ := promptguard.Open("", "")
	defer m.Close()
	_, _ = m.MigrateLegacy(promptguard.LegacyConfig{Codex: true, DSH: true})
	h := NewHandler(Config{ConfigPath: path, APIKey: "fixture-admin", PromptGuard: m, Upstream: upstream.New()})
	request := func(method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/admin/config", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer fixture-admin")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	get := request("GET", "")
	var echo struct {
		Migrated bool            `json:"legacy_prompt_compat_migrated"`
		Features map[string]bool `json:"features"`
	}
	_ = json.Unmarshal(get.Body.Bytes(), &echo)
	if get.Code != 200 || !echo.Migrated || echo.Features["codex_compat"] || echo.Features["dsh_compat"] || !h.cfg.Upstream.LegacyPromptRulesManaged {
		t.Fatal("old controls still authoritative")
	}
	for _, field := range []string{"codex_compat", "dsh_compat"} {
		w := request("POST", `{"features":{"`+field+`":false,"passthrough":true}}`)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "prompt_compat_migrated") {
			t.Fatal("old control accepted")
		}
		raw, _ := os.ReadFile(path)
		if !bytes.Equal(raw, seed) {
			t.Fatal("rejected request partially changed config")
		}
	}
	if w := request("POST", `{"features":{"passthrough":true}}`); w.Code != 200 {
		t.Fatalf("unrelated feature blocked: %s", w.Body)
	}
}
