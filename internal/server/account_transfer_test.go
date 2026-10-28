package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/groups"
	"workbuddy2api/internal/pool"
)

const transferTestPassword = "test-only-long-transfer-password"

func transferRequest(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", path, bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer admin-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func testTransferDoc() accountTransferDocument {
	return accountTransferDocument{Accounts: []transferredAccount{{Credential: json.RawMessage(`{"uid":"u1","accessToken":"fake-at","refreshToken":"fake-rt","nickname":"Test account","domain":"www.workbuddy.ai","expiresAt":9999999999}`), Groups: []string{"vip"}}}}
}
func testTransferHandler(t *testing.T) (*Handler, string) {
	t.Helper()
	dir := t.TempDir()
	gs, err := groups.New(filepath.Join(dir, "groups"))
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{APIKey: "admin-key", Pool: pool.New(filepath.Join(dir, "state.json")), AuthDir: filepath.Join(dir, "auths"), Groups: gs, Region: "cn"})
	return h, dir
}
func TestAccountTransferEncryptedRoundtripAndTamper(t *testing.T) {
	doc := testTransferDoc()
	b, err := sealAccounts(doc, transferTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(b)
	if bytes.Contains(encoded, []byte("fake-at")) || bytes.Contains(encoded, []byte("Test account")) {
		t.Fatal("plaintext leaked")
	}
	opened, err := openAccounts(b, transferTestPassword)
	if err != nil || len(opened.Accounts) != 1 {
		t.Fatalf("open: %v", err)
	}
	if _, err := openAccounts(b, "wrong-password-long"); err == nil {
		t.Fatal("wrong password accepted")
	}
	b.Ciphertext[0] ^= 1
	if _, err := openAccounts(b, transferTestPassword); err == nil {
		t.Fatal("tampering accepted")
	}
	b.Version = 2
	if _, err := openAccounts(b, transferTestPassword); err == nil {
		t.Fatal("unknown version accepted")
	}
}
func TestAccountTransferRejectsInvalidContents(t *testing.T) {
	for _, uid := range []string{"../escape", "..", "a/b", "a\\b", "", strings.Repeat("x", 201)} {
		doc := testTransferDoc()
		raw, _ := json.Marshal(map[string]string{"uid": uid, "accessToken": "fake"})
		doc.Accounts[0].Credential = raw
		b, _ := sealAccounts(doc, transferTestPassword)
		if _, err := openAccounts(b, transferTestPassword); err == nil {
			t.Fatalf("accepted invalid UID %q", uid)
		}
	}
	doc := testTransferDoc()
	doc.Accounts = append(doc.Accounts, doc.Accounts[0])
	b, _ := sealAccounts(doc, transferTestPassword)
	if _, err := openAccounts(b, transferTestPassword); err == nil {
		t.Fatal("duplicate UID accepted")
	}
	if _, err := transferUIDs(make([]string, 1001)); err == nil {
		t.Fatal("oversize selection accepted")
	}
}
func TestAccountImportPreviewApplyAndNoOverwrite(t *testing.T) {
	h, dir := testTransferHandler(t)
	b, _ := sealAccounts(testTransferDoc(), transferTestPassword)
	req := accountImportRequest{Bundle: b, Password: transferTestPassword, UIDs: []string{"u1"}}
	preview := transferRequest(t, h, "/admin/accounts/import/preview", req)
	if preview.Code != 200 || !strings.Contains(preview.Body.String(), "Test account") || strings.Contains(preview.Body.String(), "fake-at") {
		t.Fatalf("preview: %d %s", preview.Code, preview.Body.String())
	}
	if h.cfg.Pool.AuthByUID("u1") != nil {
		t.Fatal("preview mutated pool")
	}
	applied := transferRequest(t, h, "/admin/accounts/import", req)
	if applied.Code != 200 {
		t.Fatalf("apply %s", applied.Body.String())
	}
	var result struct {
		Success int `json:"success"`
		Skipped int `json:"skipped"`
	}
	json.Unmarshal(applied.Body.Bytes(), &result)
	if result.Success != 1 {
		t.Fatal(applied.Body.String())
	}
	st, ok := h.cfg.Pool.Status("u1")
	if !ok || !st.Disabled {
		t.Fatal("imported account must start disabled")
	}
	if h.cfg.Pool.Pick() != nil {
		t.Fatal("imported account was routable")
	}
	file := filepath.Join(dir, "auths", "workbuddy-u1.json")
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(file)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode %v", info.Mode())
	}
	again := transferRequest(t, h, "/admin/accounts/import", req)
	json.Unmarshal(again.Body.Bytes(), &result)
	if result.Skipped != 1 {
		t.Fatal(again.Body.String())
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(before, after) {
		t.Fatal("existing credential overwritten")
	}
	loaded, _ := auth.LoadDir(h.cfg.AuthDir, "all")
	if len(loaded) != 1 {
		t.Fatal("credential not reloadable")
	}
	fresh := pool.New("")
	fresh.SyncToDir(loaded)
	if fresh.Pick() != nil {
		t.Fatal("import could activate after crash or lost state")
	}
	reloaded := pool.New(filepath.Join(dir, "state.json"))
	reloaded.SyncToDir(loaded)
	st, ok = reloaded.Status("u1")
	if !ok || !st.Disabled {
		t.Fatal("disabled state did not survive reload")
	}
	h.cfg.Pool.Enable("u1")
	h.cfg.Pool.Flush()
	enabled := pool.New(filepath.Join(dir, "state.json"))
	enabled.SyncToDir(loaded)
	if enabled.Pick() == nil {
		t.Fatal("persisted manual enable must survive restart")
	}
	export := transferRequest(t, h, "/admin/accounts/export", map[string]any{"uids": []string{"u1"}, "password": transferTestPassword})
	if export.Code != 200 || export.Header().Get("Cache-Control") != "no-store" || strings.Contains(export.Body.String(), "fake-at") {
		t.Fatal("export failed or leaked")
	}
	var roundtrip accountTransferBundle
	json.Unmarshal(export.Body.Bytes(), &roundtrip)
	if _, err := openAccounts(roundtrip, transferTestPassword); err != nil {
		t.Fatal(err)
	}
}
func TestAccountImportDoesNotOverwriteOrphanFile(t *testing.T) {
	h, dir := testTransferHandler(t)
	os.MkdirAll(h.cfg.AuthDir, 0700)
	path := filepath.Join(dir, "auths", "workbuddy-u1.json")
	os.WriteFile(path, []byte("keep-existing"), 0600)
	b, _ := sealAccounts(testTransferDoc(), transferTestPassword)
	w := transferRequest(t, h, "/admin/accounts/import", accountImportRequest{Bundle: b, Password: transferTestPassword, UIDs: []string{"u1"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"skipped":1`) {
		t.Fatal(w.Body.String())
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "keep-existing" || h.cfg.Pool.AuthByUID("u1") != nil {
		t.Fatal("orphan file overwritten")
	}
}
func TestAccountTransferAdminOnlyAndLimits(t *testing.T) {
	h, groupKey, _ := newGroupTestHandler(t)
	for _, path := range []string{"/admin/accounts/export", "/admin/accounts/import/preview", "/admin/accounts/import"} {
		for _, key := range []string{"", groupKey} {
			r := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
			r.Header.Set("Authorization", "Bearer "+key)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 401 {
				t.Fatalf("%s accepted nonadmin %d", path, w.Code)
			}
		}
	}
	w := transferRequest(t, h, "/admin/accounts/export", map[string]any{"uids": []string{"u1"}, "password": "short"})
	if w.Code != 400 {
		t.Fatal("short password accepted")
	}
	w = transferRequest(t, h, "/admin/accounts/export", map[string]any{"uids": []string{"missing"}, "password": transferTestPassword})
	if w.Code != 409 {
		t.Fatal("missing source accepted")
	}
	h.accountTransferMu.Lock()
	w = transferRequest(t, h, "/admin/accounts/export", map[string]any{"uids": []string{"u1"}, "password": transferTestPassword})
	h.accountTransferMu.Unlock()
	if w.Code != 429 {
		t.Fatal("concurrent derivation not bounded")
	}
}
func TestAccountImportRegionAndSelectionValidation(t *testing.T) {
	h, _ := testTransferHandler(t)
	h.cfg.Groups.SetGroupRegion("default", "cn")
	b, _ := sealAccounts(testTransferDoc(), transferTestPassword)
	req := accountImportRequest{Bundle: b, Password: transferTestPassword, UIDs: []string{"not-in-bundle"}}
	if w := transferRequest(t, h, "/admin/accounts/import", req); w.Code != 400 {
		t.Fatal("accepted foreign selection")
	}
	req.UIDs = []string{"u1"}
	w := transferRequest(t, h, "/admin/accounts/import/preview", req)
	if !strings.Contains(w.Body.String(), "无兼容目标分组") {
		t.Fatal(w.Body.String())
	}
	w = transferRequest(t, h, "/admin/accounts/import", req)
	if h.cfg.Pool.AuthByUID("u1") != nil {
		t.Fatal("incompatible group import accepted")
	}
	h.cfg.Groups.CreateWithRegion("vip", "global")
	w = transferRequest(t, h, "/admin/accounts/import", req)
	if !strings.Contains(w.Body.String(), `"success":1`) {
		t.Fatal(w.Body.String())
	}
	if got := h.cfg.Groups.AccountGroups("u1"); len(got) != 1 || got[0] != "vip" {
		t.Fatalf("groups %v", got)
	}
}
