// growthtasks_test.go 成长任务端点与动作链的单测。
//
// 上游用 httptest server stub 全部 growth 域端点（chatBase/billingBase/webBase
// 全指向 stub），验证：
//   - 任务列表查询透出 auto_actions
//   - 一键完成单个任务：动作执行 → 回读 → 达标自动领奖
//   - 一键完成全部：批量报名 + 依赖序执行 + 已 claimed 跳过
//   - per-account 锁：并发请求第二个 409
//   - global 账号 400、不存在账号 404
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/collection"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// growthStub growth 域全部端点的可编程 stub。
type growthStub struct {
	mu sync.Mutex
	// tasks 任务列表原始 JSON（data.tasks 的内容）。
	tasks string
	// claimCredit / claimEnergy claim 端点返回的奖励。
	claimCredit, claimEnergy int64
	// report / desktop 事件计数。
	reportCalls, desktopCalls int64
	// claimCalls claim 调用次数。
	claimCalls  int64
	acceptCalls int64
	// reportProgress 上报驱动进度：每批 /v2/report 让 RichMeow_Chat 的 current +2
	// （模拟服务端异步计分；false = 进度恒定）。
	reportProgress bool
	listCalls      int64
}

func (s *growthStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.URL.Path {
		case "/v2/activity/growth/tasks":
			atomic.AddInt64(&s.listCalls, 1)
			fmt.Fprintf(w, `{"code":0,"data":{"tasks":%s}}`, s.tasks)
		case "/v2/activity/growth/tasks/accept":
			atomic.AddInt64(&s.acceptCalls, 1)
			w.Write([]byte(`{"code":0,"data":{}}`))
		case "/v2/report":
			n := atomic.AddInt64(&s.reportCalls, 1)
			// 一批桌面事件链让样例进度从 3/5 达标。
			if s.reportProgress {
				prev, cur := 3+2*(int(n)-1), 3+2*int(n)
				if cur > 5 {
					cur = 5
				}
				s.tasks = strings.Replace(s.tasks,
					fmt.Sprintf(`"current":%d`, prev), fmt.Sprintf(`"current":%d`, cur), 1)
			}
			w.Write([]byte(`{"code":0,"data":{}}`))
		case "/portal/operation-platform/market/expert/list":
			w.Write([]byte(`{"code":0,"data":{"list":[{"expert_id":"ex_1","expert_type":"agent","display_name_zh":"专家一","profession_zh":"专家一","version":"1.0.0"}]}}`))
		default:
			// 领奖走 Web 域（workbuddy.cn），路径形如 /activity/growth/tasks/<code>/claim。
			if strings.HasPrefix(r.URL.Path, "/activity/growth/tasks/") && strings.HasSuffix(r.URL.Path, "/claim") {
				atomic.AddInt64(&s.claimCalls, 1)
				fmt.Fprintf(w, `{"code":0,"data":{"credit":%d,"energy":%d}}`, s.claimCredit, s.claimEnergy)
				return
			}
			// 其余 growth 端点（travel/buddy/streak/appearance 等）一律成功空 data。
			w.Write([]byte(`{"code":0,"data":{}}`))
		}
	})
}

// newGrowthHandler 构造接好 pool/upstream 的 handler（growth 域全指向 stub）。
// 台账落盘到 t.TempDir()（验证持久化语义）。
func newGrowthHandler(t *testing.T, stub *growthStub, accounts ...*auth.Auth) (*Handler, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	p := pool.New("")
	for _, a := range accounts {
		p.Add(a)
	}
	up := &upstream.Client{
		HTTP:            srv.Client(),
		ChatBaseCN:      srv.URL,
		BillingBaseCN:   srv.URL,
		ChatBaseGlobal:  srv.URL,
		BillingBaseGlob: srv.URL,
		WebBaseCN:       srv.URL,
	}
	h := newTestHandler(t, Config{Pool: p, Upstream: up,
		GrowthLedgerPath: filepath.Join(t.TempDir(), "growth-ledger.json")})
	return h.Handler, srv
}

