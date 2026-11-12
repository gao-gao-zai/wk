package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func TestGrowthCompletedTaskClaimsWithoutRepeatingActions(t *testing.T) {
	fastGrowthActions(t)
	stub := &growthStub{tasks: `[{"task_code":"chat_5","target":5,"current":5,"accept_status":"accepted"}]`, claimCredit: 100}
	h, _ := newGrowthHandler(t, stub, &auth.Auth{UID: "u1", AccessToken: "fake"})
	code, out := growthPostAccount(t, h, "u1", "/tasks/auto", `{"task_code":"chat_5"}`)
	if code != 200 || out["claimed"] != true {
		t.Fatalf("status=%d result=%v", code, out)
	}
	if atomic.LoadInt64(&stub.reportCalls) != 0 || atomic.LoadInt64(&stub.claimCalls) != 1 || atomic.LoadInt64(&stub.listCalls) != 1 {
		t.Fatalf("completed task repeated work: report=%d claim=%d list=%d", stub.reportCalls, stub.claimCalls, stub.listCalls)
	}
}

func TestGrowthManualMutationsRespectAccountGuard(t *testing.T) {
	for _, entry := range []struct{ path, body string }{
		{"/tasks/accept", `{"task_codes":["chat_5"]}`},
		{"/tasks/accept_all", `{}`},
		{"/tasks/claim", `{"task_code":"chat_5"}`},
	} {
		t.Run(entry.path, func(t *testing.T) {
			stub := &growthStub{tasks: runnerTasksStub}
			h, _ := newGrowthHandler(t, stub, &auth.Auth{UID: "u1", AccessToken: "fake"})
			if !h.growthLocks.tryLock("u1") {
				t.Fatal("fixture lock unavailable")
			}
			defer h.growthLocks.unlock("u1")
			code, _ := growthPostAccount(t, h, "u1", entry.path, entry.body)
			if code != http.StatusConflict || atomic.LoadInt64(&stub.listCalls) != 0 || atomic.LoadInt64(&stub.claimCalls) != 0 {
				t.Fatal("manual mutation bypassed the account guard")
			}
		})
	}
}

func TestGrowthManualClaimRecordsOneOffCompletion(t *testing.T) {
	stub := &growthStub{tasks: runnerTasksStub, claimCredit: 10}
	h, _ := newGrowthHandler(t, stub, &auth.Auth{UID: "u1", AccessToken: "fake"})
	code, out := growthPostAccount(t, h, "u1", "/tasks/claim", `{"task_code":"chat_5"}`)
	if code != http.StatusOK || out["ok"] != true || h.growthTaskSkipReason("u1", &upstream.Task{TaskCode: "chat_5", TaskType: "single"}) == "" {
		t.Fatal("manual reward was not recorded for one-off completion checks")
	}
}

func TestGrowthNightWindowSkipsBeforeAcceptance(t *testing.T) {
	h, _ := newGrowthHandler(t, &growthStub{}, &auth.Auth{UID: "u1", AccessToken: "fake"})
	noon := time.Date(2026, 11, 12, 12, 0, 0, 0, time.Local)
	task := &upstream.Task{TaskCode: "black_cat", AcceptStatus: "not_accepted"}
	if h.growthTaskSkipReasonAt("u1", task, noon) == "" {
		t.Fatal("out-of-window night task was not skipped")
	}
	task.Claimable = true
	if reason := h.growthTaskSkipReasonAt("u1", task, noon); reason != "" {
		t.Fatalf("earned reward cannot be claimed outside action window: %s", reason)
	}
}

func TestGrowthRouteFailureReturnsSynchronousResponsesWithoutHanging(t *testing.T) {
	for _, entry := range []struct {
		path, body string
		status     int
	}{
		{"/tasks/accept", `{"task_codes":["chat_5"]}`, http.StatusBadGateway},
		{"/tasks/accept_all", `{}`, http.StatusBadGateway},
		{"/tasks/claim", `{"task_code":"chat_5"}`, http.StatusBadGateway},
		{"/tasks/auto", `{"task_code":"chat_5"}`, http.StatusBadGateway},
	} {
		t.Run(entry.path, func(t *testing.T) {
			stub := &growthStub{tasks: runnerTasksStub}
			h, _ := newGrowthHandler(t, stub, &auth.Auth{UID: "u1", AccessToken: "fake"})
			h.cfg.Upstream.GrowthRouteLease = func(context.Context, string, string) (*url.URL, []string, func(), error) {
				return nil, nil, nil, errors.New("private-provider-secret")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			r := httptest.NewRequest(http.MethodPost, "/admin/account/u1"+entry.path, strings.NewReader(entry.body)).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer "+testAPIKey)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != entry.status || strings.Contains(w.Body.String(), "private-provider-secret") || atomic.LoadInt64(&stub.listCalls) != 0 || atomic.LoadInt64(&stub.claimCalls) != 0 {
				t.Fatalf("route failure response invalid: status=%d", w.Code)
			}
		})
	}
}

