package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestRealFrontendDirServesConsole exercises the allow-list against the actual
// checked-in frontend/ directory, so a Vite rebuild that changes the asset hash
// cannot silently break the console.
func TestRealFrontendDirServesConsole(t *testing.T) {
	h := newTestHandler(t, Config{FrontendDir: "../../frontend"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200 (console root not served)", rec.Code)
	}
	html := rec.Body.String()
	if !strings.Contains(html, "<div id=\"root\">") {
		t.Fatalf("index.html does not look like the console shell: %q", html)
	}

	// Every asset the shell references must be servable.
	for _, m := range regexp.MustCompile(`/assets/[^"' ]+`).FindAllString(html, -1) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, m, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("shell references %s but GET %s = %d", m, m, rec.Code)
		}
	}

	// The legacy v1 console must be gone (deleted, and never in the allow-list).
	for _, p := range []string{"/app.js", "/admin.css", "/styles.css"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("legacy asset GET %s = %d, want 404", p, rec.Code)
		}
	}
}

// TestRealFrontendBundleDoesNotPersistAPIKey guards HIGH-006 at the artifact
// level: the shipped bundle must not contain a Web Storage write for the key.
func TestRealFrontendBundleDoesNotPersistAPIKey(t *testing.T) {
	matches, err := filepath.Glob("../../frontend/assets/index-*.js")
	if err != nil || len(matches) == 0 {
		t.Skip("no built console bundle checked in")
	}
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		bundle := string(raw)
		for _, bad := range []string{
			`sessionStorage.setItem("wb2api-api-key"`,
			`localStorage.setItem("wb2api-api-key"`,
			"sessionStorage.setItem('wb2api-api-key'",
			"localStorage.setItem('wb2api-api-key'",
		} {
			if strings.Contains(bundle, bad) {
				t.Errorf("%s still persists the API key in Web Storage (%s)", path, bad)
			}
		}
	}
}