// growthPostAccount 对账号端点发 POST 并解析 JSON 响应（带测试管理员凭证）。
func growthPostAccount(t *testing.T, h *Handler, uid, suffix, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/account/"+uid+suffix, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// fastGrowthActions 关闭动作节流/轮询等待，测试不白等。
func fastGrowthActions(t *testing.T) {
	t.Helper()
	oldGap, oldExpert := actionStepGap, expertBatchGap
	oldAttempts, oldPollGap := claimPollAttempts, claimPollGap
	actionStepGap, expertBatchGap = 0, 0
	claimPollAttempts, claimPollGap = 1, 0
	t.Cleanup(func() {
		actionStepGap, expertBatchGap = oldGap, oldExpert
		claimPollAttempts, claimPollGap = oldAttempts, oldPollGap
	})
}

// 標準任务列表：RichMeow_Chat 未达标（3/5），first_buddy 已领取。
const growthTasksSample = `[
  {"task_code":"RichMeow_Chat","title":"发起 5 次对话","reward_credit":100,"target":5,"current":3,"accept_status":"accepted"},
  {"task_code":"first_buddy","title":"领养第一只猫","reward_credit":300,"target":1,"current":1,"accept_status":"claimed"},
  {"task_code":"Expert_Philanthropy","title":"爱心捐赠","reward_credit":100,"target":1,"current":0,"accept_status":"not_accepted"}
]`

func TestAccountTasksReturnsListAndAutoActions(t *testing.T) {
	fastGrowthActions(t)
	stub := &growthStub{tasks: growthTasksSample}
	h, _ := newGrowthHandler(t, stub, &auth.Auth{UID: "u1", AccessToken: "at"})
	req := httptest.NewRequest(http.MethodGet, "/admin/account/u1/tasks", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Tasks       []map[string]any `json:"tasks"`
		AutoActions map[string]bool  `json:"auto_actions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tasks) != 2 {
		t.Fatalf("tasks = %d, want 2 (retired task filtered)", len(out.Tasks))
	}
	if len(out.AutoActions) != 3 || !out.AutoActions["RichMeow_Chat"] || !out.AutoActions["first_buddy"] || !out.AutoActions["Library_read"] {
		t.Fatalf("auto_actions missing growth codes: %v", out.AutoActions)
	}
	if out.AutoActions["Expert_Philanthropy"] {
		t.Fatal("Expert_Philanthropy should not be autoable")
	}
}

func TestAccountTaskAutoRunsAndClaims(t *testing.T) {
	fastGrowthActions(t)
	// reportProgress：一批桌面事件链驱动计分达标 → 自动领奖。
	stub := &growthStub{
		tasks:          growthTasksSample,
		reportProgress: true,
		claimCredit:    100, claimEnergy: 5,
	}
	h, _ := newGrowthHandler(t, stub, &auth.Auth{UID: "u1", AccessToken: "at"})
	code, out := growthPostAccount(t, h, "u1", "/tasks/auto", `{"task_code":"RichMeow_Chat"}`)
	if code != 200 {
		t.Fatalf("status = %d, want 200, body=%v", code, out)
	}
	if out["claimed"] != true {
		t.Fatalf("claimed = %v, want true (out=%v)", out["claimed"], out)
	}
	if stub.reportCalls != 1 { // 一批桌面事件链使 stub 计分达标。
		t.Fatalf("report calls = %d, want 1", stub.reportCalls)
	}
	if stub.claimCalls != 1 {
		t.Fatalf("claim calls = %d, want 1", stub.claimCalls)
	}
}

func TestAccountTaskAutoSkipsClaimed(t *testing.T) {
	fastGrowthActions(t)
	stub := &growthStub{tasks: growthTasksSample}
	h, _ := newGrowthHandler(t, stub, &auth.Auth{UID: "u1", AccessToken: "at"})
	code, out := growthPostAccount(t, h, "u1", "/tasks/auto", `{"task_code":"first_buddy"}`)
	if code != 200 || out["skipped"] != true {
		t.Fatalf("claimed task should skip: code=%d out=%v", code, out)
	}
	if stub.reportCalls != 0 {
		t.Fatalf("report calls = %d, want 0 (skip must not run actions)", stub.reportCalls)
	}
}

func TestAccountTaskAutoRejectsNonAutoable(t *testing.T) {
	stub := &growthStub{tasks: growthTasksSample}
	h, _ := newGrowthHandler(t, stub, &auth.Auth{UID: "u1", AccessToken: "at"})
	code, _ := growthPostAccount(t, h, "u1", "/tasks/auto", `{"task_code":"Expert_Philanthropy"}`)
	if code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", code)
	}
}

func TestAccountTaskAutoAllRunsDependencyOrder(t *testing.T) {
	fastGrowthActions(t)
	stub := &growthStub{tasks: growthTasksSample, claimCredit: 50}
	h, _ := newGrowthHandler(t, stub, &auth.Auth{UID: "u1", AccessToken: "at"})
	code, out := growthPostAccount(t, h, "u1", "/tasks/auto_all", `{}`)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (async job)", code)
	}
	jobID, _ := out["job_id"].(string)
	if jobID == "" {
		t.Fatalf("job_id missing: %v", out)
	}
	// 轮询等 job 完成（上限 5s；正常 <100ms）。
	snap := growthWaitJob(t, h, jobID, 5*time.Second)
	if snap["phase"] != "done" {
		t.Fatalf("phase = %v, want done (snap=%v)", snap["phase"], snap)
	}
	results, _ := snap["results"].([]any)
	// 报名 1 项 + RichMeow_Chat + first_buddy（Philanthropy 无动作）。
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3 (got %v)", len(results), results)
	}
	// 依赖序：accept → first_buddy（已 claimed 跳过）→ RichMeow_Chat。
	first, _ := results[1].(map[string]any)
	if first["task_code"] != "first_buddy" {
		t.Fatalf("first action should be first_buddy (dependency order), got %v", first["task_code"])
	}
	// done/total 计数自洽。
	if snap["done"].(float64) != 3 || snap["total"].(float64) != 3 {
		t.Fatalf("done/total = %v/%v, want 3/3", snap["done"], snap["total"])
	}
}

// growthWaitJob 轮询 job 状态直到非 running 或超时。
func growthWaitJob(t *testing.T, h *Handler, jobID string, limit time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		req := httptest.NewRequest(http.MethodGet, "/admin/growth/jobs/"+jobID, nil)
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		rec := httptest.NewRecorder()
		h.mux.ServeHTTP(rec, req)
		var snap map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil || rec.Code != 200 {
			t.Fatalf("job status poll failed: code=%d body=%s", rec.Code, rec.Body.String())
		}
		if phase, _ := snap["phase"].(string); phase != "running" {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s did not finish within %v (snap=%v)", jobID, limit, snap)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestGrowthJobListShowsActive(t *testing.T) {
	fastGrowthActions(t)
	// 慢轮询让 job 短暂处于 running，可查 active 列表。
	claimPollAttempts, claimPollGap = 3, 80*time.Millisecond
	t.Cleanup(func() { claimPollAttempts, claimPollGap = 1, 0 })
	stub := &growthStub{tasks: growthTasksSample}
	h, _ := newGrowthHandler(t, stub, &auth.Auth{UID: "u1", AccessToken: "at"})
	code, out := growthPostAccount(t, h, "u1", "/tasks/auto_all", `{}`)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	jobID, _ := out["job_id"].(string)
	// 列表端点应包含刚创建的 running job。
	req := httptest.NewRequest(http.MethodGet, "/admin/growth/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	var list struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, j := range list.Jobs {
		if j["id"] == jobID {
			found = true
			if j["phase"] != "running" {
				t.Fatalf("active job phase = %v, want running", j["phase"])
			}
		}
	}
	if !found {
		t.Fatalf("job %s not in active list: %v", jobID, list.Jobs)
	}
	growthWaitJob(t, h, jobID, 5*time.Second)
	// 完成后 active 列表应为空。
	rec2 := httptest.NewRecorder()
	h.mux.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/admin/growth/jobs", nil))
	var list2 struct {
		Jobs []map[string]any `json:"jobs"`
	}
	_ = json.Unmarshal(rec2.Body.Bytes(), &list2)
	if len(list2.Jobs) != 0 {
		t.Fatalf("active jobs after finish = %d, want 0", len(list2.Jobs))
	}
}

func TestGrowthJobStatus404ForUnknown(t *testing.T) {
	stub := &growthStub{tasks: growthTasksSample}
	h, _ := newGrowthHandler(t, stub)
	req := httptest.NewRequest(http.MethodGet, "/admin/growth/jobs/nope", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestAllAccountsAutoAllJob(t *testing.T) {
	fastGrowthActions(t)
	stub := &growthStub{tasks: growthTasksSample, claimCredit: 50}
	h, _ := newGrowthHandler(t, stub,
		&auth.Auth{UID: "u1", AccessToken: "at"},
		&auth.Auth{UID: "u2", AccessToken: "at"},
	)
	req := httptest.NewRequest(http.MethodPost, "/admin/tasks/auto_all", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body=%v)", rec.Code, out)
	}
	jobID, _ := out["job_id"].(string)
	if out["total_accounts"].(float64) != 2 {
		t.Fatalf("total_accounts = %v, want 2", out["total_accounts"])
	}
	snap := growthWaitJob(t, h, jobID, 5*time.Second)
	if snap["phase"] != "done" {
		t.Fatalf("phase = %v, want done (snap=%v)", snap["phase"], snap)
	}
	// 进度 = 账号粒度：2 个账号全部完成。
	if snap["done"].(float64) != 2 || snap["total"].(float64) != 2 {
		t.Fatalf("done/total = %v/%v, want 2/2 (account granularity)", snap["done"], snap["total"])
	}
	// 每账号 2 项任务（RichMeow_Chat + first_buddy）× 2 账号 = 4（all 模式无报名条目）。
	results, _ := snap["results"].([]any)
	if len(results) != 4 {
		t.Fatalf("results = %d, want 4 (got %v)", len(results), results)
	}
}

func TestGrowthLedgerRecordsClaimsAndOverview(t *testing.T) {
	fastGrowthActions(t)
	// RichMeow_Chat 上报后达标（claim +50 分）；first_buddy 上游已领（对账补全）。
	stub := &growthStub{tasks: growthTasksSample, claimCredit: 50, claimEnergy: 5, reportProgress: true}
	h, _ := newGrowthHandler(t, stub,
		&auth.Auth{UID: "u1", AccessToken: "at"},
		&auth.Auth{UID: "u2", AccessToken: "at"}, // u2 不跑任何动作
	)
	// u1 跑单任务 RichMeow_Chat：台账记 1 条（+50 分 +5 能）。
	code, out := growthPostAccount(t, h, "u1", "/tasks/auto", `{"task_code":"RichMeow_Chat"}`)
	if code != http.StatusOK || out["claimed"] != true {
		t.Fatalf("auto RichMeow_Chat: code=%d out=%v", code, out)
	}
	// 查台账总览（并发预取 + 合并对账）。
	req := httptest.NewRequest(http.MethodGet, "/admin/growth/ledger", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	var ov struct {
		TotalAccounts   int   `json:"total_accounts"`
		DoneAccounts    int   `json:"done_accounts"`
		PartialAccounts int   `json:"partial_accounts"`
		NotStarted      int   `json:"not_started"`
		TotalCredit     int64 `json:"total_credit"`
		TotalEnergy     int64 `json:"total_energy"`
		Accounts        []struct {
			UID       string `json:"uid"`
			Status    string `json:"status"`
			DoneCount int    `json:"done_count"`
			Credit    int64  `json:"credit"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ov); err != nil {
		t.Fatalf("ledger overview: %v (body=%s)", err, rec.Body.String())
	}
	if ov.TotalAccounts != 2 || ov.PartialAccounts != 2 {
		t.Fatalf("accounts = %d partial = %d, want 2/2 (stub 列表对两号都含 first_buddy claimed → 都 partial)", ov.TotalAccounts, ov.PartialAccounts)
	}
	// u1 = 2/5（RichMeow_Chat 台账 + first_buddy 对账）；u2 = 1/5（仅对账）。
	byUID := map[string]int{}
	creditByUID := map[string]int64{}
	for _, acc := range ov.Accounts {
		byUID[acc.UID] = acc.DoneCount
		creditByUID[acc.UID] = acc.Credit
	}
	if byUID["u1"] != 2 || byUID["u2"] != 1 {
		t.Fatalf("done_count u1=%d u2=%d, want 2/1", byUID["u1"], byUID["u2"])
	}
	// 积分收益只有台账条目计入：u1 +50（RichMeow_Chat），u2 0（first_buddy 无台账）。
	if creditByUID["u1"] != 50 || creditByUID["u2"] != 0 {
		t.Fatalf("credit u1=%d u2=%d, want 50/0", creditByUID["u1"], creditByUID["u2"])
	}
	if ov.TotalCredit != 50 || ov.TotalEnergy != 5 {
		t.Fatalf("total credit/energy = %d/%d, want 50/5", ov.TotalCredit, ov.TotalEnergy)
	}

	// 缓存行为：第二次查询命中缓存（上游 listCalls 不再增长）。
	before := atomic.LoadInt64(&stub.listCalls)
	req2 := httptest.NewRequest(http.MethodGet, "/admin/growth/ledger", nil)
	req2.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec2 := httptest.NewRecorder()
	h.mux.ServeHTTP(rec2, req2)
	if atomic.LoadInt64(&stub.listCalls) != before {
		t.Fatalf("second ledger load should hit cache: listCalls %d -> %d", before, atomic.LoadInt64(&stub.listCalls))
	}
	// refresh=1 绕过缓存（重新对账）。
	req3 := httptest.NewRequest(http.MethodGet, "/admin/growth/ledger?refresh=1", nil)
	req3.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec3 := httptest.NewRecorder()
	h.mux.ServeHTTP(rec3, req3)
	if atomic.LoadInt64(&stub.listCalls) == before {
		t.Fatal("refresh=1 should bypass cache")
	}
	// 领奖 → 缓存失效 → 下次查询重新对账。
	h.ledgerReconCache.set(nil, nil) // 预填一个假缓存
	h.ledgerReconCache.invalidate()
	if _, ok := h.ledgerReconCache.get(nil); ok {
		t.Fatal("invalidate should drop cache")
	}
	// 落盘验证：文件含 u1 的 RichMeow_Chat 条目（时间 + 分值）。
	raw, err := os.ReadFile(h.growthLedger.path)
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Accounts []struct {
			UID    string `json:"uid"`
			Claims []struct {
				TaskCode string `json:"task_code"`
				Credit   int64  `json:"credit"`
				At       string `json:"at"`
			} `json:"claims"`
		} `json:"accounts"`
	}
	if json.Unmarshal(raw, &stored) != nil || len(stored.Accounts) != 1 {
		t.Fatalf("ledger file = %s", raw)
	}
	c0 := stored.Accounts[0]
	if c0.UID != "u1" || len(c0.Claims) != 1 {
		t.Fatalf("ledger accounts = %+v", stored.Accounts)
	}
	cl := c0.Claims[0]
	if cl.TaskCode != "RichMeow_Chat" || cl.Credit != 50 || cl.At == "" {
		t.Fatalf("ledger claims = %+v", c0.Claims)
	}
}

