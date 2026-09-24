// uidwatcher.go 对接码监控 + 额度化自动加号（UID Watcher，2026-09）。
//
// 定位：后台值班员。它不做任何加号链路的事——取号、收码、释放、落盘、
// 池自动出库全部复用 AutoEnroller；这里只做四件事：
//
//	观察（H5 type=8 定期拉码表）
//	判定（新码 / 降价进入区间 / 补货，扣除自身消耗）
//	派活（临时切换 sid + 单码 → AutoRunWith → 等完成 → 恢复原参数）
//	记账（成功数 × 触发时快照价，从额度里扣；耗尽即停，充值才恢复）
//
// 设计文档：docs/auto-enroll-uid-watcher.md。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/haozhumah5"
)

// ---- 配置（config.json autoenroll.watch，用户可编辑） ----

// WatchProject 一个被监控的豪猪项目。
type WatchProject struct {
	// Sid 项目数字 ID（取号 API 用，如 52283）。
	Sid string `json:"sid"`
	// HexSID type=8 查询用的 16 位 hex 项目会话标识（前端选项目时带上）。
	HexSID string `json:"hex_sid"`
	// Name 项目名称（展示用）。
	Name string `json:"name"`
	// MaxPrice 只接受 ≤ 此价（元）的对接码。>0 必填。
	MaxPrice float64 `json:"max_price"`
	// MinStock 触发要求的最低库存（≥1：库存 0 的码取不到号）。
	MinStock int `json:"min_stock"`
	// Enabled false = 暂停该项目的事件判定（码表仍拉取，观测不中断）。
	Enabled bool `json:"enabled"`
}

// WatchConfig 对接码监控的完整配置（config.json autoenroll.watch）。
type WatchConfig struct {
	Enabled         bool           `json:"enabled"`
	IntervalSeconds int            `json:"interval_seconds"` // 拉取间隔；0 = 默认 300
	WantPerTrigger  int            `json:"want_per_trigger"` // 每次触发加几个号；0 = 1
	Workers         int            `json:"workers"`          // 触发任务并发；0 = 1
	Groups          []string       `json:"groups"`           // 新账号登记分组
	Projects        []WatchProject `json:"projects"`
}

// watchIntervalMin / watchIntervalMax 拉取间隔的上下限。下限 60s 是防
// 风控底线（H5 是逆向接口，打太勤等于邀请封号）；上限 3600s 再长就
// 失去"盯盘"意义。
const (
	watchIntervalMin = 60
	watchIntervalMax = 3600
)

func (c *WatchConfig) interval() time.Duration {
	secs := c.IntervalSeconds
	if secs < watchIntervalMin {
		secs = 300 // 0/非法 = 默认 5 分钟（夹到下限太激进）
	}
	if secs > watchIntervalMax {
		secs = watchIntervalMax
	}
	return time.Duration(secs) * time.Second
}

func (c *WatchConfig) want() int {
	if c.WantPerTrigger < 1 {
		return 1
	}
	if c.WantPerTrigger > 10 {
		return 10
	}
	return c.WantPerTrigger
}

func (c *WatchConfig) workers() int {
	if c.Workers < 1 {
		return 1
	}
	if c.Workers > 3 {
		return 3
	}
	return c.Workers
}

