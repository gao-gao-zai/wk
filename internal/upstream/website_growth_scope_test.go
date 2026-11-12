package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestGrowthWebsiteSnapshotKeepsPrivateRouteGuardAndCancellation(t *testing.T) {
	for _, mode := range []string{"auto", "cookie"} {
		for _, invalidate := range []string{"delete", "refresh", "cancel", "release"} {
			t.Run(mode+"/"+invalidate, func(t *testing.T) {
				c, f := newWebsiteFixture(t)
				c.WebAuthMode = mode
				privateProxy, err := c.DialProxy("setup", "cn")
				if err != nil {
					t.Fatal(err)
				}
				f.proxyCalls.Store(0)
				var active atomic.Int32
				c.GrowthRoutePlan = func(context.Context, string, string) ([]string, bool, func(context.Context) (*url.URL, func(), error), error) {
					return []string{"192.0.2.1"}, false, func(context.Context) (*url.URL, func(), error) {
						active.Add(1)
						return privateProxy, func() { active.Add(-1) }, nil
					}, nil
				}
				original := &auth.Auth{UID: "u", AccessToken: "fake-at", RefreshToken: "fake-rt"}
				before := original.Snapshot()
				var alive atomic.Bool
				alive.Store(true)
				guard := func() error {
					if !alive.Load() || original.Snapshot() != before {
						return ErrGrowthAuthStale
					}
					return nil
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				route := c.PlanGrowthRoute(ctx, original, guard).Acquire()
				defer route.Release()
				if route.Err != nil {
					t.Fatal(route.Err)
				}
				scoped := route.Auth
				operations := []func() error{
					func() error { return c.AcceptTasks(scoped, []string{"chat_5"}) },
					func() error { _, _, err := c.ClaimReward(scoped, "chat_5"); return err },
					func() error { _, err := c.BuddyInfo(scoped); return err },
					func() error {
						return c.ReportWebEvent(scoped, "fixture", c.WebBaseCN+"/profile/growth-center", "fixture", "fixture")
					},
				}
				for _, op := range operations {
					if err := op(); err != nil {
						t.Fatal(err)
					}
				}
				if f.proxyCalls.Load() != 0 {
					t.Fatal("website snapshot fell through ordinary DialProxy/business binding")
				}
				c.webMu.Lock()
				ordinarySessions := len(c.webSessions)
				c.webMu.Unlock()
				if ordinarySessions != 0 {
					t.Fatal("task cookie/login leaked into ordinary account session cache")
				}
				scope := c.growthRouteFor(scoped)
				scope.webMu.Lock()
				taskSessions := len(scope.webSessions)
				scope.webMu.Unlock()
				if taskSessions != 1 {
					t.Fatalf("private website session count %d", taskSessions)
				}
				alias := scoped
				for range 100 {
					alias = c.growthAuthSnapshot(alias, alias.Snapshot())
				}
				if alias == scoped || original.EphemeralRequestScope() != nil {
					t.Fatal("snapshot scope mixed with shared auth")
				}
				if alias.Snapshot().FilePath != "" {
					t.Fatal("task snapshot retained credential writeback path")
				}
				switch invalidate {
				case "delete":
					alive.Store(false)
				case "refresh":
					original.ApplyRefresh("rotated", "rotated-r", "", 100)
				case "cancel":
					cancel()
				case "release":
					route.Release()
				}
				f.mu.Lock()
				requests := len(f.requests)
				f.mu.Unlock()
				for _, op := range operations {
					if err := op(); err == nil {
						t.Fatal("invalid website task reached a business action")
					}
				}
				if c.GrowthRouteValid(alias) {
					t.Fatal("invalid inherited auth still valid")
				}
				if _, err := c.proxyClientFor(alias); err == nil {
					t.Fatal("invalid alias fell back to ordinary routing")
				}
				f.mu.Lock()
				after := len(f.requests)
				f.mu.Unlock()
				if after != requests || f.proxyCalls.Load() != 0 {
					t.Fatal("invalid scope performed HTTP or ordinary proxy selection")
				}
				route.Release()
				scope.webMu.Lock()
				remaining := len(scope.webSessions)
				scope.webMu.Unlock()
				if remaining != 0 || active.Load() != 0 {
					t.Fatal("release leaked cookie state or private route")
				}
				if err := alias.SaveAtomic(); err == nil {
					t.Fatal("released task snapshot could write back credentials")
				}
				if _, err := c.proxyClientFor(original); err != nil {
					t.Fatal("task cancellation affected ordinary route")
				}
				if f.proxyCalls.Load() != 1 {
					t.Fatal("ordinary model/account routing inherited task scope")
				}
			})
		}
	}
}

func TestGrowthSnapshotScopeDoesNotJoinOrdinaryWebsiteLogin(t *testing.T) {
	c, f := newWebsiteFixture(t)
	privateProxy, _ := c.DialProxy("setup", "cn")
	f.proxyCalls.Store(0)
	c.GrowthRoutePlan = func(context.Context, string, string) ([]string, bool, func(context.Context) (*url.URL, func(), error), error) {
		return []string{"192.0.2.1"}, false, func(context.Context) (*url.URL, func(), error) { return privateProxy, nil, nil }, nil
	}
	original := &auth.Auth{UID: "u", AccessToken: "fake-at", RefreshToken: "fake-rt"}
	if err := c.AcceptTasks(original, []string{"chat_5"}); err != nil {
		t.Fatal(err)
	}
	ordinaryCalls := f.proxyCalls.Load()
	route := c.PrepareGrowthRoute(context.Background(), original)
	defer route.Release()
	if err := c.AcceptTasks(route.Auth, []string{"chat_5"}); err != nil {
		t.Fatal(err)
	}
	if f.proxyCalls.Load() != ordinaryCalls {
		t.Fatal("task login used ordinary account routing")
	}
	f.mu.Lock()
	logins := f.logins["u--"]
	f.mu.Unlock()
	if logins != 2 {
		t.Fatalf("task reused ordinary route's login/session: %d", logins)
	}
	route.Release()
	if err := c.AcceptTasks(original, []string{"chat_5"}); err != nil {
		t.Fatal("release changed ordinary cookie cache")
	}
}

func TestGrowthEveryHTTPChecksGuardIncludingInFlightResponse(t *testing.T) {
	entered, finish := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		close(entered)
		<-finish
		io.WriteString(w, "ok")
	}))
	defer srv.Close()
	c := New()
	var valid atomic.Bool
	valid.Store(true)
	guard := func() error {
		if !valid.Load() {
			return ErrGrowthAuthStale
		}
		return nil
	}
	route := c.PlanGrowthRoute(context.Background(), &auth.Auth{UID: "u"}, guard).Acquire()
	defer route.Release()
	cli, err := c.proxyClientFor(route.Auth)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		resp, err := cli.Get(srv.URL)
		if resp != nil {
			resp.Body.Close()
		}
		result <- err
	}()
	<-entered
	valid.Store(false)
	close(finish)
	if err := <-result; !errors.Is(err, ErrGrowthAuthStale) {
		t.Fatalf("stale in-flight response accepted: %v", err)
	}
	if _, err := cli.Get(srv.URL); !errors.Is(err, ErrGrowthAuthStale) {
		t.Fatalf("subsequent HTTP bypassed guard: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatal("stale task sent a subsequent HTTP request")
	}
}

