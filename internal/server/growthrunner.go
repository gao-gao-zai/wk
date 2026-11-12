// growthrunner.go: all growth triggers share account and remote-proxy resources.
package server

import (
	"context"
	"log"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/growthlimit"
	"workbuddy2api/internal/lifecycle"
	"workbuddy2api/internal/upstream"
)

type growthRunner struct {
	budget       *growthlimit.Budget
	mu           sync.Mutex
	queue        []*accountJobType
	all          []*accountJobType // queued and active, in submission order (restart snapshot)
	restoring    []string          // recovered UIDs retained during the startup delay
	active       map[string]*accountJobType
	inFlight     map[string]bool
	running      bool
	wake         chan struct{}
	onChange     func([]string)  // called with mu held: snapshot and disk writes are ordered
	gate         *lifecycle.Gate // nil uses the process gate; tests may isolate admission
	closed       bool
	dispatchDone chan struct{}
}

type accountJobType struct {
	uid string
	a   *auth.Auth
	ctx context.Context
	run func()
	// Only whole-account pipelines may be restored from the UID-only marker.
	// A synchronous single action must never become an all-account pipeline.
	recoverable bool
	// Preparation is outside the runner mutex; a private route is held until
	// completion. A nil resource list is globally exclusive, never a wildcard.
	prepare    func() ([]string, func(), func())
	resources  []string
	prepared   bool
	release    func()
	stopCancel func() bool
	done       func(bool) // true if cancelled before execution
}

func (g *growthRunner) enqueue(aj accountJobType) bool {
	return g.enqueueBatch([]accountJobType{aj}, nil) == 1
}

// Admit the whole batch and set its total before any worker can observe it.
func (g *growthRunner) enqueueBatch(jobs []accountJobType, admitted func(int)) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		if admitted != nil {
			admitted(0)
		}
		return 0
	}
	if g.budget == nil {
		g.budget = growthlimit.New(growthlimit.Default)
	}
	if g.inFlight == nil {
		g.inFlight = map[string]bool{}
	}
	if g.active == nil {
		g.active = map[string]*accountJobType{}
	}
	if g.wake == nil {
		g.wake = make(chan struct{}, 1)
	}
	n := 0
	for _, entry := range jobs {
		if entry.uid == "" || g.inFlight[entry.uid] {
			continue
		}
		aj := new(accountJobType)
		*aj = entry
		if aj.ctx == nil {
			aj.ctx = context.Background()
		}
		aj.stopCancel = context.AfterFunc(aj.ctx, func() {
			g.mu.Lock()
			g.signalLocked()
			g.mu.Unlock()
		})
		if aj.prepare == nil {
			aj.prepared = true
		}
		g.inFlight[aj.uid] = true
		g.queue = append(g.queue, aj)
		g.all = append(g.all, aj)
		n++
	}
	if admitted != nil {
		admitted(n)
	}
	g.syncLocked()
	if n > 0 && !g.running {
		g.running = true
		dispatched := make(chan struct{})
		g.dispatchDone = dispatched
		go func() { defer close(dispatched); g.drain() }()
	}
	g.signalLocked()
	return n
}

func (g *growthRunner) signalLocked() {
	if g.wake != nil {
		select {
		case g.wake <- struct{}{}:
		default:
		}
	}
}

func (g *growthRunner) lifecycleGate() *lifecycle.Gate {
	if g.gate != nil {
		return g.gate
	}
	return &lifecycle.Default
}

func (g *growthRunner) canRunLocked(aj *accountJobType) bool {
	if len(g.active) >= g.budget.Current() {
		return false
	}
	if len(aj.resources) == 0 {
		return len(g.active) == 0
	}
	for _, active := range g.active {
		if len(active.resources) == 0 {
			return false
		}
		for _, a := range active.resources {
			for _, b := range aj.resources {
				if a == b {
					return false
				}
			}
		}
	}
	return true
}

