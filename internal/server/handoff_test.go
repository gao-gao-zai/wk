package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHandoffPreservesHashedSessionAndRejectsWrongKey(t *testing.T) {
	newHandler := func(key string) *Handler {
		return &Handler{cfg: Config{APIKey: key}, sessions: map[string]time.Time{}, unlockAttempts: map[string]unlockAttempt{}}
	}
	h := newHandler("test-upgrade-key")
	digest := sessionDigest("cookie-secret")
	h.sessions[digest] = time.Now().Add(time.Hour)
	p := filepath.Join(t.TempDir(), "handoff.enc")
	if e := h.SaveHandoff(p, []byte(`{}`)); e != nil {
		t.Fatal(e)
	}
	raw, _ := os.ReadFile(p)
	if len(raw) == 0 {
		t.Fatal("empty")
	}
	h2 := newHandler("test-upgrade-key")
	if _, e := h2.LoadHandoff(p); e != nil {
		t.Fatal(e)
	}
	if h2.sessions[digest].IsZero() {
		t.Fatal("session lost")
	}
	if _, e := newHandler("different-key").LoadHandoff(p); e == nil {
		t.Fatal("wrong key accepted")
	}
}
