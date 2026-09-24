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
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
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

// accountDone all 模式：一个账号处理完（进度 +1，不动 results）。
// 与 finishItem 分开：账号粒度的 done/total 才是稳定分母（启动即知账号数），
// 任务项明细用 appendResult 只进 results。
func (j *growthJob) accountDone() {
	j.mu.Lock()
	j.done++
	j.current = ""
	j.mu.Unlock()
}

// accountFinished all 模式：全部账号是否已跑完（done ≥ total）——
// 执行池里最后一个账号跑完时用它触发 job finish（多 job 交错各收各的）。
func (j *growthJob) accountFinished() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.done >= j.total
}

// appendResult 追加结果明细不动进度计数（all 模式的任务项）。
func (j *growthJob) appendResult(item growthJobItem) {
	j.mu.Lock()
	j.results = append(j.results, item)
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

// ---------------------------------------------------------------------------
// 重启恢复：all 模式批跑的落盘标记
// ---------------------------------------------------------------------------

// growthJobMark 落盘标记形状。Pending 为剩余待跑账号（启动时写入全量，
// 每完成一个划掉一个；全部完成删除文件）。
type growthJobMark struct {
	Mode    string   `json:"mode"`
	Pending []string `json:"pending"`
	Started string   `json:"started"`
}

// growthJobMarkPath 落盘路径（空 = 功能关闭）。main 从 state.json 同目录注入。
func (h *Handler) growthJobMarkPath() string { return h.cfg.GrowthJobMarkPath }

// writeGrowthJobMark 原子写标记文件（待跑清单快照）。空清单不写文件
// （避免残留 {"pending":[]}——恢复读不到即视为无任务，与删除语义一致）。
func (h *Handler) writeGrowthJobMark(pending []string) {
	path := h.growthJobMarkPath()
	if path == "" || len(pending) == 0 {
		return
	}
	mark := growthJobMark{Mode: "all", Pending: pending, Started: time.Now().Format(time.RFC3339)}
	raw, err := json.MarshalIndent(mark, "", "  ")
	if err != nil {
		return
	}
	_ = writeFileAtomic(path, append(raw, '\n'), 0600)
}

// clearGrowthJobMark 批跑全部结束时删除标记。
func (h *Handler) clearGrowthJobMark() {
	path := h.growthJobMarkPath()
	if path == "" {
		return
	}
	_ = os.Remove(path)
}

// readGrowthJobMark 读启动残留标记（无文件/损坏返回 nil）。
func (h *Handler) readGrowthJobMark() *growthJobMark {
	path := h.growthJobMarkPath()
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var mark growthJobMark
	if json.Unmarshal(raw, &mark) != nil || len(mark.Pending) == 0 {
		return nil
	}
	return &mark
}

// ResumeGrowthJobsAfterRestart 重启恢复入口（main 启动完成后调用）。
// 检测到残留标记（上次进程死在批跑半路）时，把剩余账号重新提交到执行池：
//   - 账号按标记里的 Pending 顺序重查（禁用/移除/global 的自然过滤）
//   - 动作幂等：上次已完成的项这次秒级跳过
//   - 立即清除旧标记并入队（入队即同步标记），避免二次重启叠加
func (h *Handler) ResumeGrowthJobsAfterRestart() {
	mark := h.readGrowthJobMark()
	if mark == nil {
		return
	}
	var accounts []*auth.Auth
	for _, uid := range mark.Pending {
		a := h.cfg.Pool.AuthByUID(uid)
		if a == nil || a.Snapshot().AccessToken == "" {
			continue // 账号已移除/无凭证
		}
		if a.Region() == "global" {
			continue
		}
		accounts = append(accounts, a)
	}
	log.Printf("growth: 检测到重启前未完成的批跑标记（%d 个待跑账号），自动恢复", len(mark.Pending))
	if len(accounts) == 0 {
		h.clearGrowthJobMark()
		log.Printf("growth: 待跑账号均已不可用，标记清除")
		return
	}
	job := h.growthJobs.newJob("", "all")
	// 先写初始快照再入队（入队后 drain 立即起跑，run 尾部的 syncGrowthMark
	// 是唯一同步点；入队方滞后写会与 drain 的 clear 竞争复活已删标记）。
	h.writeGrowthJobMark(mark.Pending)
	// 延迟几秒再入队：让服务先把监听/健康检查立起来，部署脚本不误判启动失败。
	go func() {
		time.Sleep(5 * time.Second)
		if n := h.growthSubmitAll(job, accounts); n == 0 {
			h.clearGrowthJobMark()
		}
	}()
}
