// Package scheduler 定时任务：签到 / 活跃上报 / 猫猫旅行 / token keepalive / 夜猫子
// 五类独立排程。签到成功后重新查余额，余额 > 0 的冷却账号自动解冻；签到收尾跑
// 连登管家（档位兑换 + 抽奖）。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/metricsstore"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即五类任务都启用
// （hours 回落默认），与引入开关前的行为一致（老调用方/老测试无需改动）。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int // 默认 [9, 21]
	TravelHours    []int // 默认 [9,21]：一趟派出 + 一趟领奖闭环
	ActivityHours  []int // 默认 [10]：对话活跃上报（点亮连登 + 解锁领养）
	KeepaliveHours []int // 默认 [22]
	BlackcatHours  []int // 默认 [23]：夜猫子（23:00–08:00 计数窗口）
	RequestCredits metricsstore.Backend

	// *Disabled 显式关闭对应排程（config 的 schedule.*_enabled=false）。
	CheckinDisabled   bool
	TravelDisabled    bool
	ActivityDisabled  bool
	KeepaliveDisabled bool
	BlackcatDisabled bool
}

// Scheduler 调度器。
type Scheduler struct {
	// mu/adoptTried 领养当日失败记录：uid → 自然日（CST）。门槛未达的账号当日不再
	// 重试，避免同日多趟对上游重试轰炸；进程重启即清零（无需持久化）。
	mu         sync.Mutex
	adoptTried map[string]string

	// schedMu 保护排程参数（时点/开关）；Reconfigure 可在运行期热改（控制台保存配置时调用）。
	schedMu sync.Mutex
	cfg     Config

	// wake 「排程已变，立即重算」通知（Reconfigure 消费）。
	wake chan struct{}
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.TravelHours) == 0 {
		cfg.TravelHours = []int{9, 21}
	}
	if len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	if len(cfg.BlackcatHours) == 0 {
		cfg.BlackcatHours = []int{23}
	}
	return &Scheduler{cfg: cfg, adoptTried: make(map[string]string), wake: make(chan struct{}, 1)}
}

// Reconfigure 热更新排程参数（控制台保存配置后调用）：改时点/开关并唤醒运行中的
// 循环重算。空 hours 视为「未配置」保留原值（与 config.normalize 的回落语义一致）。
func (s *Scheduler) Reconfigure(checkinHours, travelHours, activityHours, keepaliveHours, blackcatHours []int,
	checkinDisabled, travelDisabled, activityDisabled, keepaliveDisabled, blackcatDisabled bool) {
	s.schedMu.Lock()
	if len(checkinHours) > 0 {
		s.cfg.CheckinHours = append([]int(nil), checkinHours...)
	}
	if len(travelHours) > 0 {
		s.cfg.TravelHours = append([]int(nil), travelHours...)
	}
	if len(activityHours) > 0 {
		s.cfg.ActivityHours = append([]int(nil), activityHours...)
	}
	if len(keepaliveHours) > 0 {
		s.cfg.KeepaliveHours = append([]int(nil), keepaliveHours...)
	}
	if len(blackcatHours) > 0 {
		s.cfg.BlackcatHours = append([]int(nil), blackcatHours...)
	}
	s.cfg.CheckinDisabled = checkinDisabled
	s.cfg.TravelDisabled = travelDisabled
	s.cfg.ActivityDisabled = activityDisabled
	s.cfg.KeepaliveDisabled = keepaliveDisabled
	s.cfg.BlackcatDisabled = blackcatDisabled
	s.schedMu.Unlock()
	poke(s.wake)
}

// UpdateSchedule 兼容入口：只改签到/保活时点（老调用方语义不变）。
func (s *Scheduler) UpdateSchedule(checkinHours, keepaliveHours []int) {
	s.Reconfigure(checkinHours, nil, nil, keepaliveHours, nil, false, false, false, false, false)
}

