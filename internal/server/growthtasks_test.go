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
	claimCalls int64
	// reportProgress 上报驱动进度：每次 /v2/report 让 chat_5 的 current +1
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
			w.Write([]byte(`{"code":0,"data":{}}`))
		case "/v2/report":
			n := atomic.AddInt64(&s.reportCalls, 1)
			// 上报驱动进度：3 已有 + N 次上报，第 2 次后达标（5/5）。
			// 旧串是上一轮的值（3+n-1），避免替换目标已变导致 no-op。
			if s.reportProgress {
				prev, cur := 3+int(n)-1, 3+int(n)
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
	h := newTestHandler(t, Config{Pool: p, Upstream: up})
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

// 標準任务列表：chat_5 未达标（3/5），first_buddy 已领取。
const growthTasksSample = `[
  {"task_code":"chat_5","title":"发起 5 次对话","reward_credit":100,"target":5,"current":3,"accept_status":"accepted"},
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
	if len(out.Tasks) != 3 {
		t.Fatalf("tasks = %d, want 3", len(out.Tasks))
	}
	if !out.AutoActions["chat_5"] || !out.AutoActions["first_buddy"] {
		t.Fatalf("auto_actions missing growth codes: %v", out.AutoActions)
	}
	if out.AutoActions["Expert_Philanthropy"] {
		t.Fatal("Expert_Philanthropy should not be autoable")
	}
}

func TestAccountTaskAutoRunsAndClaims(t *testing.T) {
	fastGrowthActions(t)
	// reportProgress：上报驱动计分，2 条上报后 5/5 达标 → 自动领奖。
	stub := &growthStub{
		tasks:          growthTasksSample,
		reportProgress: true,
		claimCredit:    100, claimEnergy: 5,
	}
	h, _ := newGrowthHandler(t, stub, &auth.Auth{UID: "u1", AccessToken: "at"})
	code, out := growthPostAccount(t, h, "u1", "/tasks/auto", `{"task_code":"chat_5"}`)
	if code != 200 {
		t.Fatalf("status = %d, want 200, body=%v", code, out)
	}
	if out["claimed"] != true {
		t.Fatalf("claimed = %v, want true (out=%v)", out["claimed"], out)
	}
	if stub.reportCalls != 2 { // 5 目标 - 3 已有 = 2 条上报
		t.Fatalf("report calls = %d, want 2", stub.reportCalls)
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
	// 报名 1 项 + chat_5 + first_buddy（Philanthropy 无动作）。
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3 (got %v)", len(results), results)
	}
	// 依赖序：accept → chat_5（first_buddy 已 claimed 排其后跳过）。
	first, _ := results[1].(map[string]any)
	if first["task_code"] != "chat_5" {
		t.Fatalf("first action should be chat_5 (dependency order), got %v", first["task_code"])
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
	// 每账号 2 项任务（chat_5 + first_buddy）× 2 账号 = 4（all 模式无报名条目）。
	results, _ := snap["results"].([]any)
	if len(results) != 4 {
		t.Fatalf("results = %d, want 4 (got %v)", len(results), results)
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
	if len(results) != 2 { // u2 的 chat_5 + first_buddy
		t.Fatalf("resume results = %d, want 2", len(results))
	}
	if _, err := os.Stat(markPath); !os.IsNotExist(err) {
		t.Fatal("mark file should be removed after resumed job done")
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
		code, _ := growthPostAccount(t, h, "u1", "/tasks/auto", `{"task_code":"chat_5"}`)
		done <- code
	}()
	time.Sleep(10 * time.Millisecond) // 让第一个请求先拿锁
	code, _ := growthPostAccount(t, h, "u1", "/tasks/auto", `{"task_code":"chat_5"}`)
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
	code, _ := growthPostAccount(t, h, "nobody", "/tasks/auto", `{"task_code":"chat_5"}`)
	if code != http.StatusNotFound {
		t.Fatalf("missing account status = %d, want 404", code)
	}
	code, _ = growthPostAccount(t, h, "g1", "/tasks/auto", `{"task_code":"chat_5"}`)
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
	// 只有 Philanthropy 是 not_accepted；chat_5 accepted、first_buddy claimed。
	if n, _ := out["accepted"].(float64); n != 1 {
		t.Fatalf("accepted = %v, want 1", out["accepted"])
	}
}

func TestClaimEndpointReportsAlreadyClaimed(t *testing.T) {
	stub := &growthStub{tasks: growthTasksSample, claimCredit: 0, claimEnergy: 0}
	h, _ := newGrowthHandler(t, stub, &auth.Auth{UID: "u1", AccessToken: "at"})
	code, out := growthPostAccount(t, h, "u1", "/tasks/claim", `{"task_code":"chat_5"}`)
	if code != 200 || out["already_claimed"] != true {
		t.Fatalf("zero reward should map to already_claimed: code=%d out=%v", code, out)
	}
}
