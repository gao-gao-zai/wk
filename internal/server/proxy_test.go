package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestClientHostIgnoresSpoofedForwardedForByDefault is the core property: with no
// trusted proxy configured, XFF must not influence the rate-limit identity, or an
// attacker rotates the header per request and the unlock lockout never triggers.
func TestClientHostIgnoresSpoofedForwardedForByDefault(t *testing.T) {
	h := newTestHandler(t, Config{FrontendPassword: "pw"})
	for _, xff := range []string{"1.2.3.4", "9.9.9.9", "203.0.113.7, 10.0.0.1"} {
		req := httptest.NewRequest(http.MethodPost, "/admin/unlock", nil)
		req.RemoteAddr = "192.0.2.10:5555"
		req.Header.Set("X-Forwarded-For", xff)
		req.Header.Set("X-Real-IP", "8.8.8.8")
		if got := h.clientHost(req); got != "192.0.2.10" {
			t.Errorf("XFF=%q: clientHost=%q, want the unspoofable RemoteAddr", xff, got)
		}
	}
}

// TestClientHostHonoursForwardedForBehindTrustedProxy: when the operator has
// explicitly declared the proxy, the real client address must be recovered,
// otherwise every user shares one rate-limit bucket.
func TestClientHostHonoursForwardedForBehindTrustedProxy(t *testing.T) {
	h := newTestHandler(t, Config{FrontendPassword: "pw", TrustedProxies: []string{"10.0.0.1"}})
	req := httptest.NewRequest(http.MethodPost, "/admin/unlock", nil)
	req.RemoteAddr = "10.0.0.1:5555"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := h.clientHost(req); got != "203.0.113.9" {
		t.Errorf("clientHost=%q, want 203.0.113.9", got)
	}
}

// TestClientHostStopsAtFirstUntrustedHop: the rightmost XFF entry is appended by
// our proxy, so a client that prepends its own value must not win.
func TestClientHostStopsAtFirstUntrustedHop(t *testing.T) {
	h := newTestHandler(t, Config{FrontendPassword: "pw", TrustedProxies: []string{"10.0.0.1"}})
	req := httptest.NewRequest(http.MethodPost, "/admin/unlock", nil)
	req.RemoteAddr = "10.0.0.1:5555"
	// Attacker-supplied "1.2.3.4" on the left; proxy-appended real client on the right.
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.9")
	if got := h.clientHost(req); got != "203.0.113.9" {
		t.Errorf("clientHost=%q, want the proxy-appended 203.0.113.9", got)
	}
}

// TestClientHostSupportsCIDRProxy keeps the documented subnet form working.
func TestClientHostSupportsCIDRProxy(t *testing.T) {
	h := newTestHandler(t, Config{FrontendPassword: "pw", TrustedProxies: []string{"10.0.0.0/8"}})
	req := httptest.NewRequest(http.MethodPost, "/admin/unlock", nil)
	req.RemoteAddr = "10.1.2.3:5555"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := h.clientHost(req); got != "203.0.113.9" {
		t.Errorf("clientHost=%q, want 203.0.113.9 through the CIDR proxy", got)
	}
}

// TestUnlockLockoutSurvivesHeaderRotation is the end-to-end version of the first
// test: rotating XFF must not reset the failure counter.
func TestUnlockLockoutSurvivesHeaderRotation(t *testing.T) {
	h := newTestHandler(t, Config{FrontendPassword: "correct-horse"})
	for i := 0; i < unlockFailureLimit; i++ {
		req := httptest.NewRequest(http.MethodPost, "/admin/unlock", strings.NewReader(`{"password":"wrong"}`))
		req.RemoteAddr = "192.0.2.10:5555"
		req.Header.Set("X-Forwarded-For", "203.0.113."+itoaSmall(i))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i, rec.Code)
		}
	}
	// The next attempt — with a *different* spoofed XFF — must be blocked.
	req := httptest.NewRequest(http.MethodPost, "/admin/unlock", strings.NewReader(`{"password":"correct-horse"}`))
	req.RemoteAddr = "192.0.2.10:5555"
	req.Header.Set("X-Forwarded-For", "198.51.100.77")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("correct password after lockout = %d, want 429 (header rotation defeated the lockout)", rec.Code)
	}
}

// TestUnlockGlobalLimiterStopsDistributedGuessing: per-IP lockout alone cannot
// stop a botnet, so the global window must eventually block everyone.
func TestUnlockGlobalLimiterStopsDistributedGuessing(t *testing.T) {
	h := newTestHandler(t, Config{FrontendPassword: "correct-horse"})
	blocked := false
	// Each attempt comes from a distinct IP, so the per-IP limiter never fires.
	for i := 0; i < unlockGlobalFailureLimit+5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/admin/unlock", strings.NewReader(`{"password":"wrong"}`))
		req.RemoteAddr = "198.51.100." + itoaSmall(i%250) + ":1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			blocked = true
			break
		}
	}
	if !blocked {
		t.Fatal("distributed guessing was never globally throttled")
	}
}

// TestRemoteHostParsing covers the shapes RemoteAddr and XFF entries take.
func TestRemoteHostParsing(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"192.0.2.1:5555", "192.0.2.1"},
		{"192.0.2.1", "192.0.2.1"},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"[2001:db8::1]", "2001:db8::1"},
		{"", ""},
		{"  ", ""},
	} {
		if got := remoteHost(tc.in); got != tc.want {
			t.Errorf("remoteHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSecurityHeadersPresent guards the headers added in ServeHTTP. It uses an
// unrouted path so no handler (and therefore no Pool) is needed: the headers are
// set before dispatch and must appear on every response, including 404s.
func TestSecurityHeadersPresent(t *testing.T) {
	h := newTestHandler(t, Config{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/definitely-not-a-route", nil))
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "SAMEORIGIN",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'self'") {
		t.Errorf("CSP missing frame-ancestors: %q", csp)
	}
	if !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP missing script-src 'self': %q", csp)
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("CSP must not allow inline scripts: %q", csp)
	}
}

// itoaSmall avoids importing strconv just for test data.
func itoaSmall(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