// Validate 校验配置（PUT /watch 入口用）。返回清洗后的副本。
func (c *WatchConfig) Validate() (WatchConfig, error) {
	out := WatchConfig{
		Enabled:         c.Enabled,
		IntervalSeconds: c.IntervalSeconds,
		WantPerTrigger:  c.want(),
		Workers:         c.workers(),
		Groups:          normalizeRunGroups(c.Groups),
	}
	if c.IntervalSeconds != 0 && (c.IntervalSeconds < watchIntervalMin || c.IntervalSeconds > watchIntervalMax) {
		return out, fmt.Errorf("拉取间隔需在 %d-%d 秒之间（默认 300）", watchIntervalMin, watchIntervalMax)
	}
	seen := map[string]bool{}
	for _, p := range c.Projects {
		sid := strings.TrimSpace(p.Sid)
		if sid == "" {
			return out, errors.New("项目 ID 不能为空")
		}
		if !watchSIDPattern.MatchString(sid) {
			return out, fmt.Errorf("项目 ID 格式不正确：%q（应为数字，如 52283）", sid)
		}
		hex := strings.TrimSpace(p.HexSID)
		if !watchHexPattern.MatchString(hex) {
			return out, fmt.Errorf("项目 %s 的 hex_sid 格式不正确（应为 16 位十六进制，前端选择项目时自动带上）", sid)
		}
		if math.IsNaN(p.MaxPrice) || math.IsInf(p.MaxPrice, 0) || p.MaxPrice <= 0 {
			return out, fmt.Errorf("项目 %s 的最高价必须大于 0（元）", sid)
		}
		if p.MinStock < 1 {
			return out, fmt.Errorf("项目 %s 的最少库存需 ≥ 1", sid)
		}
		if seen[sid] {
			return out, fmt.Errorf("项目 %s 重复配置", sid)
		}
		seen[sid] = true
		out.Projects = append(out.Projects, WatchProject{
			Sid:      sid,
			HexSID:   hex,
			Name:     strings.TrimSpace(p.Name),
			MaxPrice: p.MaxPrice,
			MinStock: p.MinStock,
			Enabled:  p.Enabled,
		})
	}
	if c.Enabled && len(out.Projects) == 0 {
		return out, errors.New("启用监控前至少配置一个项目")
	}
	return out, nil
}

// ---- 运行时状态（data/watcher-state.json，程序记账管理） ----

// watchUIDState 单个对接码的监控状态。
type watchUIDState struct {
	// KnownStock 上次快照基线库存：stock_now > KnownStock 才算真补货。
	// 基线永远等于上次观察值——自身消耗只会让观察值下降或不变，不可能
	// 让它上升，所以"扣自身消耗"不需要单独记账（早先版本记过
	// OwnPending，实际会造成双重扣减：市场先降后稳会被误判成回升）。
	// 负数 = 码已失效（dead），不再参与判定。
	KnownStock int     `json:"known_stock"`
	LastPrice  float64 `json:"last_price"`
	Consumed   int     `json:"consumed"` // 我们经它成功加掉的号数（累计）
	Joined     bool    `json:"joined"`   // 是否已加入过豪猪账户
}

// watcherState 持久化的运行时状态。
type watcherState struct {
	BudgetRemaining float64                  `json:"budget_remaining"`
	SpentTotal      float64                  `json:"spent_total"`
	EnrolledTotal   int                     `json:"enrolled_total"`
	TriggerTotal    int                     `json:"trigger_total"`
	PausedReason    string                  `json:"paused_reason"`
	PerUID          map[string]*watchUIDState `json:"per_uid"`
	LastTick        string                  `json:"last_tick"`
	UpdatedAt       string                  `json:"updated_at"`
}

func (s *watcherState) clone() watcherState {
	out := watcherState{
		BudgetRemaining: s.BudgetRemaining,
		SpentTotal:      s.SpentTotal,
		EnrolledTotal:   s.EnrolledTotal,
		TriggerTotal:    s.TriggerTotal,
		PausedReason:    s.PausedReason,
		PerUID:          make(map[string]*watchUIDState, len(s.PerUID)),
		LastTick:        s.LastTick,
		UpdatedAt:       s.UpdatedAt,
	}
	for k, v := range s.PerUID {
		c := *v
		out.PerUID[k] = &c
	}
	return out
}

// ---- Watcher ----

