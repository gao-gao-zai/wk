package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// newStaticHandler builds a handler whose console directory is a temp dir we
// fully control, so the assertions are about the allow-list and not about
// whatever happens to be sitting in ./frontend.
//
// Note: ServeMux itself canonicalizes a path with ".." or a doubled slash by
// issuing a 301 to the cleaned URL, before our handler runs. That redirect is
// harmless (the cleaned path then hits the allow-list), so these tests follow
// redirects and assert on the final response rather than the first hop.
func newStaticHandler(t *testing.T, files map[string]string) (authedHandler, string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return newTestHandler(t, Config{FrontendDir: dir}), dir
}

// getFollowingRedirects performs a GET and follows up to a few same-handler
// redirects, so a clean-path 301 (ServeMux) or the "/index.html" -> "./"
// redirect that http.ServeFile performs resolves to the real outcome.
func getFollowingRedirects(t *testing.T, h authedHandler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	var rec *httptest.ResponseRecorder
	for hop := 0; hop < 4; hop++ {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code < 300 || rec.Code >= 400 {
			return rec
		}
		loc := rec.Header().Get("Location")
		if loc == "" {
			return rec
		}
		// Location may be root-relative ("/assets/…") or relative ("./"), and
		// httptest.NewRequest accepts neither as a bare target: resolve it
		// against the current request URL first.
		next, err := req.URL.Parse(loc)
		if err != nil {
			t.Fatalf("unresolvable Location %q: %v", loc, err)
		}
		req = httptest.NewRequest(http.MethodGet, next.String(), nil)
	}
	return rec
}

// TestStaticConsoleServesShellAndAssets is the happy path: the console must
// still load, or the fix has broken the product.
func TestStaticConsoleServesShellAndAssets(t *testing.T) {
	h, _ := newStaticHandler(t, map[string]string{
		"index.html":                `<!doctype html><script type="module" src="/assets/index-Dw5FsLbK.js"></script>`,
		"assets/index-Dw5FsLbK.js":  "console.log(1)",
		"assets/index-B70l1etE.css": "body{}",
	})
	for _, tc := range []struct {
		path string
		code int
	}{
		{"/", 200},
		{"/index.html", 200},
		{"/assets/index-Dw5FsLbK.js", 200},
		{"/assets/index-B70l1etE.css", 200},
	} {
		rec := getFollowingRedirects(t, h, tc.path)
		if rec.Code != tc.code {
			t.Errorf("GET %s = %d, want %d", tc.path, rec.Code, tc.code)
		}
	}
}

// TestStaticConsoleBlocksStrayFiles is the HIGH-001 regression: http.FileServer
// served every regular file under the directory, so anything an operator or a
// backup tool dropped there was world-readable without authentication.
func TestStaticConsoleBlocksStrayFiles(t *testing.T) {
	h, _ := newStaticHandler(t, map[string]string{
		"index.html":             "<!doctype html>",
		"assets/index-abc123.js": "ok",
		// Everything below must be unreachable.
		"config.json":                `{"api_key":"sk-secret"}`,
		"app.js":                     "legacy console with inline key",
		"admin.css":                  "legacy",
		"styles.css":                 "legacy",
		"notes.txt":                  "scratch",
		"backup/auths.json":          `[{"uid":"1234567890"}]`,
		"assets/index-abc123.js.bak": "backup of bundle",
		".env":                       "API_KEY=sk-secret",
	})
	for _, p := range []string{
		"/config.json",
		"/app.js",
		"/admin.css",
		"/styles.css",
		"/notes.txt",
		"/backup/auths.json",
		"/assets/index-abc123.js.bak",
		"/.env",
		"/backup/",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 (stray file exposed)", p, rec.Code)
		}
		if body := rec.Body.String(); body != "" && body != "404 page not found\n" {
			t.Errorf("GET %s leaked a body: %q", p, body)
		}
	}
}

// TestStaticConsoleBlocksDirectoryListing: FileServer generated an HTML index
// for any directory without index.html, disclosing the deployment's contents.
func TestStaticConsoleBlocksDirectoryListing(t *testing.T) {
	h, _ := newStaticHandler(t, map[string]string{
		"index.html":             "<!doctype html>",
		"assets/index-abc123.js": "ok",
	})
	for _, p := range []string{"/assets", "/assets/", "/assets/index-abc123.js/"} {
		rec := getFollowingRedirects(t, h, p)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 (listing/odd path exposed)", p, rec.Code)
		}
	}
}

// TestStaticConsoleBlocksTraversal keeps the allow-list honest against the
// classic escapes. path.Clean plus the asset regexp must reject all of them.
func TestStaticConsoleBlocksTraversal(t *testing.T) {
	h, dir := newStaticHandler(t, map[string]string{"index.html": "<!doctype html>"})
	// A file outside the console directory, reachable only via traversal.
	secret := filepath.Join(filepath.Dir(dir), "outside-secret.txt")
	if err := os.WriteFile(secret, []byte("TOP-SECRET"), 0o644); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(secret) })

	for _, p := range []string{
		"/../outside-secret.txt",
		"/assets/../../outside-secret.txt",
		"/assets/%2e%2e%2f%2e%2e%2foutside-secret.txt",
		"//outside-secret.txt",
		"/./../outside-secret.txt",
	} {
		rec := getFollowingRedirects(t, h, p)
		if rec.Code == http.StatusOK {
			t.Errorf("GET %s = 200 (traversal succeeded)", p)
		}
		if body := rec.Body.String(); body != "" && body != "404 page not found\n" {
			t.Errorf("GET %s leaked a body: %q", p, body)
		}
	}
}

// TestStaticConsoleRejectsWrites: a catch-all route must not accept state-change
// methods on unknown paths.
func TestStaticConsoleRejectsWrites(t *testing.T) {
	h, _ := newStaticHandler(t, map[string]string{"index.html": "<!doctype html>"})
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(m, "/some/unknown/path", nil))
		if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
			t.Errorf("%s /some/unknown/path = %d, want 404/405", m, rec.Code)
		}
	}
}

// TestStaticConsoleMissingIndexIs404 not a crash when the console is absent
// (e.g. a binary-only deployment).
func TestStaticConsoleMissingIndexIs404(t *testing.T) {
	h, _ := newStaticHandler(t, map[string]string{"assets/index-abc123.js": "ok"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET / with no index.html = %d, want 404", rec.Code)
	}
}
