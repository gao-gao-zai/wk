// growthledger.go 成长任务持久台账：一次性任务完成记录 + 积分收益统计。
//
// 背景：任务领奖状态（claimed）在上游服务端，网关侧的 job 明细与容器日志
// 都是易失的（job TTL 2h / 日志滚动），用户无法回答"哪些号做完了、哪些
// 还没做、做任务总共赚了多少分"。
//
// 形态：data/growth-ledger.json（state.json 同目录，随卷持久）。
//   - 记录粒度 = 账号 × 任务码：领取时间、获得积分/能量
//   - 只记"经本网关自动领奖成功"的条目；上游侧已 claimed 但非本网关领取的
//     （用户手动/此前完成）在查询端点里实时对账补全（不落盘，标记
//     source=upstream）
//   - 查询端点合并池账号给三态视图：完成 / 部分 / 未开始
package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// ledgerClaim 单条领取记录（账号 × 任务码）。
type ledgerClaim struct {
	TaskCode string `json:"task_code"`
	Credit   int64  `json:"credit"`
	Energy   int64  `json:"energy"`
	At       string `json:"at"` // RFC3339 领取时间
}

// ledgerAccount 单账号台账。
type ledgerAccount struct {
	UID      string        `json:"uid"`
	Nickname string        `json:"nickname,omitempty"`
	Claims   []ledgerClaim `json:"claims"`
}

// growthLedger 内存态 + 落盘。启动读入；领取时追加并原子写回。
type growthLedger struct {
	mu   sync.Mutex
	path string
	byUID map[string]*ledgerAccount // key: uid
	order []string                  // 首次记录顺序（落盘保留，读回恢复）
}

// newGrowthLedger path 空 = 纯内存（不落盘，测试用）。
func newGrowthLedger(path string) *growthLedger {
	l := &growthLedger{path: path, byUID: map[string]*ledgerAccount{}}
	if path == "" {
		return l
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return l
	}
	var stored struct {
		Accounts []*ledgerAccount `json:"accounts"`
	}
	if json.Unmarshal(raw, &stored) != nil {
		return l // 损坏则重新开始记（上游 claimed 仍可对账补全）
	}
	for _, acc := range stored.Accounts {
		if acc == nil || acc.UID == "" {
			continue
		}
		l.byUID[acc.UID] = acc
		l.order = append(l.order, acc.UID)
	}
	return l
}

// record 记一条领取（credit/energy 来自 ClaimReward 的返回值；为 0 也记
// —— 有些任务奖励是 buddy/道具，账面分值为 0 但"完成"事实要留痕）。
// receiver 为 nil 时静默跳过（handler 未初始化台账的路径）。
func (l *growthLedger) record(uid, nickname, taskCode string, credit, energy int64) {
	if l == nil || uid == "" || taskCode == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	acc := l.byUID[uid]
	if acc == nil {
		acc = &ledgerAccount{UID: uid, Nickname: nickname}
		l.byUID[uid] = acc
		l.order = append(l.order, uid)
	}
	if nickname != "" && acc.Nickname != nickname {
		acc.Nickname = nickname
	}
	// 幂等：同任务重复领取只保留首条（时间取最早）。
	for _, c := range acc.Claims {
		if c.TaskCode == taskCode {
			return
		}
	}
	acc.Claims = append(acc.Claims, ledgerClaim{
		TaskCode: taskCode, Credit: credit, Energy: energy,
		At: time.Now().Format(time.RFC3339),
	})
	l.flushLocked()
}

// flushLocked 原子落盘（调用方需持锁）。
func (l *growthLedger) flushLocked() {
	if l.path == "" {
		return
	}
	accounts := make([]*ledgerAccount, 0, len(l.order))
	for _, uid := range l.order {
		if acc := l.byUID[uid]; acc != nil {
			accounts = append(accounts, acc)
		}
	}
	raw, err := json.MarshalIndent(struct {
		Accounts []*ledgerAccount `json:"accounts"`
	}{accounts}, "", "  ")
	if err != nil {
		return
	}
	_ = writeFileAtomic(l.path, append(raw, '\n'), 0600)
}

// ledgerSummary 单账号的台账汇总（查询端点用）。
type ledgerSummary struct {
	UID        string   `json:"uid"`
	Nickname   string   `json:"nickname,omitempty"`
	Status     string   `json:"status"`            // done | partial | not_started
	DoneCount  int      `json:"done_count"`        // 已领取任务数（台账 + 上游对账）
	TotalCount int      `json:"total_count"`       // 该账号可自动任务总数（growthActions）
	Credit     int64    `json:"credit"`            // 台账记录的积分收益
	Energy     int64    `json:"energy"`            // 台账记录的能量收益
	LastAt     string   `json:"last_at,omitempty"` // 最近一次领取时间
	Tasks      []ledgerClaim `json:"tasks,omitempty"` // 逐任务明细（可展开）
}