func TestGrowthSkipsUnavailableTasksWithoutActionsOrPolling(t *testing.T) {
	fastGrowthActions(t)
	for _, extra := range []string{`"locked":true`, `"status":"expired"`, `"accept_status":"not_eligible"`, `"status":"completed"`} {
		t.Run(extra, func(t *testing.T) {
			stub := &growthStub{tasks: `[{"task_code":"chat_5","target":5,"current":1,` + extra + `}]`}
			h, _ := newGrowthHandler(t, stub, &auth.Auth{UID: "u1", AccessToken: "fake"})
			code, out := growthPostAccount(t, h, "u1", "/tasks/auto", `{"task_code":"chat_5"}`)
			if code != 200 || out["skipped"] != true || stub.reportCalls != 0 || stub.claimCalls != 0 || stub.listCalls != 1 {
				t.Fatalf("unavailable task executed: status=%d result=%v calls=%d/%d/%d", code, out, stub.reportCalls, stub.claimCalls, stub.listCalls)
			}
		})
	}
}

func TestGrowthOneOffLedgerDoesNotSuppressRecurringTasks(t *testing.T) {
	h, _ := newGrowthHandler(t, &growthStub{}, &auth.Auth{UID: "u1", AccessToken: "fake"})
	h.growthLedger.record("u1", "fake", "chat_5", 100, 0)
	if h.growthTaskSkipReason("u1", &upstream.Task{TaskCode: "chat_5", TaskType: "single"}) == "" {
		t.Fatal("one-off claim was ignored")
	}
	if reason := h.growthTaskSkipReason("u1", &upstream.Task{TaskCode: "chat_5", TaskType: "daily"}); reason != "" {
		t.Fatalf("recurring task suppressed: %s", reason)
	}
}

func TestGrowthBatchNeverAcceptsUnsupportedOrUnavailableTasks(t *testing.T) {
	fastGrowthActions(t)
	var accepts atomic.Int64
	h, _ := newGrowthHandler(t, &growthStub{}, &auth.Auth{UID: "u1", AccessToken: "fake"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"tasks": []map[string]any{
				{"task_code": "Expert_Philanthropy", "accept_status": "not_accepted"},
				{"task_code": "chat_5", "target": 5, "current": 0, "status": "expired"},
				{"task_code": "first_buddy", "accept_status": "claimed"},
			}}})
		case strings.HasSuffix(r.URL.Path, "/accept"):
			accepts.Add(1)
			w.Write([]byte(`{"code":0,"data":{}}`))
		default:
			t.Errorf("unexpected upstream action %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(srv.Close)
	h.cfg.Upstream.ChatBaseCN = srv.URL
	items, ok := h.runGrowthAllCollect(h.cfg.Pool.AuthByUID("u1"), nil)
	if !ok || accepts.Load() != 0 || len(items) != 2 || !items[0].Skipped || !items[1].Skipped {
		t.Fatalf("batch=%v ok=%v accepts=%d", items, ok, accepts.Load())
	}
}

func TestGrowthBatchRechecksTasksCompletedByEarlierAction(t *testing.T) {
	fastGrowthActions(t)
	var actions, claims atomic.Int64
	h, _ := newGrowthHandler(t, &growthStub{}, &auth.Auth{UID: "u1", AccessToken: "fake"})
	for _, code := range []string{"Buddy_App", "Buddy_App_QQ"} {
		action := growthActionFor(code)
		before := action.run
		action.run = func(*Handler, *auth.Auth) (string, error) { actions.Add(1); return "offline shared event", nil }
		t.Cleanup(func() { action.run = before })
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tasks") {
			current := 0
			if actions.Load() > 0 {
				current = 1
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"tasks": []map[string]any{
				{"task_code": "Buddy_App", "target": 1, "current": current, "accept_status": "accepted"},
				{"task_code": "Buddy_App_QQ", "target": 1, "current": current, "accept_status": "accepted"},
			}}})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/claim") {
			claims.Add(1)
			w.Write([]byte(`{"code":0,"data":{"credit":1}}`))
			return
		}
		t.Errorf("unexpected action endpoint %s", r.URL.Path)
		w.WriteHeader(500)
	}))
	t.Cleanup(srv.Close)
	h.cfg.Upstream.ChatBaseCN, h.cfg.Upstream.WebBaseCN = srv.URL, srv.URL
	items, ok := h.runGrowthAllCollect(h.cfg.Pool.AuthByUID("u1"), nil)
	if !ok || actions.Load() != 1 || claims.Load() != 2 || len(items) != 2 || !items[0].Claimed || !items[1].Claimed {
		t.Fatalf("shared event repeated: actions=%d claims=%d items=%v", actions.Load(), claims.Load(), items)
	}
}

func TestGrowthBatchDoesNotCountUnconfirmedActionAsCompleted(t *testing.T) {
	fastGrowthActions(t)
	stub := &growthStub{tasks: `[{"task_code":"chat_5","target":5,"current":0,"accept_status":"accepted"}]`}
	h, _ := newGrowthHandler(t, stub, &auth.Auth{UID: "u1", AccessToken: "fake"})
	action := growthActionFor("chat_5")
	before := action.run
	action.run = func(*Handler, *auth.Auth) (string, error) { return "上报成功", nil }
	t.Cleanup(func() { action.run = before })
	items, ok := h.runGrowthAllCollect(h.cfg.Pool.AuthByUID("u1"), nil)
	if !ok || len(items) != 1 || items[0].OK || items[0].Claimed || !strings.Contains(items[0].Message, "未确认完成") {
		t.Fatalf("unconfirmed event counted as complete: %+v", items)
	}
}
