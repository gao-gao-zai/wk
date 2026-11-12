package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEphemeralRequestScopeNeverPersists(t *testing.T) {
	a := &Auth{UID: "scope-fixture", AccessToken: "fake-at", RefreshToken: "fake-rt", FilePath: filepath.Join(t.TempDir(), "auth.json")}
	before := a.Snapshot()
	marker := map[string]any{"ephemeral_scope_marker": "never-persist-this", "unmarshalable": func() {}}
	a.SetEphemeralRequestScope(marker)
	if a.EphemeralRequestScope() == nil {
		t.Fatal("request scope not attached")
	}
	if a.Snapshot() != before {
		t.Fatal("request scope entered Credentials snapshot")
	}
	for _, value := range []any{a, a.Snapshot(), a.DeviceProfile} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal("ephemeral state entered JSON serialization")
		}
		if strings.Contains(string(raw), "ephemeral_scope_marker") || strings.Contains(string(raw), "never-persist-this") {
			t.Fatal("scope entered serialized auth/device data")
		}
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "ephemeral_scope_marker") || strings.Contains(string(raw), "never-persist-this") {
		t.Fatal("scope entered credential file")
	}
	restored, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if restored.EphemeralRequestScope() != nil {
		t.Fatal("temporary scope restored from disk")
	}
}