// poke 非阻塞发一次唤醒信号（已有待处理信号则忽略，语义等价）。
func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// snapshot 排程参数快照（schedMu 下读，与 Reconfigure 的并发写隔离）。
func (s *Scheduler) snapshot() Config {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskTravel
	taskActivity
	taskKeepalive
	taskBlackcat
	// taskCredit 对账刷新（固定 5 分钟 ticker，不参与小时制 nextWake）。
	taskCredit
)

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务。
// 多类任务若配到同一小时（如签到与旅行都含 9），该时刻多类任务需一并执行。
// 已显式禁用的任务不进候选。排程参数在 schedMu 下快照。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	cfg := s.snapshot()
	type slot struct {
		at   time.Time
		kind taskKind
	}
	var slots []slot
	if !cfg.CheckinDisabled {
		slots = append(slots, slot{nextFire(now, cfg.CheckinHours), taskCheckin})
	}
	if !cfg.TravelDisabled {
		slots = append(slots, slot{nextFire(now, cfg.TravelHours), taskTravel})
	}
	if !cfg.ActivityDisabled {
		slots = append(slots, slot{nextFire(now, cfg.ActivityHours), taskActivity})
	}
	if !cfg.KeepaliveDisabled {
		slots = append(slots, slot{nextFire(now, cfg.KeepaliveHours), taskKeepalive})
	}
	if !cfg.BlackcatDisabled {
		slots = append(slots, slot{nextFire(now, cfg.BlackcatHours), taskBlackcat})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// Run 主循环，阻塞直到 ctx 取消。
// Reconfigure 触发 wake 时提前唤醒重算（新时点/开关立即生效）。
func (s *Scheduler) Run(ctx context.Context) {
	creditTicker := time.NewTicker(5 * time.Minute)
	defer creditTicker.Stop()
	for {
		next, kinds := s.nextWake(time.Now())
		if next.IsZero() {
			// 五类任务全部禁用：不空转，等重排通知（在线改配置重新启用）或退出信号。
			// creditTicker 照旧（对账刷新不属于小时制排程）。
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
				continue
			case <-creditTicker.C:
				s.RunRequestCreditRefreshNow()
			}
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			timer.Stop() // 排程已变：重算下一次唤醒
		case <-timer.C:
			// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
			// 唤醒时全部并行派发：每类一个 goroutine，慢任务族（如活跃上报
			// 多号 × 间隔 ≈ 数分钟睡眠）不再阻塞同槽其他任务族；返回前等全部
			// 任务收尾（下一轮 nextWake 照旧从"现在"起算）。
			s.runBatch(ctx, kinds)
		case <-creditTicker.C:
			s.RunRequestCreditRefreshNow()
		}
	}
}

// runBatch 并行派发一批任务（同一唤醒时刻的多类任务），等全部完成返回。
// ctx 取消时由各任务内部的可取消等待快速收尾。
func (s *Scheduler) runBatch(ctx context.Context, kinds []taskKind) {
	var wg sync.WaitGroup
	for _, k := range kinds {
		wg.Add(1)
		go func(k taskKind) {
			defer wg.Done()
			switch k {
			case taskCheckin:
				s.RunCheckinNow()
			case taskTravel:
				s.RunTravelNow()
			case taskActivity:
				s.RunActivityNow(ctx)
			case taskKeepalive:
				s.RunKeepaliveNow()
			case taskBlackcat:
				s.RunBlackcatNow()
			}
		}(k)
	}
	wg.Wait()
}

// sleepCtx 可取消的等待：ctx 取消立即返回 false（优雅停机不必等限速睡醒），
// 等满返回 true。d<=0 立即放行。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// RunRequestCreditRefreshNow reconciles recent request logs with the
// authoritative web billing meter. It is intentionally asynchronous and never
// blocks an in-flight model request.
func (s *Scheduler) RunRequestCreditRefreshNow() {
	cfg := s.snapshot()
	if cfg.RequestCredits == nil {
		return
	}
	logs, err := cfg.RequestCredits.QueryRequests(metricsstore.RequestFilter{Limit: 200})
	if err != nil {
		log.Printf("request usage logs: %v", err)
		return
	}
	if len(logs) == 0 {
		return
	}
	start := time.Now().Add(-48 * time.Hour)
	end := time.Now().Add(2 * time.Hour)
	for _, st := range cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.Snapshot().AccessToken == "" {
			continue
		}
		rows, _, err := cfg.Upstream.UserRequestUsage(a, start, end, 1, 200)
		if err != nil {
			log.Printf("request-usage %s: %v", st.UID, err)
			continue
		}
		for _, row := range rows {
			if row.RequestID != "" {
				_ = cfg.RequestCredits.ReconcileRequestCredit(row.RequestID, row.Credit)
			}
		}
	}
}