// The dispatcher scans beyond a blocked same-IP head. Preparation is bounded
// to one concurrent route acquisition; task execution follows the live shared budget.
func (g *growthRunner) drain() {
	for {
		g.mu.Lock()
		if g.closed {
			g.running = false
			g.mu.Unlock()
			return
		}
		g.mu.Unlock()
		if paused, _ := g.lifecycleGate().Status(); paused {
			// Queued work is durable but is not an admitted background writer.
			// A reversible pause resumes it; shutdown does not drain the pool.
			time.Sleep(50 * time.Millisecond)
			continue
		}
		g.mu.Lock()
		if len(g.queue) == 0 && len(g.active) == 0 {
			g.running = false
			g.mu.Unlock()
			return
		}
		chosen, prepare := -1, -1
		for i, aj := range g.queue {
			if aj.ctx.Err() != nil {
				chosen = i
				break
			}
			if !aj.prepared {
				if prepare < 0 {
					prepare = i
				}
				continue
			}
			if g.canRunLocked(aj) {
				chosen = i
				break
			}
		}
		if chosen >= 0 {
			done, admitted := g.lifecycleGate().Enter()
			if !admitted {
				g.mu.Unlock()
				continue
			}
			aj := g.queue[chosen]
			if aj.ctx.Err() != nil {
				// Queued cancellation performs cleanup, never account admission.
				// It must complete even while every execution permit is occupied.
				g.queue = append(g.queue[:chosen], g.queue[chosen+1:]...)
				delete(g.inFlight, aj.uid)
				for i, entry := range g.all {
					if entry == aj {
						g.all = append(g.all[:i], g.all[i+1:]...)
						break
					}
				}
				if aj.stopCancel != nil {
					aj.stopCancel()
				}
				g.mu.Unlock()
				if aj.release != nil {
					aj.release()
				}
				g.mu.Lock()
				g.syncLocked()
				if aj.done != nil {
					aj.done(true)
				}
				g.mu.Unlock()
				done()
				continue
			}
			if !g.budget.Enter() {
				done()
				g.mu.Unlock()
				continue
			}
			g.queue = append(g.queue[:chosen], g.queue[chosen+1:]...)
			g.active[aj.uid] = aj
			g.mu.Unlock()
			go func() { defer done(); g.execute(aj) }()
			continue
		}
		if prepare >= 0 && len(g.active) < g.budget.Current() {
			aj := g.queue[prepare]
			g.mu.Unlock()
			resources, run, release := aj.prepare()
			g.mu.Lock()
			aj.resources, aj.run, aj.release, aj.prepared = resources, run, release, true
			g.mu.Unlock()
			continue
		}
		wake := g.wake
		g.mu.Unlock()
		<-wake
	}
}

// Stop the unadmitted dispatcher without canceling already admitted actions.
// Their existing lifecycle reservations cover final writes and lease release.
// Queue markers are retained for restart; a reversible pause uses Gate.Resume.
func (h *Handler) StopGrowthRunner() { h.growthRunner.stop() }

func (h *Handler) setGrowthConcurrency(n int) {
	h.initGrowthRunner()
	g := &h.growthRunner
	g.mu.Lock()
	defer g.mu.Unlock()
	_ = g.budget.Set(n) // Validated and durably persisted before this call.
	g.signalLocked()
}
func (g *growthRunner) stop() {
	g.mu.Lock()
	g.closed = true
	for _, aj := range g.queue {
		if aj.stopCancel != nil {
			aj.stopCancel()
		}
	}
	g.signalLocked()
	done := g.dispatchDone
	g.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (g *growthRunner) execute(aj *accountJobType) {
	cancelled := aj.ctx.Err() != nil
	handled := cancelled
	defer func() {
		if aj.stopCancel != nil {
			aj.stopCancel()
		}
		if aj.release != nil {
			aj.release()
		}
		g.mu.Lock()
		g.budget.Leave()
		delete(g.active, aj.uid)
		delete(g.inFlight, aj.uid)
		// A panic must leave this account in the restart marker. The process
		// may exit immediately after unwinding this worker.
		if handled {
			for i, entry := range g.all {
				if entry == aj {
					g.all = append(g.all[:i], g.all[i+1:]...)
					break
				}
			}
		}
		g.syncLocked()
		if handled && aj.done != nil {
			aj.done(cancelled || aj.ctx.Err() != nil)
		}
		g.signalLocked()
		g.mu.Unlock()
	}()
	if !cancelled && aj.run != nil {
		aj.run()
	}
	handled = true
}

func (g *growthRunner) pendingLocked() []string {
	pending := make([]string, 0, len(g.all))
	for _, aj := range g.all {
		pending = append(pending, aj.uid)
	}
	return pending
}
func (g *growthRunner) pending() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pendingLocked()
}
func (g *growthRunner) syncLocked() {
	if g.onChange != nil {
		pending := make([]string, 0, len(g.all))
		seen := map[string]bool{}
		for _, uid := range g.restoring {
			if !seen[uid] {
				seen[uid] = true
				pending = append(pending, uid)
			}
		}
		for _, aj := range g.all {
			if aj.recoverable && !seen[aj.uid] {
				seen[aj.uid] = true
				pending = append(pending, aj.uid)
			}
		}
		g.onChange(pending)
	}
}

func (h *Handler) initGrowthRunner() {
	g := &h.growthRunner
	g.mu.Lock()
	if g.budget == nil {
		g.budget = h.cfg.GrowthBudget
		if g.budget == nil {
			g.budget = growthlimit.New(growthlimit.Default)
		}
	}
	g.onChange = func(pending []string) {
		if len(pending) == 0 {
			h.clearGrowthJobMark()
		} else {
			h.writeGrowthJobMark(pending)
		}
	}
	g.mu.Unlock()
}

// Shared by full-pool, account, automatic enrollment, restored jobs and a
// synchronous single task. Callbacks receive a frozen auth with a private route.
func (h *Handler) growthAuthGuard(a *auth.Auth) func() error {
	expected := a.Snapshot()
	return func() error {
		if h.cfg.Pool == nil || h.cfg.Pool.AuthByUID(expected.UID) != a || a.Snapshot() != expected {
			return upstream.ErrGrowthAuthStale
		}
		return nil
	}
}