// UIDWatcher 对接码监控值班员。
//
// 并发模型：单个 goroutine 循环（tickLoop）串行处理所有轮次；PUT 配置
// 经 reload 通道通知循环用新配置重建 ticker。执行加号任务（可能跑几分钟）
// 也在循环 goroutine 里同步等待完成——等待期间不会再拉表（本来也不该），
// 也不会错过停止信号（select ctx.Done / 任务完成）。
type UIDWatcher struct {
	// en 派活对象（复用 AutoEnroller 全部加号链路）。
	en *AutoEnroller
	// h5 拉码表/加码（强依赖：H5 未配置时 watcher 无法工作）。
	h5 *haozhumah5.Client
	// statePath data/watcher-state.json 路径；空 = 不持久化（测试）。
	statePath string

	// logf 外部注入的日志函数（AutoEnroller.logf 走控制台可见链路）。
	logf func(format string, args ...any)

	mu     sync.Mutex
	cfg    WatchConfig
	state  watcherState
	logs   []string // 活动日志（内存，最近 100 条）
	cancel context.CancelFunc
	// loopRunning 值班循环是否在跑（Run 常驻模型，Start 幂等拉起用）。
	loopRunning bool
	reloadCh    chan struct{}
	// running 是否有值班轮触发的加号任务在跑（区别于 AutoEnroller.running：
	// 手动任务不算）。用于状态回显。
	running bool
	// lastNextTick 下次拉取时间（回显"下次拉取 xx:xx:xx"）。
	lastNextTick time.Time
}

// NewUIDWatcher 建值班员。cfg 为启动配置（main 从 config.json 读入）；
// statePath 空 = 纯内存。h5/en 为 nil 时 Run 立即退出（不可用状态回显）。
func NewUIDWatcher(cfg WatchConfig, h5 *haozhumah5.Client, en *AutoEnroller, statePath string) *UIDWatcher {
	w := &UIDWatcher{
		en:        en,
		h5:        h5,
		statePath: statePath,
		cfg:       cfg,
	}
	w.state = watcherState{PerUID: map[string]*watchUIDState{}}
	if statePath != "" {
		if raw, err := os.ReadFile(statePath); err == nil {
			var s watcherState
			if json.Unmarshal(raw, &s) == nil && s.PerUID != nil {
				w.state = s
			}
		}
	}
	return w
}

// SetLogf 注入日志函数（handler 组装时指向 AutoEnroller.AppendLog，
// 让 watcher 日志也出现在自动加号控制台）。nil = 只打标准日志。
func (w *UIDWatcher) SetLogf(fn func(format string, args ...any)) {
	w.mu.Lock()
	w.logf = fn
	w.mu.Unlock()
}

func (w *UIDWatcher) log(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("[uid-watcher] %s", msg)
	w.mu.Lock()
	fn := w.logf
	w.logs = append(w.logs, time.Now().Format("15:04:05 ")+msg)
	if len(w.logs) > 100 {
		w.logs = w.logs[len(w.logs)-50:]
	}
	w.mu.Unlock()
	if fn != nil {
		fn("%s", msg)
	}
}