func contains(hours []int, h int) bool {
	for _, v := range hours {
		if v == h {
			return true
		}
	}
	return false
}

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
// global 账号无签到体系，直接跳过（不发起任何上游调用，避免风控）。
// 末尾追加连登管家（streak.go）：可兑换档位自动兑换 + 抽奖次数自动抽完——
// 连登兑换按天数解锁，挂在每日签到后即「到天数那天自动完成兑换→抽奖闭环」。
func (s *Scheduler) RunCheckinNow() {
	cfg := s.snapshot()
	for _, st := range cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.Snapshot().RefreshToken == "" {
			continue
		}
		if a.Region() == "global" {
			continue
		}
		if err := cfg.Upstream.DailyCheckin(a); err != nil {
			// "今天已签到"是幂等成功（上游对重复签到返回 code!=0），不当失败打 error 行。
			if upstream.IsAlreadyCheckin(err) {
				log.Printf("checkin %s: 今天已签到（幂等）", st.UID)
			} else {
				log.Printf("checkin %s: %v", st.UID, err)
			}
			// 其余业务错误也继续走余额查询
		}
		_ = s.refreshAccountCredits(st.UID, a)
	}
	s.RunStreakBonusNow()
	// 开学季活动（school.go）：每日刷新，搭签到便车；活动期外 in_period=false
	// 自动跳过（幂等，无需下线代码）。
	s.RunSchoolNow()
}

// RunCreditRefreshNow refreshes upstream credit counters without performing a
// daily check-in. It is safe to run asynchronously during service startup.
func (s *Scheduler) RunCreditRefreshNow() {
	cfg := s.snapshot()
	for _, st := range cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.Snapshot().RefreshToken == "" {
			continue
		}
		_ = s.refreshAccountCredits(st.UID, a)
	}
}

// refreshAccountCredits 拉取上游余额快照并写入账号池。
// 返回 error 供按账号调用方区分"签到成功但余额查询失败"。
func (s *Scheduler) refreshAccountCredits(uid string, a *auth.Auth) error {
	cfg := s.snapshot()
	resource, err := cfg.Upstream.UserResourceDetails(a)
	if err != nil {
		log.Printf("user-resource %s: %v", uid, err)
		return err
	}
	cfg.Pool.SetCreditDetail(uid, pool.CreditDetail{
		Remaining:           resource.Remaining,
		CapacitySize:        resource.CapacitySize,
		CapacityRemain:      resource.CapacityRemain,
		CapacityUsed:        resource.CapacityUsed,
		CycleCapacitySize:   resource.CycleCapacitySize,
		CycleCapacityRemain: resource.CycleCapacityRemain,
		CycleCapacityUsed:   resource.CycleCapacityUsed,
	})
	return nil
}

// AccountResult 单个账号的签到/保活结果，供控制台按账号展示。
type AccountResult struct {
	UID    string
	OK     bool
	Detail string // 失败原因，OK 时为空
}

// CheckinAccount 对单个账号执行签到并刷新余额，供控制台"单账号签到"使用。
// 语义与 RunCheckinNow 的单账号分支完全一致：DailyCheckin 的业务错误
// （如"今日已签到"）不算失败，只要随后的余额查询成功即视为成功。
func (s *Scheduler) CheckinAccount(uid string) AccountResult {
	a := s.snapshot().Pool.AuthByUID(uid)
	if a == nil {
		return AccountResult{UID: uid, Detail: "账号不存在"}
	}
	creds := a.Snapshot()
	if creds.RefreshToken == "" {
		return AccountResult{UID: uid, Detail: "账号缺少 refresh token"}
	}
	if a.Region() == "global" {
		return AccountResult{UID: uid, Detail: "海外版账号无签到体系"}
	}
	if err := s.snapshot().Upstream.DailyCheckin(a); err != nil {
		// 已签到等业务错误也继续走余额查询，仅记录日志。
		log.Printf("checkin %s: %v", uid, err)
	}
	if err := s.refreshAccountCredits(uid, a); err != nil {
		return AccountResult{UID: uid, Detail: err.Error()}
	}
	return AccountResult{UID: uid, OK: true}
}

