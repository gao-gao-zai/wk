package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

type websiteFixture struct {
	mu                sync.Mutex
	logins            map[string]int
	requests          []string
	bodies            []string
	firstUnauthorized bool
	codeStatus        int
	legacyCalls       int
	proxyCalls        atomic.Int32
}

func newWebsiteFixture(t *testing.T) (*Client, *websiteFixture) {
	t.Helper()
	f := &websiteFixture{logins: make(map[string]int)}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/plugin/device/auth/code":
			if r.Header.Get("Authorization") != "Bearer fake-at" || r.Header.Get("X-Refresh-Token") != "fake-rt" {
				t.Error("device authorization lost its real credentials")
			}
			identity := r.Header.Get("X-User-Id") + "-" + r.Header.Get("X-Domain") + "-" + r.Header.Get("X-Enterprise-Id")
			f.mu.Lock()
			f.logins[identity]++
			generation := f.logins[identity]
			status := f.codeStatus
			f.mu.Unlock()
			if status != 0 {
				w.WriteHeader(status)
				fmt.Fprint(w, `{"code":1}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]string{"deviceCode": fmt.Sprintf("%s-g%d", identity, generation)}})
		case "/console/client-login":
			if r.Header.Get("Authorization") != "" || r.Header.Get("X-Refresh-Token") != "" {
				t.Error("credentials leaked into cookie navigation")
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Value: r.URL.Query().Get("code"), Path: "/", HttpOnly: true})
			w.Header().Set("Location", "/profile/growth-center")
			w.WriteHeader(302)
		case "/profile/growth-center":
			fmt.Fprint(w, "page")
		default:
			body, _ := io.ReadAll(r.Body)
			cookie, err := r.Cookie("session")
			f.mu.Lock()
			f.requests = append(f.requests, r.Method+" "+r.URL.EscapedPath())
			f.bodies = append(f.bodies, string(body))
			first := f.firstUnauthorized
			if err != nil {
				f.legacyCalls++
			}
			f.mu.Unlock()
			if err != nil {
				if r.Header.Get("Authorization") != "Bearer fake-at" {
					t.Error("legacy fallback lost bearer")
				}
				fmt.Fprint(w, `{"code":0,"data":{"legacy":true}}`)
				return
			}
			for _, name := range []string{"Authorization", "X-Refresh-Token", "X-User-Id", "X-Tenant-Id"} {
				if r.Header.Get(name) != "" {
					t.Errorf("cookie request retained %s", name)
				}
			}
			if first && strings.HasSuffix(cookie.Value, "-g1") {
				w.WriteHeader(401)
				fmt.Fprint(w, `{"code":10034}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]string{"session": cookie.Value}})
		}
	}))
	t.Cleanup(proxy.Close)
	proxyURL, _ := url.Parse(proxy.URL)
	c := New()
	c.HTTP.Timeout = 2 * time.Second
	c.WebAuthMode = "cookie"
	// All traffic must traverse the chosen proxy; the target has no listener.
	c.ChatBaseCN, c.BillingBaseCN, c.WebBaseCN = "http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1"
	c.ChatBaseGlobal, c.BillingBaseGlob = c.ChatBaseCN, c.BillingBaseCN
	c.DialProxy = func(uid, region string) (*url.URL, error) {
		f.proxyCalls.Add(1)
		if uid == "" {
			t.Error("proxy selection lost uid")
		}
		return proxyURL, nil
	}
	return c, f
}

func TestWebsiteSessionsStayOnAccountProxyAndDoNotMix(t *testing.T) {
	c, f := newWebsiteFixture(t)
	accounts := []*auth.Auth{{UID: "u", AccessToken: "fake-at", RefreshToken: "fake-rt"}, {UID: "v", AccessToken: "fake-at", RefreshToken: "fake-rt"}, {UID: "u", Domain: "www.workbuddy.ai", AccessToken: "fake-at", RefreshToken: "fake-rt"}, {UID: "u", EnterpriseID: "e", AccessToken: "fake-at", RefreshToken: "fake-rt"}}
	var wg sync.WaitGroup
	for _, a := range accounts {
		for range 5 {
			wg.Add(1)
			go func(a *auth.Auth) {
				defer wg.Done()
				raw, err := c.growthJSON(a, "GET", "/activity/growth/buddy/info", nil)
				if err != nil {
					t.Error(err)
					return
				}
				var data struct {
					Session string `json:"session"`
				}
				_ = json.Unmarshal(raw, &data)
				s := a.Snapshot()
				want := s.UID + "-" + s.Domain + "-" + s.EnterpriseID + "-g1"
				if data.Session != want {
					t.Errorf("session crossed account/realm boundary")
				}
			}(a)
		}
	}
	wg.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.logins) != len(accounts) || f.legacyCalls != 0 {
		t.Fatalf("logins=%v legacy=%d", f.logins, f.legacyCalls)
	}
	for _, count := range f.logins {
		if count != 1 {
			t.Errorf("concurrent login was not coalesced: %d", count)
		}
	}
	if f.proxyCalls.Load() < int32(len(accounts)) {
		t.Fatal("traffic bypassed account route")
	}
}

