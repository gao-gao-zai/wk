// growthjobs.go 成长任务「一键完成」的异步任务模型。
//
// 为什么不用同步 HTTP 等结果：全量自动化是分钟级流水线（17 项 × 节流 1.05s
// + 专家链 6s 间隔 × 8 + 异步计分回读 12s/项），全部账号批跑更久。同步等
// 会撞浏览器/网关超时，用户也不敢关页面。改成：
//
//	POST /admin/account/{uid}/tasks/auto_all  → 202 {"job_id": "..."}（立即返回）
//	GET  /admin/growth/jobs/{id}              → 进度快照（可反复查，幂等）
//	GET  /admin/growth/jobs?active=1          → 进行中的任务（页面加载时恢复进度条）
//
// 任务注册表在内存（单副本部署，重启丢进度可接受——动作本身幂等，重跑即恢复）。
package server

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// growthJob 一个异步任务的实时状态。字段按"前端渲染进度条需要什么"设计：
// 当前执行到第几项、单项进度文本、已完成项的结果明细。
type growthJob struct {
	ID        string    `json:"id"`
	UID       string    `json:"uid"`  // 目标账号（all 模式为空）
	Mode      string    `json:"mode"` // account（单账号全量）/ all（全部账号）
	CreatedAt time.Time `json:"created_at"`

	mu         sync.Mutex `json:"-"`
	startedAt  time.Time
	finishedAt time.Time
	cancelled  bool
	// total/done 进度：单账号 = 任务项数；all = 账号数 × 各自任务项数（动态累计）。
	total int
	done  int
	// current 当前执行项描述（如 "expert_5（3/5 位专家）" 或账号 uid）。
	current string
	// results 已完成项的结果（按完成序追加）。
	results []growthJobItem
	// phase 粗粒度阶段：running / done / failed / cancelled。
	phase string
	// error 整体失败原因（列表拉不到之类）。
	errMsg string
}

// growthJobItem 单项结果。
type growthJobItem struct {
	TaskCode string `json:"task_code,omitempty"`
	UID      string `json:"uid,omitempty"` // all 模式标记归属账号
	Desc     string `json:"desc,omitempty"`
	OK       bool   `json:"ok"`
	Skipped  bool   `json:"skipped,omitempty"`
	Claimed  bool   `json:"claimed,omitempty"`
	Message  string `json:"message,omitempty"`
}

// snapshot 导出一份可 JSON 化的进度（锁内拷贝，防竞态读）。
func (j *growthJob) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	elapsed := time.Since(j.startedAt)
	var eta any
	if j.phase == "running" && j.done > 0 && j.total > j.done {
		per := elapsed / time.Duration(j.done)
		eta = (time.Duration(j.total-j.done) * per).Truncate(time.Second).String()
	}
	snap := map[string]any{
		"id": j.ID, "uid": j.UID, "mode": j.Mode,
		"created_at": j.CreatedAt.Format(time.RFC3339),
		"total":      j.total, "done": j.done, "current": j.current,
		"phase": j.phase, "results": append([]growthJobItem(nil), j.results...),
	}
	if !j.finishedAt.IsZero() {
		snap["finished_at"] = j.finishedAt.Format(time.RFC3339)
		snap["elapsed"] = j.finishedAt.Sub(j.startedAt).Truncate(time.Second).String()
	} else {
		snap["elapsed"] = elapsed.Truncate(time.Second).String()
	}
	if eta != nil {
		snap["eta"] = eta
	}
	if j.errMsg != "" {
		snap["error"] = j.errMsg
	}
	return snap
}

// advance/noteCurrent/fail 等进度更新方法（内部加锁）。
func (j *growthJob) noteCurrent(text string) {
	j.mu.Lock()
	j.current = text
	j.mu.Unlock()
}

func (j *growthJob) setTotal(n int) {
	j.mu.Lock()
	j.total = n
	j.mu.Unlock()
}

func (j *growthJob) addTotal(n int) {
	j.mu.Lock()
	j.total += n
	j.mu.Unlock()
}

func (j *growthJob) finishItem(item growthJobItem) {
	j.mu.Lock()
	j.done++
	j.results = append(j.results, item)
	j.current = ""
	j.mu.Unlock()
}

func (j *growthJob) finish(phase, errMsg string) {
	j.mu.Lock()
	j.phase = phase
	j.errMsg = errMsg
	j.finishedAt = time.Now()
	j.current = ""
	j.mu.Unlock()
}

// ---------------------------------------------------------------------------
// 注册表
// ---------------------------------------------------------------------------

// growthJobRegistry 内存任务表。
type growthJobRegistry struct {
	mu   sync.Mutex
	jobs map[string]*growthJob
	seq  int
}

func (r *growthJobRegistry) newJob(uid, mode string) *growthJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.jobs == nil {
		r.jobs = make(map[string]*growthJob)
	}
	r.seq++
	// 短 ID 即可：注册表内存态，单实例无碰撞风险；带序号便于人读日志。
	id := fmt.Sprintf("gj%d", r.seq)
	j := &growthJob{
		ID: id, UID: uid, Mode: mode,
		CreatedAt: time.Now(), startedAt: time.Now(), phase: "running",
	}
	r.jobs[id] = j
	return j
}

func (r *growthJobRegistry) get(id string) *growthJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jobs[id]
}

// active 返回全部未结束任务（页面加载时恢复进度条用）。
func (r *growthJobRegistry) active() []*growthJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*growthJob
	for _, j := range r.jobs {
		j.mu.Lock()
		running := j.phase == "running"
		j.mu.Unlock()
		if running {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].CreatedAt.Before(out[b].CreatedAt) })
	return out
}

// sweep 清理已结束且超过 TTL 的任务（防内存无界增长；结果页面关了就没人看了）。
func (r *growthJobRegistry) sweep(ttl time.Duration) {
	cut := time.Now().Add(-ttl)
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, j := range r.jobs {
		j.mu.Lock()
		old := !j.finishedAt.IsZero() && j.finishedAt.Before(cut)
		j.mu.Unlock()
		if old {
			delete(r.jobs, id)
		}
	}
}

// growthJobTTL 已结束任务的保留时长。
const growthJobTTL = 2 * time.Hour

// startGrowthJobSweeper 后台定期清理（NewHandler 里起，单 goroutine）。
func (h *Handler) startGrowthJobSweeper() {
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			h.growthJobs.sweep(growthJobTTL)
		}
	}()
}