// KeepaliveAccount 对单个账号刷新 token，供控制台"单账号保活"使用。
// 与 RunKeepaliveNow 的单账号分支一致：session 死亡时自动禁用该账号。
func (s *Scheduler) KeepaliveAccount(uid string) AccountResult {
	a := s.snapshot().Pool.AuthByUID(uid)
	if a == nil {
		return AccountResult{UID: uid, Detail: "账号不存在"}
	}
	if a.Snapshot().RefreshToken == "" {
		return AccountResult{UID: uid, Detail: "账号缺少 refresh token"}
	}
	if err := s.snapshot().Upstream.RefreshToken(a); err != nil {
		log.Printf("keepalive %s: %v", uid, err)
		var ue *upstream.Error
		if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
			s.snapshot().Pool.Disable(uid, "12153 session dead")
		}
		return AccountResult{UID: uid, Detail: err.Error()}
	}
	if err := a.SaveAtomic(); err != nil {
		log.Printf("keepalive %s save: %v", uid, err)
		return AccountResult{UID: uid, Detail: "凭证保存失败: " + err.Error()}
	}
	return AccountResult{UID: uid, OK: true}
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
func (s *Scheduler) RunKeepaliveNow() {
	cfg := s.snapshot()
	for _, st := range cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.Snapshot().RefreshToken == "" {
			continue
		}
		if err := cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", st.UID, err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				cfg.Pool.Disable(st.UID, "12153 session dead")
			}
			continue
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", st.UID, err)
		}
	}
}

// ---------------------------------------------------------------------------
// 活跃上报（growth.go ReportChatActivity 的排程侧）
// ---------------------------------------------------------------------------

// activityAccountDelay 活跃上报账号间限速：与旅行同口径，避免上游风控。测试可置 0。
var activityAccountDelay = 800 * time.Millisecond

// RunActivityNow 立即对池内所有可用账号执行一次对话活跃上报（外部入口，无 ctx）。
// 一条上报同时点亮 growth 连登 + 解锁 first_buddy 任务。
// 上报成功后跑 streak 自检（checkActivityStreak）：回读连登天数，发现
// 「上报 200 但 streak 没涨」的静默丢弃（只读 oracle，不做重试）。
func (s *Scheduler) RunActivityNow(ctx context.Context) {
	cfg := s.snapshot()
	first := true
	for _, st := range cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.Snapshot().AccessToken == "" {
			continue
		}
		if a.Region() == "global" {
			continue // global 无任务中心/活跃体系，不发起任何上游调用
		}
		if !first {
			if !sleepCtx(ctx, activityAccountDelay) {
				return // 优雅停机：不等限速睡满，剩余账号下轮再报
			}
		}
		first = false
		cid := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
		if err := cfg.Upstream.ReportChatActivity(a, cid, ""); err != nil {
			log.Printf("activity %s: %v", st.UID, err)
			continue
		}
		s.checkActivityStreak(a) // 上报成功 → 回读 streak 自检
	}
}

// checkActivityStreak 上报成功后回读连登天数（只读 oracle，发现静默失败）。
// 上报 200 ≠ streak 计分——需要回读验证闭环。
// 异常检测口径：days==0 → warn（report OK but streak.days=0 (silent drop?)）；
// GET 失败 → warn 但不影响主流程（上报本身已成功，按天幂等，不做重试）。
// 日志每号一行、一眼可 grep：`activity %s: streak days=%d`（成功也打，方便对账）。
// 返回 true 表示「上报 OK 但 streak 可疑」（days==0 或回读失败）。
func (s *Scheduler) checkActivityStreak(a *auth.Auth) bool {
	days, err := s.snapshot().Upstream.GrowthStreak(a)
	if err != nil {
		log.Printf("activity %s: streak check failed (report OK): %v", a.Snapshot().UID, err)
		return true
	}
	if days == 0 {
		log.Printf("activity %s: report OK but streak.days=0 (silent drop?)", a.Snapshot().UID)
		return true
	}
	log.Printf("activity %s: streak days=%d", a.Snapshot().UID, days)
	return false
}
