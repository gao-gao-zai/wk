// growthrunner_test.go 执行池行为测试：单 worker 串行、入队去重、
// all job 在最后一个账号跑完时自动 finish（多 job 交错各收各的）。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
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

	job := h.growthJobs.newJob("", "all")
	if n := h.growthSubmitAll(job, []*auth.Auth{a1, a2}); n != 2 {
		t.Fatalf("enqueued = %d want 2", n)
	}
	// 重复入队同一账号：去重应拒绝。
	job2 := h.growthJobs.newJob("u1", "account")
	if h.growthSubmitAccount(job2, a1) {
		t.Fatal("重复入队同一账号应返回 false（inFlight 去重）")
	}

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
