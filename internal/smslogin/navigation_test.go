package smslogin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// TestNavigationRejectsCrossOriginRedirect is the HIGH-002 regression.
//
// followNavigation resolves Location with resp.Request.URL.Parse, which accepts
// any absolute cross-origin URL, and the client it drives carries the console
// cookie jar. A single hostile 302 therefore shipped AUTH_SESSION_ID /
// KEYCLOAK_IDENTITY / APISIX session cookies to the attacker's host.
func TestNavigationRejectsCrossOriginRedirect(t *testing.T) {
	var exfiltrated atomic.Value
	exfiltrated.Store("")

	// Attacker host: records whatever cookies it receives.
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exfiltrated.Store(r.Header.Get("Cookie"))
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()

	// A legitimate-looking host that immediately redirects off-site.
	hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/steal", http.StatusFound)
	}))
	defer hop.Close()

	s := newNavigationSession(t, Endpoints{Console: hop.URL, CLI: hop.URL, OneID: hop.URL, Realm: "copilot"})
	_, err := s.followNavigation(context.Background(), hop.URL+"/start", "")
	if err == nil {
		t.Fatal("followNavigation followed a cross-origin redirect to an untrusted host")
	}
	if !strings.Contains(err.Error(), "未授权主机") {
		t.Errorf("unexpected error: %v", err)
	}
	if got, _ := exfiltrated.Load().(string); got != "" {
		t.Errorf("cookies were sent to the attacker host: %q", got)
	}
}

// TestNavigationRejectsNonHTTPSchemes: file:/gopher: have no legitimate place in
// this flow and must be refused before any request is built.
func TestNavigationRejectsNonHTTPSchemes(t *testing.T) {
	s := newNavigationSession(t, DefaultEndpoints())
	for _, raw := range []string{
		"file:///etc/passwd",
		"gopher://evil.example/",
		"ftp://evil.example/",
		"//evil.example/path", // scheme-relative resolves to the current scheme
	} {
		if s.navigationHostAllowed(raw) {
			t.Errorf("navigationHostAllowed(%q) = true, want false", raw)
		}
	}
}

// TestNavigationAllowsLegitimateChainHosts: the allow-list must not break the
// real broker chain, which legitimately crosses these hosts.
func TestNavigationAllowsLegitimateChainHosts(t *testing.T) {
	s := newNavigationSession(t, DefaultEndpoints())
	for _, raw := range []string{
		"https://www.codebuddy.cn/console/accounts",
		"https://www.codebuddy.cn/auth/realms/copilot/protocol/openid-connect/auth",
		"https://copilot.tencent.com/v2/plugin/auth/token",
		"https://oauth2.account.tencent.com/v1/auth/sms/code/send",
		"https://codebuddy.cn/login/",
		"https://login.codebuddy.cn/realms/copilot", // legit subdomain
		"https://www.codebuddy.cn.evil.example/",    // must NOT match the suffix rule
	} {
		got := s.navigationHostAllowed(raw)
		want := !strings.Contains(raw, "evil.example")
		if got != want {
			t.Errorf("navigationHostAllowed(%q) = %v, want %v", raw, got, want)
		}
	}
}

// TestNavigationHostsFollowConfiguredEndpoints keeps the allow-list derived from
// configuration rather than hardcoded, so pointing Endpoints at a test server
// (or a future upstream host) still works.
func TestNavigationHostsFollowConfiguredEndpoints(t *testing.T) {
	s := newNavigationSession(t, Endpoints{
		Console: "https://console.test.invalid",
		CLI:     "https://cli.test.invalid",
		OneID:   "https://oneid.test.invalid",
		Realm:   "copilot",
	})
	for _, host := range []string{
		"https://console.test.invalid/x",
		"https://cli.test.invalid/x",
		"https://oneid.test.invalid/x",
	} {
		if !s.navigationHostAllowed(host) {
			t.Errorf("configured endpoint host rejected: %s", host)
		}
	}
	if s.navigationHostAllowed("https://other.test.invalid/x") {
		t.Error("unconfigured host accepted")
	}
}

// TestNavigationRejectsUnparseableLocation guards the error path.
func TestNavigationRejectsUnparseableLocation(t *testing.T) {
	s := newNavigationSession(t, DefaultEndpoints())
	if s.navigationHostAllowed("http://[::1") {
		t.Error("unparseable URL accepted")
	}
	if s.navigationHostAllowed("") {
		t.Error("empty URL accepted")
	}
}

// TestNavigationRejectsDifferentPortOnSameHost: cookie domain matching ignores
// the port, so an allow-list that compared hostnames only would hand the session
// to any other listener on the same IP (very real on shared/container hosts).
func TestNavigationRejectsDifferentPortOnSameHost(t *testing.T) {
	s := newNavigationSession(t, Endpoints{
		Console: "http://127.0.0.1:8080",
		CLI:     "http://127.0.0.1:8080",
		OneID:   "http://127.0.0.1:8080",
		Realm:   "copilot",
	})
	if !s.navigationHostAllowed("http://127.0.0.1:8080/ok") {
		t.Error("same origin rejected")
	}
	if s.navigationHostAllowed("http://127.0.0.1:9090/steal") {
		t.Error("same host, different port accepted — cookie would leak to another service")
	}
}

// newNavigationSession builds a session with a cookie jar and the given
// endpoints, mirroring what Manager.NewSession does.
func newNavigationSession(t *testing.T, ep Endpoints) *session {
	t.Helper()
	jar, err := newConsoleJar()
	if err != nil {
		t.Fatalf("newConsoleJar: %v", err)
	}
	s := &session{
		id: "test-session",
		consoleNoJump: &http.Client{
			Jar:           jar,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport:     wrapCookieFix(nil),
		},
	}
	s.initNavigationHosts(ep)
	// Seed a Keycloak cookie so the exfiltration test would actually observe it
	// leaving the process if the guard were removed.
	if u, err := url.Parse(ep.Console); err == nil {
		jar.SetCookies(u, []*http.Cookie{{Name: "KEYCLOAK_IDENTITY", Value: "super-secret", Path: "/"}})
	}
	return s
}