func (h *Handler) growthAccountJob(ctx context.Context, job *growthJob, a *auth.Auth, run func(*auth.Auth), done func(bool)) accountJobType {
	uid := a.Snapshot().UID
	guard := h.growthAuthGuard(a)
	fail := func(err error) {
		job.appendResult(growthJobItem{UID: uid, OK: false, Message: err.Error()})
		if job.Mode != "all" {
			job.finish("failed", err.Error())
		}
	}
	aj := accountJobType{uid: uid, a: a, ctx: ctx, done: done, recoverable: job.Mode != "task"}
	aj.prepare = func() ([]string, func(), func()) {
		plan := h.cfg.Upstream.PlanGrowthRoute(ctx, a, guard)
		return plan.Resources, func() {
			// Every task action observes the same legacy per-account lock.
			// Acquire only for this account; the queue has already deduplicated.
			if ctx.Err() != nil {
				return
			}
			if err := guard(); err != nil {
				fail(err)
				return
			}
			route := plan.Acquire()
			defer route.Release()
			if route.Err != nil {
				fail(route.Err)
				return
			}
			h.growthLocks.mu.Lock()
			if h.growthLocks.locks == nil {
				h.growthLocks.locks = map[string]*sync.Mutex{}
			}
			lock := h.growthLocks.locks[uid]
			if lock == nil {
				lock = &sync.Mutex{}
				h.growthLocks.locks[uid] = lock
			}
			h.growthLocks.mu.Unlock()
			lock.Lock()
			defer lock.Unlock()
			if err := guard(); err != nil {
				fail(err)
				return
			}
			if ctx.Err() == nil {
				run(route.Auth)
			}
		}, nil
	}
	return aj
}

func (h *Handler) growthSubmitAll(job *growthJob, accounts []*auth.Auth) int {
	h.initGrowthRunner()
	jobs := make([]accountJobType, 0, len(accounts))
	for _, a := range accounts {
		if a == nil {
			continue
		}
		uid := a.Snapshot().UID
		aj := h.growthAccountJob(context.Background(), job, a, func(frozen *auth.Auth) {
			job.noteCurrent("账号 " + uid)
			if h.growthAccountAllClaimed(uid) {
				job.appendResult(growthJobItem{UID: uid, OK: true, Skipped: true, Message: "全部任务已领取（缓存快照），跳过"})
				return
			}
			items, ok := h.runGrowthAllCollect(frozen, func(code, desc string) { job.noteCurrent(uid + "｜" + code) })
			if !ok {
				job.appendResult(growthJobItem{UID: uid, OK: false, Message: "任务列表拉取失败"})
			} else {
				for _, item := range items {
					item.UID = uid
					job.appendResult(item)
				}
			}
		}, func(cancelled bool) {
			job.accountDone()
			if job.accountFinished() {
				job.finish("done", "")
				log.Printf("growth: 全部账号一键完成 job=%s", job.ID)
			}
		})
		jobs = append(jobs, aj)
	}
	n := h.growthRunner.enqueueBatch(jobs, job.setTotal)
	if n == 0 {
		job.finish("done", "全部账号已在执行队列中（与进行中的批跑重复）")
	}
	return n
}

func (h *Handler) growthAccountAllClaimed(uid string) bool {
	ts, ok := h.ledgerReconCache.tasksForUID(uid)
	if !ok || len(ts) == 0 {
		return false
	}
	auto := map[string]bool{}
	for i := range growthActions {
		auto[growthActions[i].TaskCode] = true
	}
	found := false
	for _, t := range ts {
		if auto[t.TaskCode] {
			found = true
			if !t.Claimed {
				return false
			}
		}
	}
	return found
}

func (h *Handler) growthSubmitAccount(job *growthJob, a *auth.Auth) bool {
	h.initGrowthRunner()
	return h.growthRunner.enqueue(h.growthAccountJob(context.Background(), job, a, func(frozen *auth.Auth) {
		h.runGrowthAllWithJob(frozen, job)
	}, nil))
}

func (h *Handler) growthSubmitTask(ctx context.Context, job *growthJob, a *auth.Auth, run func(*auth.Auth)) bool {
	h.initGrowthRunner()
	return h.growthRunner.enqueue(h.growthAccountJob(ctx, job, a, run, func(cancelled bool) {
		if cancelled {
			job.finish("cancelled", ctx.Err().Error())
		} else {
			job.mu.Lock()
			running := job.phase == "running"
			job.mu.Unlock()
			if running {
				job.finish("done", "")
			}
		}
	}))
}

// Snapshot and disk write share the runner mutex; a late writer cannot revive
// a completed marker or omit another worker that is still active.
func (h *Handler) syncGrowthMark() {
	h.growthRunner.mu.Lock()
	defer h.growthRunner.mu.Unlock()
	h.growthRunner.syncLocked()
}
