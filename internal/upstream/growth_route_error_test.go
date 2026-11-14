package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/growthrouteerr"
)

func TestGrowthRouteDiagnosticReasonSurvivesWithoutSecrets(t *testing.T) {
	const secret = "https://private-user:private-password@private-host/?token=private-token"
	for _, code := range []growthrouteerr.Code{growthrouteerr.PortCapacity, growthrouteerr.KernelInbound, growthrouteerr.KernelRule, growthrouteerr.KernelOutbound, growthrouteerr.DNSChanged, growthrouteerr.RouteChanged, growthrouteerr.StateUnavailable, growthrouteerr.RouteLimit} {
		for _, planning := range []bool{false, true} {
			c := New()
			c.GrowthRoutePlan = func(context.Context, string, string) ([]string, bool, func(context.Context) (*url.URL, func(), error), error) {
				cause := fmt.Errorf("%s: %w", secret, growthrouteerr.New(code))
				if planning {
					return nil, false, nil, cause
				}
				return []string{"192.0.2.1"}, false, func(context.Context) (*url.URL, func(), error) { return nil, nil, cause }, nil
			}
			route := c.PlanGrowthRoute(context.Background(), &auth.Auth{UID: "fixture"}, nil).Acquire()
			prefix := "growth task proxy route unavailable"
			if planning {
				prefix = "growth task proxy planning failed"
			}
			if route.Err == nil || route.Err.Error() != prefix+" [reason="+string(code)+"]" {
				t.Fatalf("incorrect diagnostic for %s planning=%v", code, planning)
			}
			_, requestErr := c.proxyClientFor(route.Auth)
			if requestErr == nil || strings.Contains(requestErr.Error(), secret) {
				t.Fatal("request scope lost safe diagnostic")
			}
			route.Release()
		}
	}
	c := New()
	c.GrowthRouteLease = func(context.Context, string, string) (*url.URL, []string, func(), error) {
		return nil, nil, nil, errors.New(secret)
	}
	route := c.PrepareGrowthRoute(context.Background(), &auth.Auth{UID: "fixture"})
	defer route.Release()
	if route.Err == nil || route.Err.Error() != "growth task proxy route unavailable [reason=unknown]" {
		t.Fatal("unknown cause was not redacted")
	}
}
