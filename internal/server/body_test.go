package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestChatRejectsNullBody is the end-to-end form of a pre-existing DoS.
//
// json.Unmarshal([]byte("null"), &map) succeeds with a nil map, so a 4-byte
// `null` body used to reach upstream.PrepareBodyOptWithEfforts, whose very next
// statement (`obj["stream"] = true`) panics with "assignment to entry in nil
// map". A panic in a handler goroutine takes down the process, so this was a
// one-request outage. The handler must now answer 400.
func TestChatRejectsNullBody(t *testing.T) {
	h := newTestHandler(t, Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
	})
	for _, body := range []string{"null", "[1,2,3]", `"str"`, "42", "{broken"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST body %q = %d, want 400 (body=%s)", body, rec.Code, rec.Body)
		}
	}
}

// TestChatStillAcceptsNormalBody guards against over-tightening the new check.
func TestChatStillAcceptsNormalBody(t *testing.T) {
	h := newTestHandler(t, Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("normal request = %d, want 200 (body=%s)", rec.Code, rec.Body)
	}
}