// Run 值班循环主体（阻塞直到 ctx 结束；调用方放 goroutine 里）。
//
// 生命周期模型：循环**常驻**（H5/AutoEnroller 就绪即启动），配置里的
// enabled 只控制 tick 是否派活——这样 PUT /watch 开关切换时不需要
// 重新拉起 goroutine（启动时未配置的部署也能热开启）。依赖缺失时
// 直接返回（状态接口报不可用）。
func (w *UIDWatcher) Run(ctx context.Context) {
	if w.en == nil || w.h5 == nil {
		w.log("监控不可用：豪猪 H5 或自动加号未配置")
		return
	}
	w.mu.Lock()
	cfg := w.cfg
	running := w.loopRunning
	w.loopRunning = true
	w.mu.Unlock()
	if running {
		return // 已有一个循环（main 启动 + Reconfigure 热启动的双保险）
	}
	defer func() {
		w.mu.Lock()
		w.loopRunning = false
		w.mu.Unlock()
	}()

	var ticker *time.Ticker
	interval := cfg.interval()
	ticker = time.NewTicker(interval)
	defer ticker.Stop()

	// reload 通道：Reconfigure 换配置后通知循环重建 ticker。存进
	// w.reloadCh，Reconfigure 在 Run 未启动时安全调（nil 检查）。
	reload := make(chan struct{}, 1)
	w.mu.Lock()
	w.reloadCh = reload
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.reloadCh = nil
		w.mu.Unlock()
	}()

	if cfg.Enabled && len(cfg.Projects) > 0 {
		w.log("监控启动：%d 个项目，间隔 %s，额度余 ¥%.2f",
			len(cfg.Projects), cfg.interval(), w.stateSnapshot().BudgetRemaining)
		// 启动立刻跑第一轮（用户开了开关想立刻看到状态，不想等 5 分钟）。
		w.tick(ctx, cfg)
	}
	for {
		w.mu.Lock()
		w.lastNextTick = time.Now().Add(interval)
		w.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-reload:
			w.mu.Lock()
			cfg = w.cfg
			interval = cfg.interval()
			w.mu.Unlock()
			ticker.Reset(interval)
			if !cfg.Enabled || len(cfg.Projects) == 0 {
				w.log("监控已停止（配置禁用）")
				continue // 循环常驻：等下次热开启，不退出
			}
			w.log("配置已更新：间隔 %s，%d 个项目", interval, len(cfg.Projects))
			w.tick(ctx, cfg)
		case <-ticker.C:
			if cfg.Enabled && len(cfg.Projects) > 0 {
				w.tick(ctx, cfg)
			}
		}
	}
}

// Stop 停止值班循环（服务停机时调；Run 的 ctx 结束是主出口）。
func (w *UIDWatcher) Stop() {
	w.mu.Lock()
	c := w.cancel
	w.mu.Unlock()
	if c != nil {
		c()
	}
}

// Start 拉起值班循环（幂等：已在跑/已在启动过则跳过）。main 启动时和
// Reconfigure 热开启时都会调——双保险，谁先到谁拉起。
func (w *UIDWatcher) Start() {
	w.mu.Lock()
	if w.loopRunning || w.en == nil || w.h5 == nil {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	w.mu.Lock()
	w.cancel = cancel
	w.mu.Unlock()
	go w.Run(ctx)
}

// Reconfigure 运行时更新配置（PUT /watch）。热生效：运行中的循环收到
// reload 信号后重建 ticker；未运行时拉起循环（热开启，如启动时 enabled
// 还是 false、后来 WebUI 打开的场景）。
func (w *UIDWatcher) Reconfigure(cfg WatchConfig) {
	w.mu.Lock()
	prev := w.cfg
	w.cfg = cfg
	ch := w.reloadCh
	w.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default: // 已有排队信号：循环马上会读最新配置，无需重复
		}
		return
	}
	// 循环未运行：从关到开（或启动时从未拉起）→ 现在拉起。
	if !prev.Enabled && cfg.Enabled {
		w.Start()
	}
}

// stateSnapshot 状态快照（锁下拷贝）。
func (w *UIDWatcher) stateSnapshot() watcherState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state.clone()
}

// ---- 状态回显 ----

// WatchStatus GET /watch 的响应体。
type WatchStatus struct {
	Enabled         bool           `json:"enabled"`
	IntervalSeconds int            `json:"interval_seconds"`
	WantPerTrigger  int            `json:"want_per_trigger"`
	Workers         int            `json:"workers"`
	Groups          []string       `json:"groups"`
	Projects        []WatchProject `json:"projects"`

	BudgetRemaining float64 `json:"budget_remaining"`
	SpentTotal      float64 `json:"spent_total"`
	EnrolledTotal   int     `json:"enrolled_total"`
	TriggerTotal    int     `json:"trigger_total"`
	PausedReason    string  `json:"paused_reason"`
	Available       bool    `json:"available"` // H5 + AutoEnroller 就绪
	Running         bool    `json:"running"`   // 值班触发的任务在跑
	LastTick        string  `json:"last_tick"`
	NextTick        string  `json:"next_tick"`
	Logs            []string `json:"logs"`
}

