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

	h5 "workbuddy2api/internal/haozhumah5"
)

func postWebToken(h http.Handler, token string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"token": token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/haozhuma/h5-session", bytes.NewReader(body)))
	return rec
}

func TestHaozhumaWebTokenPersistentLifecycle(t *testing.T) {
	newFakeH5Server(t, "fixture-good-token")
	ah, _, configPath := newWebuiTestHandler(t, `{}`)
	path := filepath.Join(t.TempDir(), "web-token.enc")
	c, err := h5.NewPersistent("", path, "fixture-key")
	if err != nil {
		t.Fatal(err)
	}
	ah.Handler.SetHaozhumaH5(c)
	rec := postWebToken(ah, "fixture-good-token")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"persisted":true`) || !c.Info().Persisted {
		t.Fatalf("save failed: HTTP %d %s", rec.Code, rec.Body)
	}
	before, _ := os.ReadFile(path)
	config, _ := os.ReadFile(configPath)
	if bytes.Contains(before, []byte("fixture-good-token")) || bytes.Contains(config, []byte("fixture-good-token")) {
		t.Fatal("Web token was written as plaintext")
	}
	if bad := postWebToken(ah, "fixture-rejected-token"); bad.Code != 400 {
		t.Fatalf("bad token should be rejected: %d", bad.Code)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) || !c.Info().Has || !c.Info().Persisted {
		t.Fatal("failed verification replaced the saved token")
	}
	restored, err := h5.NewPersistent("", path, "fixture-key")
	if err != nil {
		t.Fatal(err)
	}
	ah.Handler.SetHaozhumaH5(restored)
	projects := httptest.NewRecorder()
	ah.ServeHTTP(projects, httptest.NewRequest("GET", "/admin/account/sms/haozhuma/projects?q=fixture", nil))
	if projects.Code != 200 {
		t.Fatalf("restored token not usable: HTTP %d", projects.Code)
	}
	echo := httptest.NewRecorder()
	ah.ServeHTTP(echo, httptest.NewRequest("GET", "/admin/config", nil))
	if echo.Code != 200 || strings.Contains(echo.Body.String(), "fixture-good-token") || !strings.Contains(echo.Body.String(), `"storage":"encrypted_file"`) {
		t.Fatal("persistence status missing or credential leaked in config echo")
	}
	if clear := postWebToken(ah, ""); clear.Code != 200 {
		t.Fatalf("clear failed: %s", clear.Body)
	}
	cleared, err := h5.NewPersistent("fixture-environment-token", path, "fixture-key")
	if err != nil || cleared.Info().Has {
		t.Fatal("cleared Web token reappeared on restart")
	}
}

func TestHaozhumaWebTokenSaveFailureKeepsRuntime(t *testing.T) {
	newFakeH5Server(t, "")
	ah, _, _ := newWebuiTestHandler(t, `{}`)
	// A directory cannot be replaced by the encrypted credential file.
	c, err := h5.NewPersistent("", t.TempDir(), "fixture-key")
	if err == nil {
		t.Fatal("a directory is not a valid credential store")
	}
	c.Session("fixture-old-token")
	ah.Handler.SetHaozhumaH5(c)
	for _, token := range []string{"fixture-new-token", ""} {
		rec := postWebToken(ah, token)
		if rec.Code != 500 || strings.Contains(rec.Body.String(), "fixture-") || !c.Info().Has || c.Info().StorageError == "" {
			t.Fatalf("failed save must preserve the active token: HTTP %d", rec.Code)
		}
	}
	if err := c.Ping(t.Context()); err != nil {
		t.Fatal("old active token no longer usable after failed save")
	}
}
