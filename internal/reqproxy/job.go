// Job：长动作的后端任务跟踪（刷新不丢的等待动画基础）。
//
// 前端按钮触发长动作（全量测速/再平衡/订阅刷新）后轮询任务状态驱动
// loading；页面刷新后重新拉任务列表，running 状态的动画立即恢复。
package reqproxy

import (
	"sync"
	"time"
)

// Job 任务记录。
type Job struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`  // health-run | rebalance | sub-refresh | prewarm
	Label     string    `json:"label"` // 展示名（"全量测速"）
	State     string    `json:"state"` // running | done | error
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	Note      string    `json:"note,omitempty"` // 结果摘要 / 错误信息
}

// jobTracker 任务登记簿。
type jobTracker struct {
	mu   sync.Mutex
	jobs map[string]*Job
}

var tracker = &jobTracker{jobs: map[string]*Job{}}

// StartJob 登记开始。返回任务 ID。
func StartJob(kind, label string) string {
	j := &Job{
		ID:        newID("job"),
		Kind:      kind,
		Label:     label,
		State:     "running",
		StartedAt: time.Now(),
	}
	tracker.mu.Lock()
	tracker.jobs[j.ID] = j
	// 清理：保留最近 100 条
	if len(tracker.jobs) > 100 {
		for id := range tracker.jobs {
			delete(tracker.jobs, id)
			if len(tracker.jobs) <= 100 {
				break
			}
		}
	}
	tracker.mu.Unlock()
	return j.ID
}

// FinishJob 结束任务（note 为结果摘要；errText 非空 = error 态）。
func FinishJob(id, note, errText string) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	j, ok := tracker.jobs[id]
	if !ok {
		return
	}
	j.EndedAt = time.Now()
	j.Note = note
	if errText != "" {
		j.State = "error"
		j.Note = errText
	} else {
		j.State = "done"
	}
}

// RunningJob 按 kind 找正在跑的任务（前端刷新后恢复动画用）。
// 没有运行中的返回 nil。
func RunningJob(kind string) *Job {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	for _, j := range tracker.jobs {
		if j.Kind == kind && j.State == "running" {
			cp := *j
			return &cp
		}
	}
	return nil
}

// RecentJobs 最近任务（新的在前，上限 n）。
func RecentJobs(n int) []Job {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	out := make([]Job, 0, len(tracker.jobs))
	for _, j := range tracker.jobs {
		out = append(out, *j)
	}
	// 新的在前
	sortJobsByStartDesc(out)
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func sortJobsByStartDesc(js []Job) {
	for i := 1; i < len(js); i++ {
		for j := i; j > 0 && js[j].StartedAt.After(js[j-1].StartedAt); j-- {
			js[j], js[j-1] = js[j-1], js[j]
		}
	}
}
