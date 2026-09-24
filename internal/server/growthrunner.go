// growthrunner.go 成长任务执行池：全局单队列 + 单 worker，所有触发源共用。
//
// 为什么：此前手动全池（all）、手动单账号（account）、自动加号钩子、重启恢复
// 四条路径各自起 goroutine 直跑，靠 per-account TryLock 互斥——并发时批跑会把
// 持锁账号记为「跳过」，既丢统计又不重试。收敛为单一执行队列后：
//
//   - 所有触发源 enqueue(account, job)：串行消费，天然不并发、不撞锁、不撞风控
//   - 每个账号跑完，其归属 job 的剩余计数递减，归零自动 finish——多个 job 的
//     账号在队列里交错也正确（每个 job 独立计数）
//   - 标记文件 = 队列实时快照（worker 每取走一个账号重写 Pending），重启恢复
//     语义不变：恢复 = 把残留 Pending 重新入队
//   - 单账号 job（mode=account）total = 任务项数（runGrowthAllWithJob 语义）；
//     all job total = 账号数（accountDone 语义）。两种粒度并存，前端按 mode 渲染。
//
// 代价：全池 85 号从「可并行」变严格串行——但 expert 链本就含真实对话与全局
// 节流（6s/条），并行收益极小且放大风控风险，串行是正确取舍。
package server

import (
	"log"
	"sync"

	"workbuddy2api/internal/auth"
)

// growthRunner 成长任务执行池。
type growthRunner struct {
	mu      sync.Mutex
	queue   []accountJobType
	running bool
	// inFlight 队列内去重：同一账号已在队列/执行中时不重复入队（返回 false）。
	inFlight map[string]bool
}

// accountJobType 队列的单账号条目：uid + auth + 执行回调。run 由触发方注入
// ——all 模式跑完 accountDone；account 模式跑 runGrowthAllWithJob。
type accountJobType struct {
	uid string
	a   *auth.Auth
	run func()
}

// enqueue 提交一个账号到执行池。返回 false = 该账号已在队列或执行中
// （调用方据此反馈「排队中/跳过」）。job 的 total/done 由各触发方按各自
// 粒度设置，worker 跑完一个账号后调用 job.accountDone()（all 模式）或
// 由 runGrowthAllWithJob 自己推进（account 模式 finishItem）。
func (g *growthRunner) enqueue(aj accountJobType) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inFlight == nil {
		g.inFlight = make(map[string]bool)
	}
	if g.inFlight[aj.uid] {
		return false
	}
	g.inFlight[aj.uid] = true
	g.queue = append(g.queue, aj)
	if !g.running {
		g.running = true
		go g.drain()
	}
	return true
}

// drain 单 worker 消费循环：逐账号执行，队列空时退出（下次 enqueue 重新拉起）。
func (g *growthRunner) drain() {
	for {
		g.mu.Lock()
		if len(g.queue) == 0 {
			g.running = false
			g.mu.Unlock()
			return
		}
		aj := g.queue[0]
		g.queue = g.queue[1:]
		g.mu.Unlock()

		aj.run()

		g.mu.Lock()
		delete(g.inFlight, aj.uid)
		g.mu.Unlock()
	}
}

// pending 队列实时快照（标记文件用，顺序保留）。
func (g *growthRunner) pending() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.queue))
	for _, aj := range g.queue {
		out = append(out, aj.uid)
	}
	return out
}

// ---------------------------------------------------------------------------
// Handler 侧封装
// ---------------------------------------------------------------------------