// ledgerOverview 全局统计 + 三态账号列表。
type ledgerOverview struct {
	TotalAccounts int   `json:"total_accounts"`
	DoneAccounts  int   `json:"done_accounts"`
	PartialAccounts int `json:"partial_accounts"`
	NotStarted    int   `json:"not_started"`
	TotalCredit   int64 `json:"total_credit"` // 台账口径的额外积分收益
	TotalEnergy   int64 `json:"total_energy"`
	Accounts      []ledgerSummary `json:"accounts"`
}

// growthLedgerOverview GET /admin/growth/ledger：一次性任务三态总览。
// 逐账号现查上游 claimed 对账：并发拉取（上限 8）避免 85 号串行往返；
// 只读列表端点，上不了风控面。
func (h *Handler) growthLedgerOverview(w http.ResponseWriter, r *http.Request) {
	var accounts []*auth.Auth
	for _, st := range h.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := h.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.Snapshot().AccessToken == "" || a.Region() == "global" {
			continue
		}
		accounts = append(accounts, a)
	}
	// 并发预取任务列表（results 与 accounts 下标对齐；失败 = nil，跳过对账）。
	results := make([][]upstream.Task, len(accounts))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, a := range accounts {
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ts, err := h.cfg.Upstream.ListTasks(a)
			if err == nil {
				results[i] = ts
			}
		}(i, a)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, h.buildLedgerOverview(accounts, results))
}

// buildLedgerOverview 合并台账 + 池账号 + 上游 claimed 对账，产出三态视图。
// accounts 为池内全部 CN 账号（调用方过滤）；tasks 为下标对齐的预取结果
// （nil = 该账号对账跳过，仅台账口径）。
func (h *Handler) buildLedgerOverview(accounts []*auth.Auth, tasks [][]upstream.Task) ledgerOverview {
	l := h.growthLedger
	if l == nil {
		l = newGrowthLedger("") // 未初始化（测试/异常路径）：空台账口径
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	auto := map[string]bool{}
	for i := range growthActions {
		auto[growthActions[i].TaskCode] = true
	}
	ov := ledgerOverview{Accounts: make([]ledgerSummary, 0, len(accounts))}
	for idx, a := range accounts {
		creds := a.Snapshot()
		sum := ledgerSummary{UID: creds.UID, Nickname: creds.Nickname, TotalCount: len(growthActions)}
		// 台账条目（本网关领取的，有时间与积分）。
		claims := []ledgerClaim{}
		if acc := l.byUID[creds.UID]; acc != nil {
			for _, c := range acc.Claims {
				claims = append(claims, c)
				sum.Credit += c.Credit
				sum.Energy += c.Energy
				if c.At > sum.LastAt {
					sum.LastAt = c.At
				}
			}
		}
		seen := map[string]bool{}
		for _, c := range claims {
			seen[c.TaskCode] = true
		}
		// 上游对账：claimed 但台账没有的可自动任务 → 计入 done（无时间/积分，
		// 用户手动或部署本网关之前完成的）。不落盘。
		if idx < len(tasks) && tasks[idx] != nil {
			for _, t := range tasks[idx] {
				if t.Claimed && auto[t.TaskCode] && !seen[t.TaskCode] {
					seen[t.TaskCode] = true
					claims = append(claims, ledgerClaim{TaskCode: t.TaskCode})
				}
			}
		}
		sum.DoneCount = len(seen)
		sort.Slice(claims, func(i, j int) bool { return claims[i].TaskCode < claims[j].TaskCode })
		sum.Tasks = claims
		switch {
		case sum.DoneCount >= sum.TotalCount && sum.TotalCount > 0:
			sum.Status = "done"
			ov.DoneAccounts++
		case sum.DoneCount > 0:
			sum.Status = "partial"
			ov.PartialAccounts++
		default:
			sum.Status = "not_started"
			ov.NotStarted++
		}
		ov.TotalCredit += sum.Credit
		ov.TotalEnergy += sum.Energy
		ov.Accounts = append(ov.Accounts, sum)
	}
	ov.TotalAccounts = len(accounts)
	return ov
}

// ledgerPathFromState 推导台账落盘路径（state.json 同目录）。
func ledgerPathFromState(stateFile string) string {
	if stateFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(stateFile), "growth-ledger.json")
}

// upstream.Task 需要的字段（ListTasks 返回值）在此文件可见性检查。
var _ = upstream.Task{}