// Status 当前配置 + 状态 + 日志。
func (w *UIDWatcher) Status() WatchStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := WatchStatus{
		Enabled:         w.cfg.Enabled,
		IntervalSeconds: w.cfg.IntervalSeconds,
		WantPerTrigger:  w.cfg.want(),
		Workers:         w.cfg.workers(),
		Groups:          append([]string(nil), w.cfg.Groups...),
		Projects:        append([]WatchProject(nil), w.cfg.Projects...),
		BudgetRemaining: round2(w.state.BudgetRemaining),
		SpentTotal:      round2(w.state.SpentTotal),
		EnrolledTotal:   w.state.EnrolledTotal,
		TriggerTotal:    w.state.TriggerTotal,
		PausedReason:    w.state.PausedReason,
		Available:       w.h5 != nil && w.en != nil,
		Running:         w.running,
		LastTick:        w.state.LastTick,
		Logs:            append([]string(nil), w.logs...),
	}
	if !w.lastNextTick.IsZero() {
		st.NextTick = w.lastNextTick.Format("15:04:05")
	}
	return st
}

// AddBudget 充值（add > 0）或重置（set ≥ 0）。充值会清掉"额度耗尽"的
// 暂停原因——这正是恢复监控的方式。返回新的剩余额度。
func (w *UIDWatcher) AddBudget(add float64, set *float64) (float64, error) {
	if set != nil {
		if *set < 0 || math.IsNaN(*set) || math.IsInf(*set, 0) {
			return 0, errors.New("额度需 ≥ 0")
		}
		w.mu.Lock()
		w.state.BudgetRemaining = round2(*set)
		if w.state.PausedReason != "" && strings.Contains(w.state.PausedReason, "额度耗尽") {
			w.state.PausedReason = ""
		}
		rem := w.state.BudgetRemaining
		w.mu.Unlock()
		w.persist()
		w.log("额度已设为 ¥%.2f", rem)
		return rem, nil
	}
	if add <= 0 || math.IsNaN(add) || math.IsInf(add, 0) {
		return 0, errors.New("充值金额需大于 0")
	}
	w.mu.Lock()
	w.state.BudgetRemaining = round2(w.state.BudgetRemaining + add)
	resumed := w.state.PausedReason != "" && strings.Contains(w.state.PausedReason, "额度耗尽")
	if resumed {
		w.state.PausedReason = ""
	}
	rem := w.state.BudgetRemaining
	w.mu.Unlock()
	if resumed {
		w.log("充值 ¥%.2f：额度恢复为 ¥%.2f，监控继续", add, rem)
	} else {
		w.log("充值 ¥%.2f：额度恢复为 ¥%.2f", add, rem)
	}
	w.persist()
	return rem, nil
}

// ---- 核心：一轮值班 ----