func TestWebsite401RenewalKeepsWriteBodyAndStopsAfterOne(t *testing.T) {
	c, f := newWebsiteFixture(t)
	f.firstUnauthorized = true
	a := &auth.Auth{UID: "u", AccessToken: "fake-at", RefreshToken: "fake-rt"}
	if _, err := c.growthJSON(a, "POST", streakRedeemPath, map[string]any{"tier": "7d", "client_token": "same-idempotency-key"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 2 || f.bodies[0] != f.bodies[1] || f.legacyCalls != 0 {
		t.Fatalf("unexpected write replay/fallback: %v %v", f.requests, f.bodies)
	}
	for _, count := range f.logins {
		if count != 2 {
			t.Fatalf("renewal count=%d", count)
		}
	}
}

func TestWebsiteAutoFallsBackOnlyBeforeBusinessRequest(t *testing.T) {
	for _, status := range []int{404, 401, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c, f := newWebsiteFixture(t)
			c.WebAuthMode = "auto"
			f.codeStatus = status
			a := &auth.Auth{UID: "u", AccessToken: "fake-at", RefreshToken: "fake-rt"}
			for range 2 {
				_, err := c.growthJSON(a, "POST", travelClaimPath, map[string]any{"record_id": 123})
				if (status == 404) != (err == nil) {
					t.Fatalf("status=%d err=%v", status, err)
				}
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if status == 404 {
				if f.legacyCalls != 2 {
					t.Fatalf("legacy calls=%d", f.legacyCalls)
				}
				for _, count := range f.logins {
					if count != 1 {
						t.Fatal("unsupported endpoint was repeatedly probed")
					}
				}
			} else if len(f.requests) != 0 {
				t.Fatal("authorization/rate failure caused business request")
			}
		})
	}
}

func TestWebsiteTransportFailureNeverReplaysWrite(t *testing.T) {
	c := testClient(nil)
	c.WebAuthMode = "auto"
	a := &auth.Auth{UID: "u", AccessToken: "fake-at", RefreshToken: "fake-rt"}
	jar, _ := cookiejar.New(nil)
	base, _ := url.Parse(c.websiteBase(a))
	jar.SetCookies(base, []*http.Cookie{{Name: "session", Value: "fake-session", Path: "/"}})
	c.storeWebsite(c.websiteKey(a), websiteSession{jar: jar, created: time.Now()})
	var calls atomic.Int32
	c.HTTP.Transport = rtFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, context.DeadlineExceeded })
	_, err := c.growthJSON(a, "POST", lotteryDrawPath, map[string]any{"client_token": "same-key"})
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatalf("write replayed: calls=%d err=%v", calls.Load(), err)
	}
}

func TestWebsiteCurrentTaskPaths(t *testing.T) {
	c, f := newWebsiteFixture(t)
	a := &auth.Auth{UID: "u", AccessToken: "fake-at", RefreshToken: "fake-rt"}
	if err := c.AcceptTasks(a, []string{"RichMeow_Chat"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.ClaimReward(a, "RichMeow_Chat"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 2 || f.requests[0] != "POST /activity/growth/tasks/accept" || f.requests[1] != "POST /activity/growth/tasks/RichMeow_Chat/claim" {
		t.Fatalf("paths=%v", f.requests)
	}
}

func TestWebsiteAuthorizationNeverForwardsRefreshTokenOnRedirect(t *testing.T) {
	var leaked atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer sink.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Location", sink.URL); w.WriteHeader(302) }))
	defer api.Close()
	c := New()
	c.WebAuthMode = "cookie"
	c.ChatBaseCN, c.WebBaseCN = api.URL, api.URL
	_, err := c.growthJSON(&auth.Auth{UID: "u", AccessToken: "fake-at", RefreshToken: "fake-rt"}, "GET", buddyInfoPath, nil)
	if err == nil || leaked.Load() != 0 {
		t.Fatalf("authorization redirected: leaked=%d err=%v", leaked.Load(), err)
	}
}

func TestWebsiteCanceledLeaderDoesNotCancelWaiter(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v2/plugin/device/auth/code":
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			return jsonResp(200, `{"code":0,"data":{"deviceCode":"fake-code"}}`), nil
		case "/console/client-login":
			response := jsonResp(200, `{}`)
			response.Header.Set("Set-Cookie", "session=fake; Path=/")
			return response, nil
		default:
			return jsonResp(200, `{"code":0,"data":{"ok":true}}`), nil
		}
	})
	c.WebAuthMode = "cookie"
	a := &auth.Auth{UID: "u", AccessToken: "fake-at", RefreshToken: "fake-rt"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leader := make(chan error, 1)
	go func() { _, err := c.growthJSONContext(ctx, a, "GET", buddyInfoPath, nil); leader <- err }()
	<-started
	waiterContext := &joinedRefreshContext{Context: context.Background(), joined: make(chan struct{})}
	waiter := make(chan error, 1)
	go func() { _, err := c.growthJSONContext(waiterContext, a, "GET", buddyInfoPath, nil); waiter <- err }()
	<-waiterContext.joined
	cancel()
	if err := <-leader; !errors.Is(err, context.Canceled) {
		close(release)
		t.Fatalf("leader=%v", err)
	}
	close(release)
	if err := <-waiter; err != nil {
		t.Fatalf("waiter inherited leader cancellation: %v", err)
	}
}
