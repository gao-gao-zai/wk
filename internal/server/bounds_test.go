package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// observingLogStore 记录 RecentRequests 实际收到的 limit。
type observingLogStore struct {
	captureRequestLogStore
	gotLimit int
	called   bool
}

func (s *observingLogStore) RecentRequests(limit int) ([]RequestLog, error) {
	s.gotLimit = limit
	s.called = true
	return nil, nil
}

// TestRequestsLimitIsClamped: /v1/requests is reachable by any authenticated
// caller, and `limit` was passed straight to the store. A request for
// limit=2000000000 either allocates wildly or errors out depending on backend.
func TestRequestsLimitIsClamped(t *testing.T) {
	cases := []struct {
		query string
		want  int
	}{
		{"", 50},            // no limit → default
		{"?limit=10", 10},   // in range → honoured
		{"?limit=500", 500}, // exactly the cap → honoured
		{"?limit=501", 500}, // just over → clamped
		{"?limit=1000000", 500},
		{"?limit=2000000000", 500}, // would overflow int32 conversions downstream
		{"?limit=0", 50},           // zero is not "all"
		{"?limit=-5", 50},          // negative is not "all"
		{"?limit=abc", 50},         // garbage → default
		{"?limit=", 50},            // empty → default
	}
	for _, tc := range cases {
		store := &observingLogStore{}
		h := newTestHandler(t, Config{RequestLogStore: store})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/requests"+tc.query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("limit=%q code=%d body=%s", tc.query, rec.Code, rec.Body)
		}
		if !store.called {
			t.Fatalf("limit=%q: store was not consulted", tc.query)
		}
		if store.gotLimit != tc.want {
			t.Errorf("limit=%q passed %d to store, want %d", tc.query, store.gotLimit, tc.want)
		}
	}
}

// TestRateLimitResetAtRejectsAbsurdlyDistantReset is the account-freezing fix:
// reset_at becomes the cooldown deadline, so an untrusted upstream body
// containing a far-future date could disable a healthy account indefinitely.
func TestRateLimitResetAtRejectsAbsurdlyDistantReset(t *testing.T) {
	location := time.FixedZone("UTC+8", 8*60*60)
	now := time.Date(2026, 9, 11, 18, 0, 0, 0, location)
	for _, body := range []string{
		`{"code":6004,"msg":"将在 9999-12-31 23:59:59 UTC+8 重置"}`,
		`{"code":6004,"msg":"将在 2027-09-11 18:00:00 UTC+8 重置"}`, // ~1 year
		`{"code":6004,"msg":"将在 2026-09-13 18:00:01 UTC+8 重置"}`, // just past 24h
	} {
		got, ok := rateLimitResetAt(body, now)
		if ok || !got.IsZero() {
			t.Errorf("distant reset accepted: body=%s got=%v ok=%v", body, got, ok)
		}
	}
}

// TestRateLimitResetAtAcceptsPlausibleReset ensures the ceiling did not
// swallow the real cases it must keep working.
func TestRateLimitResetAtAcceptsPlausibleReset(t *testing.T) {
	location := time.FixedZone("UTC+8", 8*60*60)
	now := time.Date(2026, 9, 11, 9, 0, 0, 0, location)
	for _, tc := range []struct {
		body string
		want time.Time
	}{
		{`{"code":6004,"msg":"将在 2026-09-11 17:58:44 UTC+8 重置"}`, time.Date(2026, 9, 11, 17, 58, 44, 0, location)},
		{`{"code":6004,"msg":"将在 2026-09-12 00:00:00 UTC+8 重置"}`, time.Date(2026, 9, 12, 0, 0, 0, 0, location)},
		{`{"code":6004,"msg":"将在 2026-09-12 09:00:00 UTC+8 重置"}`, time.Date(2026, 9, 12, 9, 0, 0, 0, location)}, // exactly 24h
	} {
		got, ok := rateLimitResetAt(tc.body, now)
		if !ok || !got.Equal(tc.want) {
			t.Errorf("body=%s got=%v ok=%v want=%v", tc.body, got, ok, tc.want)
		}
	}
}
