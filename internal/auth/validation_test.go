package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestParseRejectsControlCharactersInCredentialFields is the HIGH-005 regression.
//
// UID / EnterpriseID / Domain / tokens from the credential JSON flow straight
// into outbound request headers (X-User-Id, X-Enterprise-Id, X-Refresh-Token).
// net/http currently neutralizes embedded CR/LF instead of rejecting it, so the
// header is silently mangled rather than split — a correctness defect today and
// a latent injection if that protection ever moves out of the standard library.
func TestParseRejectsControlCharactersInCredentialFields(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"uid CRLF", `{"accessToken":"at","uid":"u1\r\nX-Injected: 1"}`},
		{"uid LF", `{"accessToken":"at","uid":"u1\nX-Injected: 1"}`},
		{"uid NUL", `{"accessToken":"at","uid":"u1\u0000"}`},
		{"uid tab", `{"accessToken":"at","uid":"u1\tx"}`},
		{"uid space", `{"accessToken":"at","uid":"u1 x"}`},
		{"enterpriseId CRLF", `{"accessToken":"at","enterpriseId":"e1\r\nX-Injected: 1"}`},
		{"domain CRLF", `{"accessToken":"at","domain":"www.codebuddy.cn\r\nHost: evil"}`},
		{"refreshToken CRLF", `{"accessToken":"at","refreshToken":"r1\r\nX-Injected: 1"}`},
		{"nested uid CRLF", `{"auth":{"accessToken":"at"},"account":{"uid":"u1\r\nX-Injected: 1"}}`},
		{"accessToken CRLF", `{"accessToken":"at\r\nX-Injected: 1"}`},
	}
	for _, tc := range cases {
		if _, err := Parse([]byte(tc.body)); err == nil {
			t.Errorf("%s: Parse accepted a credential with illegal characters", tc.name)
		}
	}
}

// TestParseAcceptsRealisticCredentials is the guard against over-tightening: real
// tokens are long base64/JWT-ish strings and must keep loading.
func TestParseAcceptsRealisticCredentials(t *testing.T) {
	for _, body := range []string{
		`{"accessToken":"eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiIxIn0.sig-_A","uid":"64087495","enterpriseId":"ent-1","domain":"www.codebuddy.cn"}`,
		`{"auth":{"accessToken":"at-1","refreshToken":"rt-1","expiresAt":9999999999,"domain":"www.codebuddy.cn"},"account":{"uid":"64087495","enterpriseId":"ent-1","nickname":"64087495"}}`,
		`{"accessToken":"at","uid":"","enterpriseId":"","domain":""}`,
	} {
		if _, err := Parse([]byte(body)); err != nil {
			t.Errorf("realistic credential rejected: %v (body=%s)", err, body)
		}
	}
}

// TestIsSafeCredentialField documents the exact allowed range.
func TestIsSafeCredentialField(t *testing.T) {
	for _, ok := range []string{"abc", "ABC123", "a-b_c.d~e", "eyJhbGciOiJSUzI1NiJ9.eyJ9.x-y_z", "64087495"} {
		if !isSafeCredentialField(ok) {
			t.Errorf("%q should be allowed", ok)
		}
	}
	// Empty is allowed on purpose: uid/enterpriseId/domain are optional, and
	// upstream/headers.go already handles the empty case with X-No-* markers.
	if !isSafeCredentialField("") {
		t.Error(`"" should be allowed (optional fields)`)
	}
	for _, bad := range []string{"a b", "a\tb", "a\nb", "a\rb", "a\x00b", "a\x7fb", "中文", "a\xc3\xa9b"} {
		if isSafeCredentialField(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

// TestSaveAtomicRestrictsPermissions is the MEDIUM-001 regression: os.WriteFile's
// perm argument only applies when the file is created, so a pre-existing .tmp
// with loose permissions carried them onto the credential file via rename.
//
// The permission assertion is POSIX-only: Windows has no Unix mode bits, so
// os.Chmod there can only toggle the read-only attribute and the reported mode
// stays 0666. Production runs on Linux (alpine), which is where it matters.
func TestSaveAtomicRestrictsPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy-cn.json")

	a := &Auth{AccessToken: "at-1", RefreshToken: "rt-1", UID: "u1", Domain: "www.codebuddy.cn", FilePath: path}
	// Pre-create the temp file world-readable, simulating a crash/leftover.
	if err := os.WriteFile(path+".tmp", []byte("{}"), 0o666); err != nil {
		t.Fatalf("seed tmp: %v", err)
	}
	if err := os.Chmod(path+".tmp", 0o666); err != nil {
		t.Fatalf("chmod tmp: %v", err)
	}

	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("credential file mode = %o, want 600 (loose permissions leaked through rename)", perm)
		}
	}
	// The content must still be a valid, reloadable credential file.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := Parse(raw); err != nil {
		t.Errorf("saved file no longer parses: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("saved file is not JSON: %v", err)
	}
}

// TestSaveAtomicLeavesNoTempBehind keeps the atomic-write contract.
func TestSaveAtomicLeavesNoTempBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy-cn.json")
	a := &Auth{AccessToken: "at", UID: "u1", FilePath: path}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file was left behind: %v", err)
	}
}

// TestLoadDirSkipsMalformedCredentialFiles confirms one bad file does not abort
// loading the rest, and that the new validation participates in that skip.
func TestLoadDirSkipsMalformedCredentialFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("workbuddy-good.json", `{"auth":{"accessToken":"at-good","expiresAt":9999999999,"domain":"www.codebuddy.cn"},"account":{"uid":"good"}}`)
	write("workbuddy-inject.json", `{"auth":{"accessToken":"at-bad","domain":"www.codebuddy.cn\r\nHost: evil"},"account":{"uid":"bad"}}`)
	write("workbuddy-garbage.json", `{not json`)

	got, err := LoadDir(dir, "cn")
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("loaded %d accounts, want 1 (only the valid one)", len(got))
	}
	if !strings.Contains(got[0].AccessToken, "at-good") {
		t.Errorf("wrong account loaded: %+v", got[0])
	}
}
