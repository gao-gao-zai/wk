package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/groups"
	"workbuddy2api/internal/promptguard"
	"workbuddy2api/internal/upstream"
)

func selectionFixture(t *testing.T) (*Handler, *promptguard.Manager, *groups.Store, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer allowed-token" {
			t.Errorf("wrong account credential used")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseOK))
	}))
	t.Cleanup(fake.Close)
	up := upstream.New()
	up.HTTP = fake.Client()
	up.ChatBaseCN = fake.URL
	p := testPoolWith(&auth.Auth{UID: "original", AccessToken: "original-token", ExpiresAt: 9999999999}, &auth.Auth{UID: "allowed", Nickname: "诊断候选号", AccessToken: "allowed-token", ExpiresAt: 9999999999}, &auth.Auth{UID: "outside", AccessToken: "outside-token", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	p.Disable("original", "fixture")
	gs, err := groups.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = gs.Create("vip")
	_ = gs.SetAccountGroups("original", []string{"vip"})
	_ = gs.SetAccountGroups("allowed", []string{"vip"})
	_ = gs.SetAccountGroups("outside", []string{"default"})
	m, err := promptguard.Open("", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	h := NewHandler(Config{Pool: p, Upstream: up, Groups: gs, PromptGuard: m, APIKey: "fixture-admin"})
	return h, m, gs, &calls
}

func TestPromptSessionSelectsAllowedAvailableAccountAndPinsLease(t *testing.T) {
	h, _, gs, calls := selectionFixture(t)
	item := promptguard.Case{UID: "original", Model: "glm-5.3-flash", AccountScope: &promptguard.AccountScope{Kind: "group", Group: "vip", Accounts: []string{"original", "allowed"}}}
	session := h.promptSession(context.Background(), item)
	defer session.Close()
	if session.Selection.UID != "allowed" || session.Selection.ScopeKind != "group" || session.Selection.Group != "vip" {
		t.Fatalf("wrong selection: %+v", session.Selection)
	}
	if h.cfg.Pool.Acquire("allowed") {
		h.cfg.Pool.Release("allowed")
		t.Fatal("job did not hold its account slot")
	}
	for i := 0; i < 2; i++ {
		if r := session.Probe(context.Background(), []promptguard.Message{{Role: "system", Content: "fixture"}}, false); r.Outcome != "pass" {
			t.Fatalf("probe: %+v", r)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("unexpected probe count")
	}
	_ = gs.SetAccountGroups("allowed", []string{"default"})
	r := session.Probe(context.Background(), nil, false)
	if r.Reason != "account_scope_changed" || calls.Load() != 2 || *r.RequestAttempted {
		t.Fatal("scope change did not stop request")
	}
	session.Close()
	status, _ := h.cfg.Pool.Status("allowed")
	if status.InFlight != 0 {
		t.Fatal("slot leaked")
	}
}

func TestPromptSessionDoesNotExpandSnapshotOrInferLegacyGroup(t *testing.T) {
	h, _, _, calls := selectionFixture(t)
	for _, scope := range []*promptguard.AccountScope{nil, {Kind: "group", Group: "vip", Accounts: []string{"original"}}, {Kind: "group", Group: "deleted", Accounts: []string{"allowed"}}} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		session := h.promptSession(ctx, promptguard.Case{UID: "original", Model: "glm-5.3-flash", AccountScope: scope})
		r := session.Probe(ctx, nil, false)
		cancel()
		if r.Reason != "no_available_account" || session.Selection.UID != "" || calls.Load() != 0 {
			t.Fatal("scope expanded to unrelated available account")
		}
	}
}

func TestPromptSessionDoesNotUseCooledOrExpiredAccounts(t *testing.T) {
	for _, kind := range []string{"cooled", "expired"} {
		t.Run(kind, func(t *testing.T) {
			h, _, _, calls := selectionFixture(t)
			if kind == "cooled" {
				h.cfg.Pool.CooldownModel("allowed", "glm-5.3-flash", time.Minute, "fixture")
			} else {
				h.cfg.Pool.AuthByUID("allowed").ApplyRefresh("expired-fixture", "", "", time.Now().Add(-time.Hour).Unix())
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			session := h.promptSession(ctx, promptguard.Case{UID: "allowed", Model: "glm-5.3-flash"})
			r := session.Probe(ctx, nil, false)
			if r.Reason != "no_available_account" || calls.Load() != 0 {
				t.Fatal("used cooled or expired account for diagnosis")
			}
		})
	}
}

func TestPromptCapturePersistsOriginalScopeForHTTPAndSSE(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "sse"}[stream], func(t *testing.T) {
			h, m, gs, _ := selectionFixture(t)
			key, err := gs.CreateKey("fixture-vip", "vip")
			if err != nil {
				t.Fatal(err)
			}
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					_, _ = w.Write([]byte("data: {\"error\":{\"code\":11128}}\n\ndata: [DONE]\n\n"))
				} else {
					w.WriteHeader(400)
					_, _ = w.Write([]byte(`{"code":11128}`))
				}
			}))
			defer fake.Close()
			h.cfg.Upstream.HTTP = fake.Client()
			h.cfg.Upstream.ChatBaseCN = fake.URL
			r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.3-flash","stream":true,"messages":[{"role":"system","content":"fixture"},{"role":"user","content":"hello"}]}`))
			r.Header.Set("Authorization", "Bearer "+key.Key)
			h.ServeHTTP(httptest.NewRecorder(), r)
			list := m.List()
			if len(list) != 1 {
				t.Fatal("missing capture")
			}
			item, _ := m.Get(list[0].ID)
			if item.AccountScope == nil || item.AccountScope.Kind != "group" || item.AccountScope.Group != "vip" || !reflect.DeepEqual(item.AccountScope.Accounts, []string{"allowed", "original"}) {
				t.Fatalf("original scope not captured: %+v", item.AccountScope)
			}
		})
	}
}

func TestPromptLegacyOverrideRequiresAdminAndCannotWidenKnownScope(t *testing.T) {
	h, m, _, _ := selectionFixture(t)
	legacy, _ := m.Capture("glm-5.3-flash", "original", nil, nil)
	known, _ := m.CaptureScoped("glm-5.3-flash", "original", nil, nil, promptguard.AccountScope{Kind: "group", Group: "vip", Accounts: []string{"original"}})
	m.SetSessionFactory(func(_ context.Context, item promptguard.Case) promptguard.ProbeSession {
		return promptguard.ProbeSession{Selection: promptguard.AccountSelection{UID: "allowed"}, Probe: func(context.Context, []promptguard.Message, bool) promptguard.ProbeResult {
			return promptguard.ProbeResult{Outcome: "pass", HTTP: 200}
		}}
	})
	request := func(id, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/admin/prompt-guard/cases/"+id+"/verify", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer fixture-admin")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request(known, `{"account_scope":{"kind":"all"}}`); w.Code != 409 {
		t.Fatal("known scope override accepted")
	}
	if w := request(legacy, `{"account_scope":{"kind":"group","group":"vip","accounts":["outside"]}}`); w.Code != 400 {
		t.Fatal("client-injected account snapshot accepted")
	}
	w := request(legacy, `{"account_scope":{"kind":"group","group":"vip"}}`)
	if w.Code != 202 {
		t.Fatalf("legacy override: %d %s", w.Code, w.Body)
	}
	var job promptguard.Job
	_ = json.Unmarshal(w.Body.Bytes(), &job)
	if job.AccountScope == nil || !reflect.DeepEqual(job.AccountScope.Accounts, []string{"allowed", "original"}) {
		t.Fatal("scope snapshot not derived by server")
	}
}