func TestGrowthCatalogueAndResourceSnapshotsKeepScope(t *testing.T) {
	var requests, dynamic atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"data":{}}`)
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	c := New()
	c.ChatBaseCN = "http://provider.fixture.invalid"
	c.BillingBaseCN = c.ChatBaseCN
	c.WebBaseCN = c.ChatBaseCN
	c.DialProxy = func(string, string) (*url.URL, error) {
		dynamic.Add(1)
		return nil, errors.New("ordinary route forbidden for scoped request")
	}
	c.GrowthRoutePlan = func(context.Context, string, string) ([]string, bool, func(context.Context) (*url.URL, func(), error), error) {
		return []string{"192.0.2.1"}, false, func(context.Context) (*url.URL, func(), error) { return proxyURL, nil, nil }, nil
	}
	var valid atomic.Bool
	valid.Store(true)
	route := c.PlanGrowthRoute(context.Background(), &auth.Auth{UID: "u", AccessToken: "fixture"}, func() error {
		if !valid.Load() {
			return ErrGrowthAuthStale
		}
		return nil
	}).Acquire()
	defer route.Release()
	// The intentionally empty fixture may fail schema validation; networking
	// must still use the inherited private scope through every fallback.
	_, _ = c.FetchModelsContext(context.Background(), route.Auth)
	_, _ = c.UserResourceDetailsContext(context.Background(), route.Auth)
	if requests.Load() == 0 || dynamic.Load() != 0 {
		t.Fatal("collection snapshot escaped task route")
	}
	before := requests.Load()
	valid.Store(false)
	_, _ = c.FetchModelsContext(context.Background(), route.Auth)
	_, _ = c.UserResourceDetailsContext(context.Background(), route.Auth)
	if requests.Load() != before || dynamic.Load() != 0 {
		t.Fatal("stale collection snapshot performed networking")
	}
}