// tick 一轮：拉表 → 判定 → （可能）派活 → 记账。
// 在 Run 的 goroutine 里串行执行，天然无并发问题（state 读写仍走锁，
// 因为 AddBudget/Status 会并发进来）。
func (w *UIDWatcher) tick(ctx context.Context, cfg WatchConfig) {
	w.mu.Lock()
	w.state.LastTick = time.Now().Format("2006-01-02 15:04:05")
	w.mu.Unlock()

	// 会话失效：暂停派活但继续拉表（拉表免费，充值/重贴会话后立即恢复）。
	if err := w.h5.Ping(ctx); err != nil {
		w.setPaused(fmt.Sprintf("H5 会话失效（%v）——请在控制台重新粘贴 PHPSESSID", err))
		return
	}

	// 逐项目拉表 + 判定。**先不同步基线**：预算不足/手动任务在跑时基线
	// 保留陈旧值——否则暂停期间发生的事件会被"看过就算"吞掉，恢复后
	// 反而不触发。
	type candidate struct {
		proj  WatchProject
		uid   string
		price float64
		ev    string // 事件描述（新码 / 降价 / 补货 / 重新上架）
		stock int
	}
	type fetchedTable struct {
		proj  WatchProject
		items []haozhumah5.UIDItem
	}
	var candidates []candidate
	fetched := make([]fetchedTable, 0, len(cfg.Projects))
	// 监控语义就是"看最新库存"：先清 type=8 缓存（通用 TTL 5 分钟比
	// 监控间隔可能长，旧缓存会制造假事件/漏掉真事件）。
	w.h5.InvalidateUIDLists()
	for _, p := range cfg.Projects {
		if !p.Enabled {
			continue
		}
		items, err := w.h5.UIDs(ctx, p.HexSID)
		if err != nil {
			// 拉表失败（非会话失效）：退避不派活，下一轮再看。
			w.log("项目 %s 拉取对接码失败：%v", p.Sid, err)
			continue
		}
		fetched = append(fetched, fetchedTable{p, items})
		snap := w.stateSnapshot()
		for _, it := range items {
			// 价格/库存门槛：超出区间的码不触发。
			if it.Price > p.MaxPrice || it.Stock < p.MinStock {
				continue
			}
			st := snap.PerUID[it.UID]
			if st == nil {
				candidates = append(candidates, candidate{p, it.UID, it.Price, fmt.Sprintf("新码 ¥%.2f", it.Price), it.Stock})
				continue
			}
			if st.KnownStock < 0 {
				// dead 码重新出现在列表里：值得触发（它曾被删，现在
				// 重新上架）。
				candidates = append(candidates, candidate{p, it.UID, it.Price, fmt.Sprintf("重新上架 ¥%.2f", it.Price), it.Stock})
				continue
			}
			// 补货：观察库存相对基线的真增量（基线 = 上次观察值；
			// 自身消耗只会让观察值下降或不变，不会误报）。
			if it.Stock > st.KnownStock {
				candidates = append(candidates, candidate{p, it.UID, it.Price, fmt.Sprintf("补货 %d→%d", st.KnownStock, it.Stock), it.Stock})
				continue
			}
			// 降价进入区间：上一轮此码因价格超限被过滤（LastPrice >
			// MaxPrice），这一轮降到区间内且库存达标——用户核心场景
			//（"出现比设定价格更低的对接码"）。
			if st.LastPrice > p.MaxPrice && it.Price <= p.MaxPrice && it.Stock >= p.MinStock {
				candidates = append(candidates, candidate{p, it.UID, it.Price, fmt.Sprintf("降价 ¥%.2f→¥%.2f", st.LastPrice, it.Price), it.Stock})
				continue
			}
			// 其余情况（价格区间内小幅波动、库存下降=别人消耗）：静默，
			// 由 syncSnapshot 更新基线。
		}
	}
	syncAll := func() {
		for _, f := range fetched {
			w.syncSnapshot(f.proj, f.items)
		}
	}
	if len(candidates) == 0 {
		syncAll() // 无事件：观测值正常入账
		w.clearTransientPause()
		return
	}

	// 多候选取价格最低的一个（跨项目比价）。
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].price < candidates[j].price })
	c := candidates[0]

	// 额度判定。不足时：暂停派活，基线不同步（充值后事件仍成立）。
	snap := w.stateSnapshot()
	cost := round2(c.price * float64(cfg.want()))
	if snap.BudgetRemaining < cost {
		w.setPaused(fmt.Sprintf("额度耗尽（剩 ¥%.2f，本次需 ¥%.2f：最低可用码 %s ¥%.2f）——充值后自动恢复",
			snap.BudgetRemaining, cost, c.uid, c.price))
		return
	}
	w.clearTransientPause()

	// 派活前看互斥：手动任务在跑就跳过（不排队——用户正在主动加号，
	// 值班员插队会打架）。基线同样保留（下轮再判）。
	if w.en.Status().Running {
		w.log("有手动加号任务在跑，跳过本轮触发（%s ¥%.2f，下轮再看）", c.uid, c.price)
		return
	}

	// 派活：基线同步（OwnPending 归零）→ 执行。
	syncAll()
	w.executeTrigger(ctx, cfg, c.proj, c.uid, c.price, c.ev)
}

