package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestGrowthRouteFreezesOnlyTaskAuthAndReleases(t *testing.T) {
	var requests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); io.WriteString(w, "private-task-route") }))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	var released, dynamic atomic.Int32
	c := New()
	c.DialProxy = func(string, string) (*url.URL, error) { dynamic.Add(1); return nil, nil }
	c.GrowthRoutePlan = func(context.Context, string, string) ([]string, bool, func(context.Context) (*url.URL, func(), error), error) {
		return []string{"::ffff:192.0.2.1", "192.0.2.1"}, false, func(context.Context) (*url.URL, func(), error) { return proxyURL, func() { released.Add(1) }, nil }, nil
	}
	original := &auth.Auth{UID: "account", AccessToken: "initial"}
	route := c.PrepareGrowthRoute(context.Background(), original)
	defer route.Release()
	if route.Auth == original {
		t.Fatal("shared auth pointer reused")
	}
	original.ApplyRefresh("updated", "", "", 0)
	if route.Auth.Snapshot().AccessToken != "initial" {
		t.Fatal("task credential was not frozen")
	}
	if !reflect.DeepEqual(route.Resources, []string{"proxy-ip:192.0.2.1"}) {
		t.Fatalf("resources %v", route.Resources)
	}
	cli, err := c.proxyClientFor(route.Auth)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := cli.Get("http://task.invalid/list")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(body) != "private-task-route" {
		t.Fatalf("private route response %q err=%v", body, err)
	}
	if requests.Load() != 1 || dynamic.Load() != 0 {
		t.Fatal("task used mutable account routing")
	}
	if _, err = c.proxyClientFor(original); err != nil {
		t.Fatal(err)
	}
	if dynamic.Load() != 1 {
		t.Fatal("ordinary account routing changed")
	}
	route.Release()
	route.Release()
	if released.Load() != 1 {
		t.Fatal("private route release is not idempotent")
	}
	if c.GrowthRouteValid(route.Auth) {
		t.Fatal("released auth remains active")
	}
	if _, err = c.proxyClientFor(route.Auth); err == nil {
		t.Fatal("released auth fell back to ordinary routing")
	}
	if original.EphemeralRequestScope() != nil {
		t.Fatal("task scope leaked into ordinary auth")
	}
}

func TestGrowthRouteUnknownDirectAndErrors(t *testing.T) {
	local, _ := url.Parse("http://127.0.0.1:31080")
	for _, tc := range []struct {
		name string
		hook GrowthRouteLeaseFunc
		dial DialProxyFunc
		want []string
	}{
		{name: "direct", want: []string{"direct"}},
		{name: "mutable local slot", dial: func(string, string) (*url.URL, error) { return local, nil }},
		{name: "unknown remote host", hook: func(context.Context, string, string) (*url.URL, []string, func(), error) { return local, nil, nil, nil }},
		{name: "invalid server address", hook: func(context.Context, string, string) (*url.URL, []string, func(), error) {
			return local, []string{"192.0.2.1", "node-1"}, nil, nil
		}},
		{name: "disabled proxy", want: []string{"direct"}, hook: func(context.Context, string, string) (*url.URL, []string, func(), error) { return nil, nil, nil, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New()
			c.DialProxy = tc.dial
			if tc.hook != nil {
				c.GrowthRoutePlan = func(ctx context.Context, uid, region string) ([]string, bool, func(context.Context) (*url.URL, func(), error), error) {
					proxy, ips, release, err := tc.hook(ctx, uid, region)
					return ips, proxy == nil, func(context.Context) (*url.URL, func(), error) { return proxy, release, err }, err
				}
			}
			route := c.PrepareGrowthRoute(context.Background(), &auth.Auth{UID: "a"})
			defer route.Release()
			if !reflect.DeepEqual(route.Resources, tc.want) {
				t.Fatalf("resources %v want %v", route.Resources, tc.want)
			}
		})
	}
	c := New()
	c.GrowthRouteLease = func(context.Context, string, string) (*url.URL, []string, func(), error) {
		return nil, nil, nil, errors.New("private-proxy-password")
	}
	route := c.PrepareGrowthRoute(context.Background(), &auth.Auth{UID: "a"})
	defer route.Release()
	_, err := c.proxyClientFor(route.Auth)
	if err == nil || strings.Contains(err.Error(), "private-proxy-password") {
		t.Fatalf("unsafe route error %v", err)
	}
	if len(route.Resources) != 0 {
		t.Fatal("failed route is not exclusive")
	}
}

func TestGrowthRouteCancellationPreventsUpstreamRequest(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); io.WriteString(w, "unexpected") }))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	c := New()
	route := c.PrepareGrowthRoute(ctx, &auth.Auth{UID: "a"})
	defer route.Release()
	cli, err := c.proxyClientFor(route.Auth)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err = cli.Get(srv.URL); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request err=%v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("cancelled task reached upstream")
	}
}
