package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// testAPIKey is the credential the test helpers install. It only ever exists
// inside the test binary.
const testAPIKey = "test-key"

// authedHandler is a Handler that can attach the test credential to requests.
//
// Routes are driven through ServeHTTP, and the middleware is now
// deny-by-default: a Handler with no credential configured refuses everything.
// Rather than leave most of the suite asserting 401, the wrapper injects the
// test credential for handlers that were built without an explicit one.
//
// Embedding *Handler keeps field and method access working (`h.cfg`,
// `h.persistAccount`, `h.unlock`), so call sites need no other change.
//
// Handlers built WITH an explicit credential use inject=false, so the tests
// that assert 401/403 keep exercising the real unauthenticated path.
type authedHandler struct {
	*Handler
	inject bool
}

func (a authedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if a.inject {
		r.Header.Set("Authorization", "Bearer "+testAPIKey)
	}
	a.Handler.ServeHTTP(w, r)
}

// newTestHandler builds a handler for tests that just want a working route.
//
// When the caller configures no credential, a test key is installed and the
// returned wrapper attaches it to every request. When the caller does configure
// a credential, the underlying handler is built as-is and returned with
// inject=false — those are the tests deliberately exercising authentication.
func newTestHandler(t *testing.T, cfg Config) authedHandler {
	t.Helper()
	inject := false
	if cfg.APIKey == "" && cfg.FrontendPassword == "" {
		cfg.APIKey = testAPIKey
		inject = true
	}
	return authedHandler{Handler: NewHandler(cfg), inject: inject}
}

// TestRoutesDenyWhenNoCredentialConfigured is the regression test for the
// fail-open defect: `APIKey != "" && ...` meant that leaving the credential
// unset disarmed authentication on every route instead of enforcing it.
//
// Deliberately excluded, each for a documented reason:
//   - GET  /healthz      — unauthenticated by design, for load balancers.
//   - POST /admin/unlock — mints the console cookie; requiring it would deadlock.
//   - GET  /             — the console bundle must load before unlock.
func TestRoutesDenyWhenNoCredentialConfigured(t *testing.T) {
	// Built raw on purpose: this test is about the unconfigured handler.
	h := NewHandler(Config{})

	cases := []struct{ method, path string }{
		{http.MethodGet, "/status"},
		{http.MethodGet, "/stats"},
		{http.MethodGet, "/requests"},
		{http.MethodGet, "/v1/requests"},
		{http.MethodGet, "/v1/models"},
		{http.MethodGet, "/models"},
		{http.MethodPost, "/v1/chat/completions"},
		{http.MethodPost, "/chat/completions"},
		{http.MethodPost, "/v1/responses"},
		{http.MethodPost, "/responses"},
		{http.MethodGet, "/admin/config"},
		{http.MethodPost, "/admin/config"},
		{http.MethodPost, "/admin/checkin"},
		{http.MethodPost, "/admin/credits/refresh"},
		{http.MethodPost, "/admin/account/url"},
		{http.MethodPost, "/admin/account/poll"},
		{http.MethodPost, "/admin/account/sms/send"},
		{http.MethodPost, "/admin/account/sms/verify"},
		{http.MethodPost, "/admin/account/sms/captcha"},
		// Spends real money on SMS numbers: unauthenticated it could drain a
		// paid balance.
		{http.MethodPost, "/admin/account/sms/auto-enroll"},
		{http.MethodGet, "/admin/account/sms/auto-enroll"},
		{http.MethodPost, "/admin/account/sms/auto-enroll/stop"},
		{http.MethodGet, "/admin/proxy/status"},
		{http.MethodPost, "/admin/account/u1/enable"},
		{http.MethodPost, "/admin/account/u1/disable"},
		{http.MethodPost, "/admin/account/u1/clear-cooldown"},
		{http.MethodPost, "/admin/account/u1/checkin"},
		{http.MethodPost, "/admin/account/u1/keepalive"},
		{http.MethodDelete, "/admin/account/u1"},
	}

	for _, tc := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401 (fail-open regression)", tc.method, tc.path, rec.Code)
		}
	}
}

// TestHealthzStaysPublicWithoutCredential pins the one intentional exception:
// a probe must answer even when no credential is configured.
func TestHealthzStaysPublicWithoutCredential(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code == http.StatusUnauthorized {
		t.Error("/healthz must stay reachable for load balancers")
	}
}

// TestConfiguredCredentialRequiresMatch confirms the gate still accepts the
// right credential and rejects the wrong one.
func TestConfiguredCredentialRequiresMatch(t *testing.T) {
	h := newTestHandler(t, Config{Pool: testPoolWith()})

	rec := httptest.NewRecorder()
	h.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stats", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no credential: code=%d want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stats", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	h.Handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Errorf("valid credential rejected: code=%d", rec.Code)
	}
}
