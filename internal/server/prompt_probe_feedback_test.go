package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/promptguard"
	"workbuddy2api/internal/upstream"
)

func TestPromptProbeRecordsLocalFailureWithoutRequestAttempt(t *testing.T) {
	up := upstream.New()
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: up, APIKey: "fixture"})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result := h.promptProbe(promptguard.Case{UID: "missing", Model: "glm-5.3-flash"})(ctx, []promptguard.Message{{Role: "system", Content: "fixture"}}, false)
	if result.Reason != "account_busy_or_unavailable" || result.HTTP != 0 || result.RequestAttempted == nil || *result.RequestAttempted {
		t.Fatalf("local precondition reported as request: %+v", result)
	}
}

func TestPromptProbeRecordsHTTPAndSafeErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		status int
		reason string
	}{{200, ""}, {429, "rate_limit"}, {401, "upstream_auth_error"}} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				if tc.status == 200 {
					_, _ = w.Write([]byte(sseOK))
				} else {
					_, _ = w.Write([]byte(`{"message":"sensitive upstream fixture must not leak"}`))
				}
			}))
			defer fake.Close()
			up := upstream.New()
			up.HTTP = fake.Client()
			up.ChatBaseCN = fake.URL
			h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "fixture", ExpiresAt: 9999999999}), Upstream: up, APIKey: "fixture"})
			result := h.promptProbe(promptguard.Case{UID: "u1", Model: "glm-5.3-flash"})(context.Background(), []promptguard.Message{{Role: "system", Content: "fixture"}}, false)
			if result.RequestAttempted == nil || !*result.RequestAttempted || result.HTTP != tc.status || result.Reason != tc.reason {
				t.Fatalf("request result: %+v", result)
			}
		})
	}
}
