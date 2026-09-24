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
	mu    sync.Mutex
	path  string
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
// 记录后由调用方失效对账缓存（见 growthLedgerOverview 的 ledgerReconCache）。
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
	UID        string        `json:"uid"`
	Nickname   string        `json:"nickname,omitempty"`
	Status     string        `json:"status"`            // done | partial | not_started
	DoneCount  int           `json:"done_count"`        // 已领取任务数（台账 + 上游对账）
	TotalCount int           `json:"total_count"`       // 该账号可自动任务总数（growthActions）
	Credit     int64         `json:"credit"`            // 台账记录的积分收益
	Energy     int64         `json:"energy"`            // 台账记录的能量收益
	LastAt     string        `json:"last_at,omitempty"` // 最近一次领取时间
	Tasks      []ledgerClaim `json:"tasks,omitempty"`   // 逐任务明细（可展开）
}

// ledgerOverview 全局统计 + 三态账号列表。
type ledgerOverview struct {
	TotalAccounts   int             `json:"total_accounts"`
	DoneAccounts    int             `json:"done_accounts"`
	PartialAccounts int             `json:"partial_accounts"`
	NotStarted      int             `json:"not_started"`
	TotalCredit     int64           `json:"total_credit"` // 台账口径的额外积分收益
	TotalEnergy     int64           `json:"total_energy"`
	Accounts        []ledgerSummary `json:"accounts"`
}

// ledgerCacheTTL 上游对账结果的缓存时长。三态判定是低频慢变数据：
// claimed 只在领奖时变，而领奖都经本网关（写台账 + 主动失效缓存）。
// TTL 只兜「上游侧手动领奖」的窗口（面板上点过「领取」之类）。
const ledgerCacheTTL = 10 * time.Minute

// ledgerCache 上游 ListTasks 对账结果的 TTL 缓存（账号集 + 结果快照）。
// key = 账号 uid 列表（池变化 = 不同 key，自动失效）。
type ledgerCache struct {
	mu      sync.Mutex
	key     string // 参与对账的 uid 逗号串（签名）
	fetched time.Time
	results [][]upstream.Task
	// byUID 单账号索引（与 results 同源同 TTL）：批跑预跳过按号查快，
	// 不必线性扫描。填充于 set；invalidate 清空。
	byUID map[string][]upstream.Task
}

// signature 账号集签名（uid 顺序拼接）。
func ledgerCacheKey(accounts []*auth.Auth) string {
	var b []byte
	for _, a := range accounts {
		b = append(b, a.Snapshot().UID...)
		b = append(b, ',')
	}
	return string(b)
}

// get 命中返回缓存结果（TTL 内且账号集一致）。
func (c *ledgerCache) get(accounts []*auth.Auth) ([][]upstream.Task, bool) {
	key := ledgerCacheKey(accounts)
	c.mu.Lock()
	defer c.mu.Unlock()
	// fetched 零值 = 从未填充（invalidate 也会清零），key 匹配 + 未过期才命中。
	if c.key == "" || c.key != key || time.Since(c.fetched) > ledgerCacheTTL {
		return nil, false
	}
	return c.results, true
}

// set 写入缓存（深拷贝不需要：ListTasks 每次新分配，读方只读）。
// 同时填充按 uid 索引（批跑预跳过用）。
func (c *ledgerCache) set(accounts []*auth.Auth, results [][]upstream.Task) {
	key := ledgerCacheKey(accounts)
	c.mu.Lock()
	c.key, c.results, c.fetched = key, results, time.Now()
	c.byUID = make(map[string][]upstream.Task, len(accounts))
	for i, a := range accounts {
		if i < len(results) && results[i] != nil {
			c.byUID[a.Snapshot().UID] = results[i]
		}
	}
	c.mu.Unlock()
}

// invalidate 主动失效（领奖成功后调用：上游 claimed 变了，缓存即过期）。
// key 清空 + fetched 归零：get 的双条件都挡（空 key 恒不命中，防止
// 「无账号集」与「已失效」共享空串 key 的边界）。
func (c *ledgerCache) invalidate() {
	c.mu.Lock()
	c.key = ""
	c.fetched = time.Time{}
	c.byUID = nil
	c.mu.Unlock()
}

// tasksForUID 取单账号的对账快照（TTL 内命中才返回 true；快照可能为 nil
// = 该号上次对账拉取失败，调用方按未命中处理）。
func (c *ledgerCache) tasksForUID(uid string) ([]upstream.Task, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byUID == nil || time.Since(c.fetched) > ledgerCacheTTL {
		return nil, false
	}
	ts, ok := c.byUID[uid]
	return ts, ok
}

// markClaimed 领奖后把该账号快照中对应任务置 claimed（单号精准更新而非
// 整体失效：批跑中刚领完的号预跳过立即可用，其它号的快照也不丢）。
// 该号无快照时无操作（get 路径下次对账自然反映）。
func (c *ledgerCache) markClaimed(uid, taskCode string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byUID == nil || time.Since(c.fetched) > ledgerCacheTTL {
		return
	}
	ts := c.byUID[uid]
	for i := range ts {
		if ts[i].TaskCode == taskCode {
			ts[i].Claimed = true
			return
		}
	}
}

// growthLedgerOverview GET /admin/growth/ledger：一次性任务三态总览。
// 上游对账结果带 TTL 缓存（10 分钟 + 领奖主动失效）：对账是低频慢变数据，
// 每次页面加载都打 85 号 ListTasks 既慢又白费；领奖都经本网关 → record
// 即失效，TTL 只兜上游侧手动领奖的窗口。
// ?refresh=1 强制绕过缓存（对账怀疑不准时的手动出口）。
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
	var results [][]upstream.Task
	force := r.URL.Query().Get("refresh") == "1"
	if !force {
		if cached, ok := h.ledgerReconCache.get(accounts); ok {
			results = cached
		}
	}
	if results == nil {
		// 并发预取任务列表（results 与 accounts 下标对齐；失败 = nil，跳过对账）。
		results = make([][]upstream.Task, len(accounts))
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
		h.ledgerReconCache.set(accounts, results)
	}
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