// syncSnapshot 把一轮拉到的码表同步进 per_uid 基线。
// 基线 = 本次观察值。上游真回升时观察值上升（下轮触发补货）；自身
// 消耗让观察值下降或不变（天然不误报）。
func (w *UIDWatcher) syncSnapshot(p WatchProject, items []haozhumah5.UIDItem) {
	seen := map[string]bool{}
	w.mu.Lock()
	if w.state.PerUID == nil {
		w.state.PerUID = map[string]*watchUIDState{}
	}
	for _, it := range items {
		seen[it.UID] = true
		st := w.state.PerUID[it.UID]
		if st == nil {
			w.state.PerUID[it.UID] = &watchUIDState{KnownStock: it.Stock, LastPrice: it.Price}
		} else {
			// dead 重新出现且满足该项目的价格/库存门槛：重建基线
			//（否则每轮都会当成"重新上架"重复触发）。不满足门槛的
			// 保持 dead，等它达标或消失。
			if st.KnownStock < 0 {
				if it.Price <= p.MaxPrice && it.Stock >= p.MinStock {
					st.KnownStock = it.Stock
				}
			} else {
				// 基线 = 本次观察值。上游真回升（别人释放/官方补充）
				// 时观察值上升，下轮触发；自身消耗只会让观察值下降或
				// 不变，天然不会误报补货。
				st.KnownStock = it.Stock
			}
			st.LastPrice = it.Price
		}
	}
	// 消失的码（上游删了）：标记 dead（KnownStock=-1），tick 里跳过；
	// 重新出现时按"重新上架"触发。
	for uid := range w.state.PerUID {
		if !seen[uid] && strings.HasPrefix(uid, p.Sid+"-") {
			w.state.PerUID[uid].KnownStock = -1
		}
	}
	w.mu.Unlock()
}

