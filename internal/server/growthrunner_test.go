// growthrunner_test.go 执行池行为测试：单 worker 串行、入队去重、
// all job 在最后一个账号跑完时自动 finish（多 job 交错各收各的）。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/growthlimit"
	"workbuddy2api/internal/lifecycle"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// runnerTasksStub 全部任务已领取：流水线秒级空转（只测池行为，不测任务逻辑）。
// claimed 的真实口径是 accept_status == "claimed"（ListTasks 解析时推导）。
var runnerTasksStub = `[{"task_code":"chat_5","accept_status":"claimed","current":5,"target":5},
	{"task_code":"first_buddy","accept_status":"claimed","current":1,"target":1}]`

// TestGrowthRunnerSerializesAndFinishes 三个验证点：
//  1. 两个账号提交 → all job 正常跑完（done=total，phase=done）
//  2. 重复提交同一账号 → enqueue 拒绝（inFlight 去重）
//  3. 执行池串行（单 worker；runGrowthAllCollect 不会并发进入——
//     由 growthRunner.drain 的单 goroutine 结构保证）
func TestGrowthRunnerSerializesAndFinishes(t *testing.T) {
	fastGrowthActions(t)
	stub := &growthStub{tasks: runnerTasksStub}
	h, _ := newGrowthHandler(t, stub,
		&auth.Auth{UID: "u1", AccessToken: "at"},
		&auth.Auth{UID: "u2", AccessToken: "at"},
	)
	a1 := h.cfg.Pool.AuthByUID("u1")
	a2 := h.cfg.Pool.AuthByUID("u2")
	// Hold the legacy lock until duplicate admission has been checked; a fast
	// fake upstream must not finish u1 before this assertion gets scheduled.
	if !h.growthLocks.tryLock("u1") {
		t.Fatal("initial account lock unavailable")
	}
	locked := true
	defer func() {
		if locked {
			h.growthLocks.unlock("u1")
		}
	}()

	job := h.growthJobs.newJob("", "all")
	if n := h.growthSubmitAll(job, []*auth.Auth{a1, a2}); n != 2 {
		t.Fatalf("enqueued = %d want 2", n)
	}
	// 重复入队同一账号：去重应拒绝。
	job2 := h.growthJobs.newJob("u1", "account")
	if h.growthSubmitAccount(job2, a1) {
		t.Fatal("重复入队同一账号应返回 false（inFlight 去重）")
	}
	h.growthLocks.unlock("u1")
	locked = false

	deadline := time.Now().Add(10 * time.Second)
	for {
		job.mu.Lock()
		phase, done, total := job.phase, job.done, job.total
		job.mu.Unlock()
		if phase == "done" || time.Now().After(deadline) {
			if phase != "done" || done != total || total != 2 {
				t.Fatalf("job phase=%s done=%d total=%d want done/2", phase, done, total)
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestGrowthRunnerTwoJobsInterleave 两个 all job 交错入队：各自独立计数、
// 各自 finish（u1 归 jobA、u2 归 jobB——不同账号避开 inFlight 去重干扰）。
func TestGrowthRunnerTwoJobsInterleave(t *testing.T) {
	fastGrowthActions(t)
	stub := &growthStub{tasks: runnerTasksStub}
	h, _ := newGrowthHandler(t, stub,
		&auth.Auth{UID: "u1", AccessToken: "at"},
		&auth.Auth{UID: "u2", AccessToken: "at"},
	)
	a1 := h.cfg.Pool.AuthByUID("u1")
	a2 := h.cfg.Pool.AuthByUID("u2")

	jobA := h.growthJobs.newJob("", "all")
	jobB := h.growthJobs.newJob("", "all")
	h.growthSubmitAll(jobA, []*auth.Auth{a1})
	h.growthSubmitAll(jobB, []*auth.Auth{a2})

	waitFinish := func(j *growthJob, label string) {
		deadline := time.Now().Add(10 * time.Second)
		for {
			j.mu.Lock()
			phase := j.phase
			j.mu.Unlock()
			if phase == "done" || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		j.mu.Lock()
		defer j.mu.Unlock()
		if j.phase != "done" {
			t.Fatalf("%s phase=%s want done", label, j.phase)
		}
	}
	waitFinish(jobA, "jobA")
	waitFinish(jobB, "jobB")
}

// TestGrowthRunnerMarkSnapshotQueue 标记文件 = 队列实时快照：
// 全部跑完 → 清除（重启恢复语义：恢复 = 残留 Pending 重新入队）。
func TestGrowthRunnerMarkSnapshotQueue(t *testing.T) {
	fastGrowthActions(t)
	stub := &growthStub{tasks: runnerTasksStub}
	markPath := filepath.Join(t.TempDir(), "growth-job.json")
	h, _ := newGrowthHandler(t, stub,
		&auth.Auth{UID: "u1", AccessToken: "at"},
		&auth.Auth{UID: "u2", AccessToken: "at"},
	)
	h.cfg.GrowthJobMarkPath = markPath

	req := httptest.NewRequest(http.MethodPost, "/admin/tasks/auto_all", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	jobID, _ := out["job_id"].(string)
	growthWaitJob(t, h, jobID, 5*time.Second)

	if _, err := os.Stat(markPath); !os.IsNotExist(err) {
		t.Fatalf("mark file should be removed after queue drained: %v", err)
	}
}

// TestGrowthRunnerPrefetchSkipsAllClaimed 账号级预跳过：对账缓存显示某号
// 全部可自动任务已领 → 批跑该号零上游 ListTasks 调用（秒级出结果）。
func TestGrowthRunnerPrefetchSkipsAllClaimed(t *testing.T) {
	fastGrowthActions(t)
	// stub 列表：chat_5/first_buddy 均已领 + 一个不在 growthActions 的任务。
	stub := &growthStub{tasks: runnerTasksStub}
	h, _ := newGrowthHandler(t, stub,
		&auth.Auth{UID: "u1", AccessToken: "at"},
	)
	a1 := h.cfg.Pool.AuthByUID("u1")

	// 预填对账缓存：u1 快照 = stub 列表（全已领）。
	accounts := []*auth.Auth{a1}
	results := make([][]upstream.Task, 1)
	ts, err := h.cfg.Upstream.ListTasks(a1)
	if err != nil {
		t.Fatal(err)
	}
	results[0] = ts
	h.ledgerReconCache.set(accounts, results)

	before := atomic.LoadInt64(&stub.listCalls)
	job := h.growthJobs.newJob("", "all")
	if n := h.growthSubmitAll(job, accounts); n != 1 {
		t.Fatalf("enqueued = %d want 1", n)
	}
	snap := growthWaitJob(t, h, job.ID, 5*time.Second)
	if snap["phase"] != "done" {
		t.Fatalf("phase = %v want done", snap["phase"])
	}
	// 预跳过生效：该号不再打 ListTasks。
	if got := atomic.LoadInt64(&stub.listCalls); got != before {
		t.Fatalf("listCalls %d -> %d（预跳过应零上游调用）", before, got)
	}
	// 结果含跳过标记。
	results2, _ := snap["results"].([]any)
	if len(results2) != 1 {
		t.Fatalf("results = %d want 1", len(results2))
	}
	item, _ := results2[0].(map[string]any)
	if item["skipped"] != true || item["uid"] != "u1" {
		t.Fatalf("skip item = %v", item)
	}
}

// TestGrowthRunnerPrefetchPartialNotSkipped 预跳过条件从严：快照显示还有
// 可自动任务未领 → 不跳，走正常流水线（宁可多打一次 ListTasks 不漏跑）。
func TestGrowthRunnerPrefetchPartialNotSkipped(t *testing.T) {
	fastGrowthActions(t)
	// chat_5 未领（current 3/5）：预跳过应判定不跳。
	stub := &growthStub{tasks: `[{"task_code":"chat_5","accept_status":"accepted","current":3,"target":5},
		{"task_code":"first_buddy","accept_status":"claimed","current":1,"target":1}]`}
	h, _ := newGrowthHandler(t, stub,
		&auth.Auth{UID: "u1", AccessToken: "at"},
	)
	a1 := h.cfg.Pool.AuthByUID("u1")
	accounts := []*auth.Auth{a1}

	results := make([][]upstream.Task, 1)
	ts, err := h.cfg.Upstream.ListTasks(a1)
	if err != nil {
		t.Fatal(err)
	}
	results[0] = ts
	h.ledgerReconCache.set(accounts, results)

	job := h.growthJobs.newJob("", "all")
	h.growthSubmitAll(job, accounts)
	snap := growthWaitJob(t, h, job.ID, 5*time.Second)
	if snap["phase"] != "done" {
		t.Fatalf("phase = %v want done", snap["phase"])
	}
	// 未预跳过：走了正常流水线——chat_5 被执行（部分完成的号不能账号级跳过）。
	// first_buddy 已领 → 任务级 skip（skipped:true）是正常流水线行为。
	results2, _ := snap["results"].([]any)
	chat5Ran := false
	accountSkipped := false
	for _, raw := range results2 {
		item, _ := raw.(map[string]any)
		if item["uid"] != "u1" {
			continue
		}
		if item["task_code"] == "chat_5" {
			chat5Ran = true
			if item["skipped"] == true {
				t.Fatalf("chat_5 未领取却被跳过: %v", item)
			}
		}
	}
	if !chat5Ran {
		t.Fatalf("chat_5 未被执行（账号被误预跳过？）: %v", results2)
	}
	_ = accountSkipped
}

func runnerReceive(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not make progress")
		return ""
	}
}

func TestGrowthRunnerDistinctProxyIPsBypassBlockedHead(t *testing.T) {
	var g growthRunner
	started := make(chan string, 3)
	finished := make(chan string, 3)
	release := make(chan struct{})
	defer close(release)
	entry := func(uid, ip string) accountJobType {
		return accountJobType{uid: uid, resources: []string{ip}, run: func() { started <- uid; <-release }, done: func(bool) { finished <- uid }}
	}
	if got := g.enqueueBatch([]accountJobType{entry("a", "ip1"), entry("b", "ip1"), entry("c", "ip2")}, nil); got != 3 {
		t.Fatal(got)
	}
	first, second := runnerReceive(t, started), runnerReceive(t, started)
	if !((first == "a" && second == "c") || (first == "c" && second == "a")) {
		t.Fatalf("same-IP head blocked different IP: %s %s", first, second)
	}
	if g.enqueue(entry("a", "ip3")) {
		t.Fatal("same UID accepted while active")
	}
	select {
	case got := <-started:
		t.Fatalf("same IP started concurrently: %s", got)
	case <-time.After(40 * time.Millisecond):
	}
	// Let a/c finish; b must then run even though it was skipped by the scan.
	release <- struct{}{}
	release <- struct{}{}
	runnerReceive(t, finished)
	runnerReceive(t, finished)
	if got := runnerReceive(t, started); got != "b" {
		t.Fatal(got)
	}
	release <- struct{}{}
	runnerReceive(t, finished)
}

func TestGrowthRunnerUnknownRouteIsGloballyExclusive(t *testing.T) {
	var g growthRunner
	started := make(chan string, 3)
	finished := make(chan string, 3)
	release := make(chan struct{})
	defer close(release)
	mk := func(uid string, resources []string) accountJobType {
		return accountJobType{uid: uid, resources: resources, run: func() { started <- uid; <-release }, done: func(bool) { finished <- uid }}
	}
	g.enqueue(mk("unknown", nil))
	if runnerReceive(t, started) != "unknown" {
		t.Fatal("unknown did not start")
	}
	g.enqueueBatch([]accountJobType{mk("a", []string{"ip1"}), mk("b", []string{"ip2"})}, nil)
	select {
	case got := <-started:
		t.Fatalf("%s overlapped unknown route", got)
	case <-time.After(40 * time.Millisecond):
	}
	release <- struct{}{}
	runnerReceive(t, finished)
	runnerReceive(t, started)
	runnerReceive(t, started)
	release <- struct{}{}
	release <- struct{}{}
	runnerReceive(t, finished)
	runnerReceive(t, finished)
}

func TestGrowthRunnerBoundedConcurrencyAndSharedDNSAnswers(t *testing.T) {
	g := growthRunner{budget: growthlimit.New(4)}
	started := make(chan string, 8)
	finished := make(chan string, 8)
	release := make(chan struct{})
	defer close(release)
	var entries []accountJobType
	for _, uid := range []string{"a", "b", "c", "d", "e", "f"} {
		uid := uid
		entries = append(entries, accountJobType{uid: uid, resources: []string{uid}, run: func() { started <- uid; <-release }, done: func(bool) { finished <- uid }})
	}
	g.enqueueBatch(entries, nil)
	for range 4 {
		runnerReceive(t, started)
	}
	select {
	case got := <-started:
		t.Fatalf("concurrency bound exceeded by %s", got)
	case <-time.After(40 * time.Millisecond):
	}
	for range 4 {
		release <- struct{}{}
	}
	for range 4 {
		runnerReceive(t, finished)
	}
	runnerReceive(t, started)
	runnerReceive(t, started)
	release <- struct{}{}
	release <- struct{}{}
	runnerReceive(t, finished)
	runnerReceive(t, finished)

	check := growthRunner{active: map[string]*accountJobType{"a": {resources: []string{"ip1", "ip2"}}}}
	if check.canRunLocked(&accountJobType{resources: []string{"ip2", "ip3"}}) {
		t.Error("overlapping DNS answers may not run together")
	}
	if !check.canRunLocked(&accountJobType{resources: []string{"ip4"}}) {
		t.Error("independent IP incorrectly blocked")
	}
}

func TestGrowthRunnerMarkIncludesOtherActiveAccount(t *testing.T) {
	h := &Handler{}
	h.cfg.GrowthJobMarkPath = filepath.Join(t.TempDir(), "growth.json")
	h.initGrowthRunner()
	started := make(chan string, 2)
	done := make(chan string, 2)
	one, two := make(chan struct{}), make(chan struct{})
	defer close(one)
	defer close(two)
	entries := []accountJobType{
		{uid: "a", recoverable: true, resources: []string{"ip1"}, run: func() { started <- "a"; <-one }, done: func(bool) { done <- "a" }},
		{uid: "b", recoverable: true, resources: []string{"ip2"}, run: func() { started <- "b"; <-two }, done: func(bool) { done <- "b" }},
	}
	h.growthRunner.enqueueBatch(entries, nil)
	runnerReceive(t, started)
	runnerReceive(t, started)
	one <- struct{}{}
	if runnerReceive(t, done) != "a" {
		t.Fatal("wrong completed account")
	}
	mark := h.readGrowthJobMark()
	if mark == nil || len(mark.Pending) != 1 || mark.Pending[0] != "b" {
		t.Fatalf("other active account lost from restart marker: %+v", mark)
	}
	two <- struct{}{}
	runnerReceive(t, done)
	if _, err := os.Stat(h.cfg.GrowthJobMarkPath); !os.IsNotExist(err) {
		t.Fatalf("finished marker remains: %v", err)
	}
}

func TestGrowthRunnerBatchTotalBeforeFastCompletion(t *testing.T) {
	var g growthRunner
	const count = 100
	job := (&growthJobRegistry{}).newJob("", "all")
	done := make(chan string, count)
	badTotal := make(chan int, count)
	var entries []accountJobType
	for i := 0; i < count; i++ {
		uid := fmt.Sprintf("u%d", i)
		entries = append(entries, accountJobType{uid: uid, resources: []string{uid}, run: func() {
			job.mu.Lock()
			total := job.total
			job.mu.Unlock()
			if total != count {
				badTotal <- total
			}
		}, done: func(bool) {
			job.accountDone()
			if job.accountFinished() {
				job.finish("done", "")
			}
			done <- uid
		}})
	}
	if n := g.enqueueBatch(entries, job.setTotal); n != count {
		t.Fatal(n)
	}
	for range count {
		runnerReceive(t, done)
	}
	select {
	case got := <-badTotal:
		t.Fatalf("worker observed incomplete batch total %d", got)
	default:
	}
	snap := job.snapshot()
	if snap["done"] != count || snap["total"] != count || snap["phase"] != "done" {
		t.Fatalf("bad completion: %v", snap)
	}
}

func TestGrowthRunnerCancelledQueuedTaskDoesNotExecute(t *testing.T) {
	g := growthRunner{budget: growthlimit.New(1)}
	started := make(chan string, 1)
	done := make(chan string, 2)
	release := make(chan struct{})
	defer close(release)
	g.enqueue(accountJobType{uid: "a", resources: []string{"same"}, run: func() { started <- "a"; <-release }, done: func(bool) { done <- "a" }})
	runnerReceive(t, started)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g.enqueue(accountJobType{uid: "b", ctx: ctx, resources: []string{"same"}, run: func() { t.Error("cancelled callback executed") }, done: func(cancelled bool) {
		if !cancelled {
			t.Error("not marked cancelled")
		}
		done <- "b"
	}})
	cancel()
	if runnerReceive(t, done) != "b" {
		t.Fatal("queued cancellation waited for active task")
	}
	if g.budget.Enter() {
		t.Fatal("queued cancellation released the active account permit")
	}
	pending := g.pending()
	if len(pending) != 1 || pending[0] != "a" {
		t.Fatalf("cancel snapshot %v", pending)
	}
	release <- struct{}{}
	runnerReceive(t, done)
}

func TestGrowthRunnerSingleActionDoesNotRestoreAsWholeAccount(t *testing.T) {
	h := &Handler{}
	h.cfg.Upstream = upstream.New()
	h.cfg.Pool = pool.New("")
	a := &auth.Auth{UID: "single"}
	h.cfg.Pool.Add(a)
	h.cfg.GrowthJobMarkPath = filepath.Join(t.TempDir(), "growth.json")
	job := h.growthJobs.newJob("single", "task")
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !h.growthSubmitTask(ctx, job, a, func(*auth.Auth) { close(started); <-release }) {
		t.Fatal("single task rejected")
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("single task did not start")
	}
	if mark := h.readGrowthJobMark(); mark != nil {
		t.Fatalf("single action would restore as account pipeline: %+v", mark)
	}
	cancel()
	release <- struct{}{}
	deadline := time.Now().Add(3 * time.Second)
	for {
		snap := job.snapshot()
		if snap["phase"] == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancelled job left running: %v", snap)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestGrowthRunnerPreservesRecoveryDuringStartupDelay(t *testing.T) {
	h := &Handler{}
	h.cfg.GrowthJobMarkPath = filepath.Join(t.TempDir(), "growth.json")
	h.initGrowthRunner()
	h.growthRunner.mu.Lock()
	h.growthRunner.restoring = []string{"restored"}
	h.growthRunner.syncLocked()
	h.growthRunner.mu.Unlock()
	// A single-action submission/completion during the startup delay must not
	// erase the recovered account that has yet to enter the runner.
	done := make(chan string, 1)
	h.growthRunner.enqueue(accountJobType{uid: "single", run: func() {}, done: func(bool) { done <- "single" }})
	runnerReceive(t, done)
	mark := h.readGrowthJobMark()
	if mark == nil || len(mark.Pending) != 1 || mark.Pending[0] != "restored" {
		t.Fatalf("startup recovery lost: %+v", mark)
	}
}

func TestGrowthRunnerStartupRecoverySurvivesStopBeforeDelayedAdmission(t *testing.T) {
	h := &Handler{}
	h.cfg.Upstream = upstream.New()
	h.cfg.GrowthJobMarkPath = filepath.Join(t.TempDir(), "growth.json")
	h.initGrowthRunner()
	h.growthRunner.mu.Lock()
	h.growthRunner.restoring = []string{"restored"}
	h.growthRunner.syncLocked()
	h.growthRunner.mu.Unlock()
	h.StopGrowthRunner()
	job := h.growthJobs.newJob("", "all")
	// This is the callback invoked after ResumeGrowthJobsAfterRestart's delay.
	h.submitRestoredGrowthJobs(job, []*auth.Auth{{UID: "restored", AccessToken: "fake"}})
	mark := h.readGrowthJobMark()
	if mark == nil || len(mark.Pending) != 1 || mark.Pending[0] != "restored" {
		t.Fatalf("shutdown erased delayed recovery: %+v", mark)
	}
}

func TestGrowthRunnerRejectsQueuedRemovedReplacedAndRefreshedAuth(t *testing.T) {
	for _, change := range []string{"delete", "replace", "refresh"} {
		t.Run(change, func(t *testing.T) {
			h := &Handler{}
			h.cfg.Pool = pool.New("")
			h.cfg.Upstream = upstream.New()
			blocker := &auth.Auth{UID: "blocker", AccessToken: "fixture"}
			target := &auth.Auth{UID: "target", AccessToken: "old", RefreshToken: "old-r"}
			h.cfg.Pool.Add(blocker)
			h.cfg.Pool.Add(target)
			started, release := make(chan struct{}), make(chan struct{})
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()
			blockJob := h.growthJobs.newJob("blocker", "task")
			h.growthSubmitTask(context.Background(), blockJob, blocker, func(*auth.Auth) { close(started); <-release })
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("blocker did not start")
			}
			var callbacks atomic.Int32
			job := h.growthJobs.newJob("target", "task")
			h.growthSubmitTask(context.Background(), job, target, func(*auth.Auth) { callbacks.Add(1) })
			switch change {
			case "delete":
				if removed, busy := h.cfg.Pool.Remove("target"); !removed || busy {
					t.Fatal("fixture remove failed")
				}
			case "replace":
				h.cfg.Pool.Add(&auth.Auth{UID: "target", AccessToken: "old", RefreshToken: "old-r"})
			case "refresh":
				target.ApplyRefresh("new", "new-r", "", 100)
			}
			close(release)
			released = true
			deadline := time.Now().Add(3 * time.Second)
			for {
				snap := job.snapshot()
				if snap["phase"] == "failed" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("stale queued auth not rejected: %v", snap)
				}
				time.Sleep(time.Millisecond)
			}
			if callbacks.Load() != 0 {
				t.Fatal("stale queued auth executed callback")
			}
		})
	}
}

func TestGrowthRunnerBlockedQueueDoesNotAcquireLeases(t *testing.T) {
	h := &Handler{}
	h.cfg.Pool = pool.New("")
	h.cfg.Upstream = upstream.New()
	var plans, acquires, live, peak atomic.Int32
	h.cfg.Upstream.GrowthRoutePlan = func(_ context.Context, uid, _ string) ([]string, bool, func(context.Context) (*url.URL, func(), error), error) {
		plans.Add(1)
		ip := "192.0.2.1"
		if uid == "independent" {
			ip = "192.0.2.2"
		}
		return []string{ip}, false, func(context.Context) (*url.URL, func(), error) {
			acquires.Add(1)
			n := live.Add(1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			return nil, func() { live.Add(-1) }, nil
		}, nil
	}
	const blocked = 60
	started := make(chan string, blocked+2)
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	submit := func(uid string) {
		a := &auth.Auth{UID: uid}
		h.cfg.Pool.Add(a)
		job := h.growthJobs.newJob(uid, "task")
		if !h.growthSubmitTask(context.Background(), job, a, func(*auth.Auth) { started <- uid; <-release }) {
			t.Fatal("fixture submission failed")
		}
	}
	submit("first")
	if runnerReceive(t, started) != "first" {
		t.Fatal("first account did not start")
	}
	for i := 0; i < blocked; i++ {
		submit(fmt.Sprintf("blocked-%d", i))
	}
	submit("independent")
	if got := runnerReceive(t, started); got != "independent" {
		t.Fatalf("same-IP head blocked independent account: %s", got)
	}
	if plans.Load() < blocked+2 || acquires.Load() != 2 || live.Load() != 2 {
		t.Fatalf("blocked accounts acquired resources: plans=%d acquired=%d live=%d", plans.Load(), acquires.Load(), live.Load())
	}
	close(release)
	released = true
	deadline := time.Now().Add(5 * time.Second)
	for len(h.growthRunner.pending()) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("queue did not drain")
		}
		time.Sleep(time.Millisecond)
	}
	if peak.Load() > 4 || live.Load() != 0 {
		t.Fatalf("lease bound/cleanup failed peak=%d live=%d", peak.Load(), live.Load())
	}
}

func TestGrowthRunnerPauseWaitsOnlyForAdmittedAccountsAndResumes(t *testing.T) {
	gate := &lifecycle.Gate{}
	g := growthRunner{gate: gate}
	started, done := make(chan string, 2), make(chan string, 2)
	release := make(chan struct{})
	defer close(release)
	for _, uid := range []string{"first", "queued"} {
		uid := uid
		g.enqueue(accountJobType{uid: uid, recoverable: true, resources: []string{"same"}, run: func() { started <- uid; <-release }, done: func(bool) { done <- uid }})
	}
	if runnerReceive(t, started) != "first" {
		t.Fatal("first account did not start")
	}
	gate.Pause()
	release <- struct{}{}
	runnerReceive(t, done)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := gate.Wait(ctx); err != nil {
		t.Fatal("paused Wait drained queued accounts instead of only admitted work")
	}
	if pending := g.pending(); len(pending) != 1 || pending[0] != "queued" {
		t.Fatalf("pause lost queued account: %v", pending)
	}
	select {
	case got := <-started:
		t.Fatalf("paused dispatcher started %s", got)
	default:
	}
	gate.Resume()
	if runnerReceive(t, started) != "queued" {
		t.Fatal("resume did not continue queue")
	}
	release <- struct{}{}
	runnerReceive(t, done)
	g.stop()
}

func TestGrowthRunnerShutdownStopsPausedDispatcherPreservingPending(t *testing.T) {
	gate := &lifecycle.Gate{}
	gate.Pause()
	g := growthRunner{gate: gate}
	g.enqueue(accountJobType{uid: "queued", recoverable: true, run: func() { t.Error("shutdown dispatched queued action") }})
	g.stop()
	g.mu.Lock()
	running := g.running
	g.mu.Unlock()
	if running || len(g.pending()) != 1 {
		t.Fatal("shutdown did not stop dispatcher/preserve queue")
	}
	if g.enqueue(accountJobType{uid: "late"}) {
		t.Fatal("stopped runner admitted work")
	}
}