func TestGrowthJobMarkLifecycleAndResume(t *testing.T) {
	fastGrowthActions(t)
	stub := &growthStub{tasks: growthTasksSample, claimCredit: 50}
	markPath := filepath.Join(t.TempDir(), "growth-job.json")
	h, _ := newGrowthHandler(t, stub,
		&auth.Auth{UID: "u1", AccessToken: "at"},
		&auth.Auth{UID: "u2", AccessToken: "at"},
	)
	h.cfg.GrowthJobMarkPath = markPath

	// 1) 正常批跑：跑完标记应被删除。
	req := httptest.NewRequest(http.MethodPost, "/admin/tasks/auto_all", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	jobID, _ := out["job_id"].(string)
	growthWaitJob(t, h, jobID, 5*time.Second)
	if _, err := os.Stat(markPath); !os.IsNotExist(err) {
		t.Fatalf("mark file should be removed after job done: %v", err)
	}

	// 2) 模拟中断：手写半路标记（u1 已跑完，剩 u2），重启恢复只跑 u2。
	mark := `{"mode":"all","pending":["u2"],"started":"2026-09-19T01:00:00+08:00"}`
	if err := os.WriteFile(markPath, []byte(mark), 0600); err != nil {
		t.Fatal(err)
	}
	h.ResumeGrowthJobsAfterRestart()
	// 恢复 job 异步起跑（5s 延迟在测试里等不起——轮询窗口放宽）。
	var snap map[string]any
	deadline := time.Now().Add(12 * time.Second)
	for {
		req := httptest.NewRequest(http.MethodGet, "/admin/growth/jobs", nil)
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		rec := httptest.NewRecorder()
		h.mux.ServeHTTP(rec, req)
		var list struct {
			Jobs []map[string]any `json:"jobs"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &list)
		if len(list.Jobs) > 0 {
			snap = list.Jobs[len(list.Jobs)-1]
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("resume job did not start within 12s")
		}
		time.Sleep(200 * time.Millisecond)
	}
	// 等 job 跑完：结果只含 u2 的条目，且标记文件被清。
	resumeID, _ := snap["id"].(string)
	snap = growthWaitJob(t, h, resumeID, 15*time.Second)
	if snap["phase"] != "done" {
		t.Fatalf("resume phase = %v, want done", snap["phase"])
	}
	results, _ := snap["results"].([]any)
	for _, raw := range results {
		item, _ := raw.(map[string]any)
		if item["uid"] != "u2" {
			t.Fatalf("resume job should only run pending u2, got %v", item["uid"])
		}
	}
	if len(results) != 2 { // u2 的 RichMeow_Chat + first_buddy
		t.Fatalf("resume results = %d, want 2", len(results))
	}
	if _, err := os.Stat(markPath); !os.IsNotExist(err) {
		raw, _ := os.ReadFile(markPath)
		t.Fatalf("mark file should be removed after resumed job done: %s", raw)
	}
}

func TestAccountTaskAutoLocksPerAccount(t *testing.T) {
	fastGrowthActions(t)
	stub := &growthStub{tasks: growthTasksSample}
	// 恢复轮询等待让第一个请求"慢"，制造并发窗口。
	claimPollAttempts, claimPollGap = 3, 50*time.Millisecond
	t.Cleanup(func() { claimPollAttempts, claimPollGap = 1, 0 })
	h, _ := newGrowthHandler(t, stub, &auth.Auth{UID: "u1", AccessToken: "at"})
	done := make(chan int, 2)
	go func() {
		code, _ := growthPostAccount(t, h, "u1", "/tasks/auto", `{"task_code":"RichMeow_Chat"}`)
		done <- code
	}()
	time.Sleep(10 * time.Millisecond) // 让第一个请求先拿锁
	code, _ := growthPostAccount(t, h, "u1", "/tasks/auto", `{"task_code":"RichMeow_Chat"}`)
	if code != http.StatusConflict {
		t.Fatalf("concurrent status = %d, want 409", code)
	}
	if c := <-done; c != 200 {
		t.Fatalf("first request status = %d, want 200", c)
	}
}

func TestGrowthEndpointsRejectGlobalAndMissing(t *testing.T) {
	stub := &growthStub{tasks: growthTasksSample}
	h, _ := newGrowthHandler(t, stub,
		&auth.Auth{UID: "cn1", AccessToken: "at", Domain: "copilot.tencent.com"},
		&auth.Auth{UID: "g1", AccessToken: "at", Domain: "www.workbuddy.ai"},
	)
	code, _ := growthPostAccount(t, h, "nobody", "/tasks/auto", `{"task_code":"RichMeow_Chat"}`)
	if code != http.StatusNotFound {
		t.Fatalf("missing account status = %d, want 404", code)
	}
	code, _ = growthPostAccount(t, h, "g1", "/tasks/auto", `{"task_code":"RichMeow_Chat"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("global account status = %d, want 400", code)
	}
}

func TestAcceptAllSkipsAcceptedAndClaimed(t *testing.T) {
	fastGrowthActions(t)
	stub := &growthStub{tasks: growthTasksSample}
	h, _ := newGrowthHandler(t, stub, &auth.Auth{UID: "u1", AccessToken: "at"})
	code, out := growthPostAccount(t, h, "u1", "/tasks/accept_all", `{}`)
	if code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	// 唯一 not_accepted 的 Philanthropy 已移除，不能再接受。
	if n, _ := out["accepted"].(float64); n != 0 {
		t.Fatalf("accepted = %v, want 0", out["accepted"])
	}
}

func TestClaimEndpointReportsAlreadyClaimed(t *testing.T) {
	stub := &growthStub{tasks: growthTasksSample, claimCredit: 0, claimEnergy: 0}
	h, _ := newGrowthHandler(t, stub, &auth.Auth{UID: "u1", AccessToken: "at"})
	code, out := growthPostAccount(t, h, "u1", "/tasks/claim", `{"task_code":"RichMeow_Chat"}`)
	if code != 200 || out["already_claimed"] != true {
		t.Fatalf("zero reward should map to already_claimed: code=%d out=%v", code, out)
	}
}

func TestRetiredGrowthTasksRejectedBeforeQueueOrUpstream(t *testing.T) {
	retired := []string{"chat_5", "Model_chat_GLM5.2", "Buddy_App", "Buddy_App_QQ", "automation_1", "template_5", "playbook_prompt", "create_canvas", "expert_5", "Expert_team_use_3", "Hp_Appearance", "skill_1", "Expert_lighthouse", "black_cat", "Expert_Philanthropy", "unknown"}
	for _, code := range retired {
		t.Run(code, func(t *testing.T) {
			stub := &growthStub{tasks: growthTasksSample}
			h, _ := newGrowthHandler(t, stub, &auth.Auth{UID: "u1", AccessToken: "fake"})
			for _, tc := range []struct {
				path, body string
				status     int
			}{
				{"/tasks/accept", fmt.Sprintf(`{"task_codes":[%q]}`, code), http.StatusBadRequest},
				{"/tasks/accept", fmt.Sprintf(`{"task_codes":["first_buddy",%q]}`, code), http.StatusBadRequest},
				{"/tasks/claim", fmt.Sprintf(`{"task_code":%q}`, code), http.StatusBadRequest},
				{"/tasks/auto", fmt.Sprintf(`{"task_code":%q}`, code), http.StatusNotImplemented},
			} {
				status, _ := growthPostAccount(t, h, "u1", tc.path, tc.body)
				if status != tc.status {
					t.Fatalf("%s status=%d want=%d", tc.path, status, tc.status)
				}
			}
			if len(h.growthJobs.active()) != 0 || stub.listCalls != 0 || stub.reportCalls != 0 || stub.acceptCalls != 0 || stub.claimCalls != 0 {
				t.Fatal("retired task queued work or reached upstream")
			}
		})
	}
}

type growthSnapshotFixture struct{ records map[string][]byte }

func (s growthSnapshotFixture) CollectionLoad() (map[string][]byte, error) {
	return s.records, nil
}
func (s growthSnapshotFixture) CollectionSave(map[string][]byte, []string) error { return nil }

func TestAccountTasksFiltersRetiredCollectionSnapshot(t *testing.T) {
	stub := &growthStub{tasks: `[]`}
	h, _ := newGrowthHandler(t, stub, &auth.Auth{UID: "u1", AccessToken: "fake"})
	tasks := []upstream.Task{{TaskCode: "chat_5"}, {TaskCode: "Expert_Philanthropy"}}
	for _, code := range upstream.CurrentGrowthTaskCodes() {
		tasks = append(tasks, upstream.Task{TaskCode: code})
	}
	data, _ := json.Marshal(tasks)
	raw, _ := json.Marshal(collection.Snapshot{Target: collection.Target{UID: "u1", Resource: "tasks"}, Data: data})
	m, err := collection.New(collection.Options{
		Store:       growthSnapshotFixture{map[string][]byte{"snapshot/u1/tasks": raw}},
		StartPaused: true,
		Run: func(context.Context, collection.Target, collection.Snapshot) (json.RawMessage, error) {
			t.Error("snapshot read started online collection")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	h.cfg.Collection = m
	r := httptest.NewRequest(http.MethodGet, "/admin/account/u1/tasks", nil)
	r.Header.Set("Authorization", "Bearer "+testAPIKey)
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, r)
	var result struct {
		Tasks       []upstream.Task `json:"tasks"`
		AutoActions map[string]bool `json:"auto_actions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 {
		t.Fatalf("snapshot response: status=%d err=%v", w.Code, err)
	}
	if len(result.Tasks) != 5 || len(result.AutoActions) != 3 || stub.listCalls != 0 {
		t.Fatalf("tasks=%v actions=%v calls=%d", result.Tasks, result.AutoActions, stub.listCalls)
	}
	for _, task := range result.Tasks {
		if !upstream.IsCurrentGrowthTask(task.TaskCode) {
			t.Fatalf("retired cached task displayed: %s", task.TaskCode)
		}
	}
	for _, code := range []string{"wb_wechat_oa_subscribe_task", "skill_share"} {
		if result.AutoActions[code] {
			t.Fatalf("new task gained unverified automation: %s", code)
		}
	}
}

func TestFirstBuddyDoesNotAcceptOrClaimRetiredPrerequisite(t *testing.T) {
	fastGrowthActions(t)
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/v2/report", "/activity/growth/buddy/agreement", "/activity/growth/buddy/first":
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		default:
			t.Errorf("unexpected prerequisite request %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	h := &Handler{cfg: Config{Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}}}
	if _, err := runFirstBuddy(h, &auth.Auth{UID: "fixture", AccessToken: "fake"}); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 3 {
		t.Fatalf("prerequisite requests=%v", paths)
	}
}

func TestAcceptAllRestrictsOldUpstreamListToCurrentFiveTasks(t *testing.T) {
	fastGrowthActions(t)
	tasks := []upstream.Task{{TaskCode: "chat_5", AcceptStatus: "not_accepted"}, {TaskCode: "Expert_Philanthropy", AcceptStatus: "not_accepted"}}
	for _, code := range upstream.CurrentGrowthTaskCodes() {
		tasks = append(tasks, upstream.Task{TaskCode: code, AcceptStatus: "not_accepted"})
	}
	raw, _ := json.Marshal(tasks)
	h, _ := newGrowthHandler(t, &growthStub{tasks: string(raw)}, &auth.Auth{UID: "u1", AccessToken: "fake"})
	var accepted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tasks") {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"tasks": tasks}})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/accept") {
			var body struct {
				TaskCodes []string `json:"task_codes"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			accepted = append(accepted, body.TaskCodes...)
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
			return
		}
		t.Errorf("unexpected request %s", r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	h.cfg.Upstream.ChatBaseCN = srv.URL
	status, out := growthPostAccount(t, h, "u1", "/tasks/accept_all", `{}`)
	if status != http.StatusOK || out["accepted"] != float64(5) || len(accepted) != 5 {
		t.Fatalf("status=%d result=%v accepted=%v", status, out, accepted)
	}
	for _, code := range accepted {
		if !upstream.IsCurrentGrowthTask(code) {
			t.Fatalf("retired task accepted: %s", code)
		}
	}
}

func TestNewCurrentTasksAllowManualActionsWithoutAutomation(t *testing.T) {
	for _, code := range []string{"wb_wechat_oa_subscribe_task", "skill_share"} {
		t.Run(code, func(t *testing.T) {
			stub := &growthStub{tasks: `[]`}
			h, _ := newGrowthHandler(t, stub, &auth.Auth{UID: "u1", AccessToken: "fake"})
			status, _ := growthPostAccount(t, h, "u1", "/tasks/accept", fmt.Sprintf(`{"task_codes":[%q]}`, code))
			if status != http.StatusOK {
				t.Fatalf("manual accept status=%d", status)
			}
			status, _ = growthPostAccount(t, h, "u1", "/tasks/claim", fmt.Sprintf(`{"task_code":%q}`, code))
			if status != http.StatusOK {
				t.Fatalf("manual claim status=%d", status)
			}
			status, _ = growthPostAccount(t, h, "u1", "/tasks/auto", fmt.Sprintf(`{"task_code":%q}`, code))
			if status != http.StatusNotImplemented || stub.acceptCalls != 1 || stub.claimCalls != 1 || stub.reportCalls != 0 {
				t.Fatalf("new task gained automation: status=%d calls=%d/%d/%d", status, stub.acceptCalls, stub.claimCalls, stub.reportCalls)
			}
		})
	}
}