// executeTrigger 派活：加入账户 → 临时切换取号参数 → 跑一轮 → 记账 → 恢复。
func (w *UIDWatcher) executeTrigger(ctx context.Context, cfg WatchConfig, p WatchProject, uid string, price float64, ev string) {
	want := cfg.want()
	w.log("触发：%s %s（项目 %s）——加 %d 个号，预算 ¥%.2f", uid, ev, p.Sid, want, round2(price*float64(want)))

	// 1) 确保码已加入账户（官方 API 只认已加入的；幂等）。
	if err := w.h5.AddUID(ctx, uid); err != nil {
		w.log("加入对接码 %s 失败：%v（本轮放弃）", uid, err)
		return
	}
	w.mu.Lock()
	w.state.TriggerTotal++ // 触发即计数（全失败的轮也算触发过）
	if st := w.state.PerUID[uid]; st != nil {
		st.Joined = true
	}
	w.mu.Unlock()

	// 2) 快照当前取号参数，切换到触发码。
	sid, author, singleUID, poolUIDs, isp := w.en.FetchSnapshot()
	w.en.SetSid(p.Sid)
	w.en.UpdateFetchOptions(author, "", []string{uid}, isp)
	w.mu.Lock()
	w.running = true
	w.mu.Unlock()
	defer func() {
		w.en.SetSid(sid)
		w.en.UpdateFetchOptions(author, singleUID, poolUIDs, isp)
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()

	// 3) 跑任务。AutoRunWith 报"已在进行中"（竞态：手动任务刚启动）时放弃本轮。
	err := w.en.AutoRunWith(AutoRunOptions{
		Want:    want,
		Workers: cfg.workers(),
		Groups:  cfg.Groups,
	})
	if err != nil {
		w.log("启动加号失败：%v（本轮放弃）", err)
		return
	}
	// 等待任务完成：轮询 Status。AutoEnroller 没有完成回调（run 在内部
	// goroutine 里跑），这里用轮询 + ctx 感知（粒度 2s，任务通常分钟级）。
	for {
		st := w.en.Status()
		if !st.Running {
			break
		}
		select {
		case <-ctx.Done():
			// 服务停机：不再等（AutoEnroller 自己的 ctx 收尾会兜底释放）。
			// 记账照做——消耗已经发生。
			st = w.en.Status()
			if st.Running {
				// 还在跑：用当前成功数记账（保守），标记状态。
				w.charge(uid, price, st.OK, st.StopReason)
				return
			}
			break
		case <-time.After(2 * time.Second):
		}
	}
	st := w.en.Status()
	w.charge(uid, price, st.OK, st.StopReason)
}

// charge 记账：成功数 × 触发时快照价，从额度扣；更新 per_uid 消耗。
// 触发计数（TriggerTotal）在派活时就已入账——全失败的轮也要算"触发过"，
// 否则统计低估值班员的活跃度。
func (w *UIDWatcher) charge(uid string, price float64, ok int, stopReason string) {
	if ok <= 0 {
		w.log("本轮加号未成功（%s）：不扣额度", stopReason)
		return
	}
	cost := round2(price * float64(ok))
	w.mu.Lock()
	w.state.BudgetRemaining = round2(w.state.BudgetRemaining - cost)
	w.state.SpentTotal = round2(w.state.SpentTotal + cost)
	w.state.EnrolledTotal += ok
	if st := w.state.PerUID[uid]; st != nil {
		st.Consumed += ok
	}
	w.mu.Unlock()
	w.persist()
	w.log("完成：%d 个号（%s）花费 ¥%.2f，额度余 ¥%.2f", ok, uid, cost, w.stateSnapshot().BudgetRemaining)
	if stopReason != "" {
		w.log("任务提前结束：%s", stopReason)
	}
}

// setPaused / clearTransientPause 暂停原因管理。"额度耗尽"/"H5 会话失效"
// 是需要用户动作的持续态；其它轮次的瞬时错误不写这里（避免误导）。
func (w *UIDWatcher) setPaused(reason string) {
	w.mu.Lock()
	if w.state.PausedReason != reason {
		w.state.PausedReason = reason
		w.mu.Unlock()
		w.log("监控暂停：%s", reason)
		w.persist()
		return
	}
	w.mu.Unlock()
}

func (w *UIDWatcher) clearTransientPause() {
	w.mu.Lock()
	w.state.PausedReason = ""
	w.mu.Unlock()
}

// persist 状态落盘（原子写，与 config 同链路）。调用点都在 mu 释放后。
func (w *UIDWatcher) persist() {
	if w.statePath == "" {
		return
	}
	w.mu.Lock()
	w.state.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
	snap := w.state.clone()
	w.mu.Unlock()
	out, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return
	}
	if err := writeFileAtomic(w.statePath, append(out, '\n'), 0600); err != nil {
		log.Printf("[uid-watcher] 状态落盘失败：%v", err)
	}
}

// round2 金额两位小数（浮点直接加减会留 1e-17 残尾，落盘和回显都难看）。
func round2(f float64) float64 {
	return math.Round(f*100) / 100
}

// watchSIDPattern 项目数字 ID。
var watchSIDPattern = regexp.MustCompile(`^[0-9]{1,10}$`)

// watchHexPattern 16 位 hex 项目会话标识（type=8 用）。
var watchHexPattern = regexp.MustCompile(`^[0-9a-fA-F]{16}$`)