// growthSubmitAll all 模式（手动全池/重启恢复）：全部账号入池，跑完 accountDone。
// job 完成收尾（finish）在最后一个账号跑完时自动执行（remaining 归零）。
func (h *Handler) growthSubmitAll(job *growthJob, accounts []*auth.Auth) int {
	enqueued := 0
	for _, a := range accounts {
		uid := a.Snapshot().UID
		aj := accountJobType{uid: uid, a: a}
		aj.run = func() {
			job.noteCurrent("账号 " + uid)
			// 账号级预跳过：对账缓存（TTL 内）显示该号全部可自动任务已领
			// → 不打上游 ListTasks，直接出结果（批跑重跑已完成号零成本）。
			if h.growthAccountAllClaimed(uid) {
				job.appendResult(growthJobItem{UID: uid, OK: true, Skipped: true, Message: "全部任务已领取（缓存快照），跳过"})
				job.accountDone()
				h.syncGrowthMark()
				if job.accountFinished() {
					job.finish("done", "")
					log.Printf("growth: 全部账号一键完成 job=%s", job.ID)
				}
				return
			}
			items, ok := h.runGrowthAllCollect(a, func(code, desc string) {
				job.noteCurrent(uid + "｜" + code)
			})
			if !ok {
				job.finishItem(growthJobItem{UID: uid, OK: false, Message: "任务列表拉取失败"})
			} else {
				for _, item := range items {
					item.UID = uid
					job.appendResult(item)
				}
			}
			job.accountDone()
			h.syncGrowthMark()
			// 本 job 全部账号跑完 → 收尾（多个 job 交错时各收各的）。
			if job.accountFinished() {
				job.finish("done", "")
				log.Printf("growth: 全部账号一键完成 job=%s", job.ID)
			}
		}
		if h.growthRunner.enqueue(aj) {
			enqueued++
		}
	}
	job.setTotal(enqueued)
	if enqueued == 0 {
		// 全部账号被去重拒绝（已在其它 job 的队列里）：本 job 直接收尾，
		// 不留 running 空壳（TTL 前一直显示"执行中"误导前端）。
		job.finish("done", "全部账号已在执行队列中（与进行中的批跑重复）")
		log.Printf("growth: 批跑 job=%s 无新账号（均已入队），直接完成", job.ID)
	}
	return enqueued
}

// growthAccountAllClaimed 对账缓存快照显示该账号全部可自动任务已领奖。
// 未命中缓存 / 有未领任务 → false（走正常流水线，内含任务级跳过）。
// 快照只用于「跳过」决策：全已领才跳——部分完成、任何一项未领都不跳，
// 宁可多打一次 ListTasks 也不漏跑（跳过条件从严）。
func (h *Handler) growthAccountAllClaimed(uid string) bool {
	ts, ok := h.ledgerReconCache.tasksForUID(uid)
	if !ok || len(ts) == 0 {
		return false
	}
	auto := map[string]bool{}
	for i := range growthActions {
		auto[growthActions[i].TaskCode] = true
	}
	for _, t := range ts {
		if auto[t.TaskCode] && !t.Claimed {
			return false // 有可自动任务未领
		}
	}
	return true
}

// growthSubmitAccount account 模式（手动单账号/自动加号）：入池跑全量流水线。
func (h *Handler) growthSubmitAccount(job *growthJob, a *auth.Auth) bool {
	uid := a.Snapshot().UID
	aj := accountJobType{uid: uid, a: a}
	aj.run = func() {
		h.runGrowthAllWithJob(a, job)
	}
	return h.growthRunner.enqueue(aj)
}

// syncGrowthMark 队列快照落盘（all 模式 worker 每账号跑完调用）。
// 队列空（全部跑完）→ 清标记；非空 → 写剩余（重启只恢复没跑的）。
//
// 注意：入队方（growthSubmitAll 调用侧）不做入队后补写——drain 的本调用
// 是标记文件的唯一同步点。若入队方也写，会与 drain 的 clear/write 竞争
// （实测：u2 瞬间跑完 clear 后，入队方的滞后 write 会复活已删的标记）。
// 入队前的初始快照由调用方在 SubmitAll 之前写入（此时 drain 必然没跑）。
func (h *Handler) syncGrowthMark() {
	pending := h.growthRunner.pending()
	if len(pending) == 0 {
		h.clearGrowthJobMark()
		return
	}
	h.writeGrowthJobMark(pending)
}
