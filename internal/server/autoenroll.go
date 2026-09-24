package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/groups"
	"workbuddy2api/internal/haozhuma"
	"workbuddy2api/internal/smslogin"
)

// AutoEnroller 自动加号：豪猪取号 → 本服务短信直登链 → 账号落盘。
//
// 计费前提：豪猪只在**收码成功**时扣费，取号后收不到码不扣费。
// 因此下面的终止条件不是"省钱"，而是"别浪费时间/别在坏通道上打转"：
//   - 成功数达到目标
//   - 豪猪余额不足（低于单次成功成本时停：取到号也付不了款）
//   - 无号可取 / 项目不存在 / 账号被禁（重试无意义的致命错误）
//   - 连续失败达到熔断阈值（通道或接收率崩了，继续跑也不会有结果）
//   - token 失效：自动重登一次，重登后仍失败则终止
type AutoEnroller struct {
	sms *smslogin.Manager
	hzm *haozhuma.Client
	// sid 豪猪项目 ID。运行时可改（SetSid）：换对接项目不停服。
	// 读取点（GetPhone/GetMessage/Release/Blacklist）都在 a.mu 内或
	// 单次调用内快照，改号对在途号码的影响见 SetSid 注释。
	sid     string
	persist func(accountCredential, string) (map[string]any, int, error)
	find    func(mobile string) (nickname string, exists bool)
	// setAccountGroups 分组登记回调（handler 注入）。
	setAccountGroups func(uid string, groups []string) error
	// onAccountEnrolled 加号成功后的联动回调（handler 注入；nil = 未启用）。
	// 用于「注册后自动跑成长任务」：异步执行，不阻塞加号流水线。
	onAccountEnrolled func(uid string)
	// onUIDsDrained 对接码池自动生命周期回调（handler 注入；nil = 未启用）。
	// GetPhone 把失效码移出轮换池后调用：上层负责从 config.json 的
	// sms.haozhuma.uids 删掉它们 + 经 H5 type=41 从豪猪账户移出。
	onUIDsDrained func(uids []string)

	mu      sync.Mutex
	running bool
	logs    []string
	started time.Time
	// stats
	//
	// attempts 只统计**跑完**的尝试（成功 + 失败），被取消中断的不算，
	// 否则中途停止会把半截尝试算成失败，成功率看起来比实际差。
	// consumed 统计**实际取到的号**（含被中断的），它才是"消耗了多少号码"。
	attempts   int
	ok         int
	fail       int
	consumed   int
	stopReason string
	workers    int
	// retryDelay 两次尝试之间的间隔（测试里调短）。0 时用默认 5s。
	retryDelay time.Duration
	// minBalance 豪猪余额低于此值（元）时不再开始新的取号。只在**收码成功**
	// 时扣费，所以留出够付一次成功的钱即可；比这更低时收到码也扣不了费，
	// 白等。0 = 关闭余额保护。启动时从环境变量 AUTO_ENROLL_MIN_BALANCE /
	// 配置文件 autoenroll.min_balance 初始化（配置文件优先），运行期可经
	// SetLimits 热改（WebUI「高级设置」）。
	minBalance float64
	// consecutiveFails 连续失败熔断阈值。单号收不到码很正常（接收率就是有
	// 概率），但连续这么多个都失败说明通道坏了或项目被限。失败不扣费，
	// 阈值主要防"浪费时间"。运行期可经 SetLimits 热改。
	consecutiveFails int
	// pollTimeout 单个号等验证码的时长（测试里调短）。0 时用默认 90s。
	// pollCount > 0 时以次数为准，本字段只作为推导次数的后备。
	pollTimeout time.Duration
	// pollInterval 轮询间隔（测试里调短）。0 时用默认 5s。
	pollInterval time.Duration
	// pollCount 单个号轮询收码的**次数**上限。0 时由 pollTimeout 推导
	// （默认 90s / 5s = 18 次）。
	//
	// 次数比时长更贴近实际操作：对接商质量差、短信到得晚时，用户想调的是
	// "再等多轮几次"，而不是去换算秒数。运行中要临时多轮几次也走这里。
	pollCount int

	// reloginMu 保证并发的 token 失效只触发一次重登。
	reloginMu   sync.Mutex
	lastRelogin time.Time

	// cancel 停止当前运行中的任务（由 run 注册，任务结束后清空）。
	// 没有它就只能重启容器才能停下一个跑偏的任务。
	cancel context.CancelFunc

	// runPollCount / runMaxAttempts 记录**本次运行**生效的参数，
	// 让 /status 能回显"这次到底按几次轮询在跑"，而不只是用户填了什么。
	runPollCount   int
	runMaxAttempts int
	// runGroups 本次运行新账号要登记的分组（AutoRunWith 时定格；
	// 运行期间只读）。空 = default。
	runGroups []string

	// held 本次运行中"已从豪猪取走、但还没确认释放"的号码。
	//
	// 为什么要单独记账：取号成功后就占用了豪猪的并发额度，而额度满了会返回
	// 「您的余额不足,请释放拉黑后再取号」——后续所有取号都会失败，看起来像
	// 账户没钱（其实是没释放）。tryOne 的正常路径都会调 finish 释放，但如果
	// 进程被 kill、或某次 finish 的 HTTP 请求本身失败，号就一直挂在豪猪那边
	// （实测：一次任务结束后额度仍被占着，下一轮 14 次尝试全部失败）。
	// 任务收尾时按这份账本兜底释放，保证"任务结束 = 号全部还回去"。
	held map[string]bool
	// released 已确认释放的号（从 held 移出后记在这里，用于统计与日志）。
	released int
	// ledgerPath 账本落盘路径（空 = 不落盘）。
	//
	// 落盘是为了跨**进程重启**兜底：容器重启是 SIGKILL，defer 不执行，
	// 内存里的账本直接消失，号就永远留在豪猪那边占额度。启动时读回来
	// 补释放一次（见 ReclaimOrphans）。
	ledgerPath string
	// smsDebugPath SMS 诊断日志路径（空 = 关闭）。记录完整手机号与短信
	// 原文（不脱敏），只落服务器本机文件（data/ 卷，0600），不回传控制台。
	// 上限 1 MiB 自动轮转。见 smsDebug。
	smsDebugPath string
}

// 并发默认值与上限。并发加号靠代理池撑：每个号一个独立出口 IP
// （新会话创建时绑定一次）。上限保守——腾讯风控对同批次注册敏感，
// 并发太高会整批收不到码。
const (
	defaultWorkers = 3
	maxWorkers     = 8
	// maxPollCount 单个号收码轮询次数上限。默认 18 次 × 5s = 90s；调到 36
	// 次已经是 3 分钟，远超验证码 60s 有效期，再往后只是白占着豪猪的号。
	maxPollCount = 36
	// defaultPollCount 默认轮询次数（90s / 5s），与历史行为一致。
	defaultPollCount = 18
	// maxAttemptsLimit 总尝试次数上限，防"目标 1 个但想试 5000 次"把号池抽干。
	maxAttemptsLimit = 2000
	// maxPerSuccess 每个成功号默认允许的尝试次数（自动推导上限时用）。
	maxPerSuccess = 12
	// minAttempts 总尝试次数下限，保证目标很小时也有足够重试。
	minAttempts = 20
)

// consecutiveFails 连续失败熔断阈值的默认值（AUTO_ENROLL_MAX_CONSECUTIVE
// 可覆盖）。真正的阈值存在 AutoEnroller.consecutiveFails 实例字段上，
// 运行期可经 SetLimits 热改。
var defaultConsecutiveFails = envInt("AUTO_ENROLL_MAX_CONSECUTIVE", 15)

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// minBalance 豪猪余额保护阈值的默认值（元）（AUTO_ENROLL_MIN_BALANCE
// 可覆盖；52283 腾讯项目单价 2.2 元/次）。真正的阈值存在
// AutoEnroller.minBalance 实例字段上，运行期可经 SetLimits 热改。
var defaultMinBalance = envFloat("AUTO_ENROLL_MIN_BALANCE", 2.2)

func envFloat(key string, def float64) float64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
	}
	return def
}

// normalizeRunGroups 清洗任务指定的分组列表：去空白、去重、保序；
// 空列表回落 ["default"]。有效性（分组是否存在）由 setAccountGroups
// 回调在登记时校验——启动时校验会在"任务跑着时用户改分组"的场景下
// 产生假失败。
func normalizeRunGroups(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, g := range in {
		if g = strings.TrimSpace(g); g == "" || seen[g] {
			continue
		}
		seen[g] = true
		out = append(out, g)
	}
	if len(out) == 0 {
		return []string{groups.DefaultGroup}
	}
	return out
}

type AutoEnrollStatus struct {
	Running    bool     `json:"running"`
	Attempts   int      `json:"attempts"`
	OK         int      `json:"ok"`
	Fail       int      `json:"fail"`
	Workers    int      `json:"workers,omitempty"`
	StopReason string   `json:"stop_reason,omitempty"`
	Logs       []string `json:"logs"`
	// Consumed 本次已取到的号码数（含中途停止时在途的）。
	// 与 Attempts 分开：Attempts 是跑完的尝试数，Consumed 是真实号码消耗。
	Consumed int `json:"consumed"`
	// PollCount / MaxAttempts 本次运行**实际生效**的参数（已夹到上限内）。
	// 回显生效值而不是用户输入值：用户填 999 时看到 36 才知道被夹住了。
	PollCount   int `json:"poll_count,omitempty"`
	MaxAttempts int `json:"max_attempts,omitempty"`
	// Held 仍占着豪猪额度的号码数（正常任务结束时必须为 0；
	// 大于 0 说明有号没还回去，会拖累后续取号）。
	Held int `json:"held"`
	// Released 本次已确认归还的号码数。
	Released int `json:"released"`
}

// NewAutoEnroller 组装自动加号器。persist 落盘回调必填（nil 时 tryOne 会
// 直接报错而不是 panic）；findAccountByMobile 可选，用于跳过已在池里的号；
// setAccountGroups 可选（分组存储启用时由 handler 注入），加号成功后把
// 新账号登记进任务指定的分组。
func NewAutoEnroller(sms *smslogin.Manager, hzm *haozhuma.Client, sid string,
	persist func(accountCredential, string) (map[string]any, int, error),
	findAccountByMobile func(string) (string, bool),
	setAccountGroups func(uid string, groups []string) error) *AutoEnroller {
	if persist == nil {
		persist = func(accountCredential, string) (map[string]any, int, error) {
			return nil, 500, errors.New("persist 回调未配置")
		}
	}
	if findAccountByMobile == nil {
		findAccountByMobile = func(string) (string, bool) { return "", false }
	}
	if setAccountGroups == nil {
		setAccountGroups = func(string, []string) error { return nil }
	}
	return &AutoEnroller{
		sms:              sms,
		hzm:              hzm,
		sid:              sid,
		persist:          persist,
		find:             findAccountByMobile,
		setAccountGroups: setAccountGroups,
		// 默认轮询次数可用 AUTO_ENROLL_POLL_COUNT 覆盖（部署级调参），
		// 单次任务还能再用 poll_count 覆盖它。
		pollCount: envInt("AUTO_ENROLL_POLL_COUNT", defaultPollCount),
		// 熔断/余额保护默认值：环境变量兜底（部署级），配置文件
		// autoenroll.* 与 WebUI SetLimits 优先（main 启动链 + handler）。
		minBalance:       defaultMinBalance,
		consecutiveFails: defaultConsecutiveFails,
	}
}

// SetLimits 更新余额保护阈值与连续失败熔断阈值（WebUI「高级设置」）。
// 即时生效：余额检查在下一个 worker 循环按新值判定，熔断计数在下一个
// 失败时按新阈值比较（已累计的连续失败数不清零——中途调小阈值理应
// 立刻更容易触发熔断，清零反而把它推迟了）。任务运行中调用安全。
func (a *AutoEnroller) SetLimits(minBalance float64, consecutiveFails int) {
	a.mu.Lock()
	a.minBalance = minBalance
	a.consecutiveFails = consecutiveFails
	a.mu.Unlock()
}

// Limits 返回当前生效的余额保护阈值与熔断阈值（GET config / 状态回显用）。
func (a *AutoEnroller) Limits() (minBalance float64, consecutiveFails int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.minBalance, a.consecutiveFails
}

// currentMinBalance / currentConsecutiveFails 是 run 循环的每轮快照读。
// 单独成函数（而不是直接 a.mu.Lock）是因为 run 的 worker 循环里已有
// 多处持锁段，抽方法保证读法唯一、不会某天改出双锁死锁。
func (a *AutoEnroller) currentMinBalance() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.minBalance
}

func (a *AutoEnroller) currentConsecutiveFails() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.consecutiveFails
}

// SetRetryDelay 更新两次尝试之间的间隔（默认 5s）。0 = 恢复默认。
func (a *AutoEnroller) SetRetryDelay(d time.Duration) {
	if d < 0 {
		d = 0
	}
	a.mu.Lock()
	a.retryDelay = d
	a.mu.Unlock()
}

// RetryDelay 返回当前生效的两次尝试间隔（0 = 默认 5s，回显时由调用方
// 翻译成具体秒数）。
func (a *AutoEnroller) RetryDelay() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.retryDelay
}

// currentSid 读取当前项目 ID 快照（mu 保护，与 SetSid 并发安全）。
func (a *AutoEnroller) currentSid() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sid
}

// SetOnAccountEnrolled 注入加号成功联动回调（成长任务自动化）。
// nil = 关闭联动。mu 保护（运行期可换）。
func (a *AutoEnroller) SetOnAccountEnrolled(fn func(uid string)) {
	a.mu.Lock()
	a.onAccountEnrolled = fn
	a.mu.Unlock()
}

// enrolledCallback 读取联动回调快照（mu 下读，与 SetOnAccountEnrolled 并发安全）。
func (a *AutoEnroller) enrolledCallback() func(uid string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.onAccountEnrolled
}

// SetOnUIDsDrained 注入对接码池出池回调（自动生命周期管理）。
// GetPhone 移出失效码后调用；nil = 关闭（码只退出本地轮换池，不动
// config 与豪猪账户）。mu 保护（运行期可换）。
func (a *AutoEnroller) SetOnUIDsDrained(fn func(uids []string)) {
	a.mu.Lock()
	a.onUIDsDrained = fn
	a.mu.Unlock()
}

// uidsDrainedCallback 读取出池回调快照。
func (a *AutoEnroller) uidsDrainedCallback() func(uids []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.onUIDsDrained
}

// SetSid 运行时切换豪猪项目 ID（WebUI「豪猪项目」设置）。
// 对在途任务的影响：已取到的号继续按 tryOne 开头快照的旧 sid 收码/
// 释放/拉黑（号码归属项目，跨 sid 释放报"手机号不存在"——releaseAllHeld
// 的失败留账本 + reclaim 的 goneUpstream 判定兜底，无额度泄漏）。
// 新取号立刻用新 sid。任务运行中允许切换、不中断。
func (a *AutoEnroller) SetSid(sid string) {
	sid = strings.TrimSpace(sid)
	a.mu.Lock()
	defer a.mu.Unlock()
	if sid != "" && a.sid != sid {
		msg := time.Now().Format("15:04:05 ") + maskPhonesIn(fmt.Sprintf("豪猪项目 ID 已切换: %s → %s（新取号立即生效）", a.sid, sid))
		log.Printf("[auto-enroll] %s", msg)
		a.logs = append(a.logs, msg)
	}
	a.sid = sid
}

// Sid 返回当前项目 ID（观测/测试用）。
func (a *AutoEnroller) Sid() string {
	return a.currentSid()
}

// logf 记录自动加号日志。所有日志都经过 maskPhonesIn，因为豪猪取号返回的
// **手机号本身就是账号昵称**（见 handler 的 findAccountByMobile 注释），
// 而这段日志会原样通过 GET /admin/account/sms/auto-enroll 回给控制台并展示。
// 号码已经付过费，泄露等于把资产清单交出去，所以在唯一的出口统一脱敏，
// 而不是逐个调用点去改（那样迟早会漏一个）。
func (a *AutoEnroller) logf(format string, args ...any) {
	msg := time.Now().Format("15:04:05 ") + maskPhonesIn(fmt.Sprintf(format, args...))
	log.Printf("[auto-enroll] %s", msg)
	a.mu.Lock()
	a.logs = append(a.logs, msg)
	if len(a.logs) > 200 {
		a.logs = a.logs[len(a.logs)-100:]
	}
	a.mu.Unlock()
}

func (a *AutoEnroller) Status() AutoEnrollStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return AutoEnrollStatus{
		Running:     a.running,
		Attempts:    a.attempts,
		OK:          a.ok,
		Fail:        a.fail,
		Consumed:    a.consumed,
		Workers:     a.workers,
		StopReason:  a.stopReason,
		Logs:        append([]string(nil), a.logs...),
		PollCount:   a.runPollCount,
		MaxAttempts: a.runMaxAttempts,
		Held:        len(a.held),
		Released:    a.released,
	}
}

// AppendLog 追加一条控制台可见日志（handler 层入口）。走 logf 的脱敏链。
func (a *AutoEnroller) AppendLog(format string, args ...any) {
	a.logf(format, args...)
}

// Balance 查当前豪猪余额（元）；查询失败返回 -1 和错误。
func (a *AutoEnroller) Balance(ctx context.Context) (float64, error) {
	return a.hzm.Balance(ctx)
}

// EnrollAccountSummary 账户卡片数据：一次调用拿全（WebUI「豪猪账户」卡片，
// GET /admin/account/sms/haozhuma/summary 的响应体）。
type EnrollAccountSummary struct {
	Balance     float64 `json:"balance"`
	Occupied    int     `json:"occupied"`   // 平台侧占用号数；-1 = 未知
	HeldLocal   int     `json:"held_local"` // 本地账本"仍占着额度"的号数
	Sid         string  `json:"sid"`
	UID         string  `json:"uid"` // 当前钉死的对接码（空 = 平台自动分配）
	ISP         string  `json:"isp,omitempty"`
	Author      string  `json:"author,omitempty"`
	MinBalance  float64 `json:"min_balance"`
	ConsecFails int     `json:"consecutive_fails"`
	Running     bool    `json:"running"`
}

// Summary 账户概览（余额/占用/本地账本/当前取号配置）。hzm 为 nil 时
// 返回 ErrNotConfigured（理论上不会发生：AutoEnroller 只在有豪猪配置时组装）。
func (a *AutoEnroller) Summary(ctx context.Context) (EnrollAccountSummary, error) {
	a.mu.Lock()
	base := EnrollAccountSummary{
		HeldLocal:   len(a.held),
		Sid:         a.sid,
		MinBalance:  a.minBalance,
		ConsecFails: a.consecutiveFails,
		Running:     a.running,
	}
	hzm := a.hzm
	a.mu.Unlock()
	if hzm == nil {
		return base, errors.New("豪猪客户端未配置")
	}
	s, err := hzm.Summary(ctx)
	if err != nil {
		return base, err
	}
	base.Balance = s.Balance
	base.Occupied = s.Occupied
	base.UID = hzm.UID()
	base.ISP = hzm.ISP
	base.Author = hzm.Author
	return base, nil
}

// ErrBusyAuth 鉴权更换被拒：任务运行中取号/释放必须同一客户端身份，
// 换客户端会让在途号码的收码/释放打到新 token 上（新 token 不认识这些号）。
var ErrBusyAuth = errors.New("自动加号任务运行中不可更换豪猪鉴权，等任务结束后再试")

// ReplaceClient 更换豪猪客户端（WebUI 改鉴权后调用）。
// 调用方（handler）负责先用新客户端验证（login/getSummary 成功）再替换；
// 这里只做运行态保护。失败不回滚——调用方验证过才进来，走到这里说明
// 新客户端至少能通过一次完整调用。
func (a *AutoEnroller) ReplaceClient(c *haozhuma.Client) error {
	if c == nil {
		return errors.New("新客户端为 nil")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return ErrBusyAuth
	}
	a.hzm = c
	return nil
}

// UpdateFetchOptions 更新取号策略（对接方标识/对接码/轮换池/运营商优先级）。
// 任务运行中允许（与 SetSid 同语义）：在途号码按 tryOne 开头快照的旧值
// 收尾，新取号立刻用新值。
//
// uids 非空时轮换池优先（单 uid 忽略）；uids 为 nil 保持池现值（未提供），
// 空切片清空池退回单 uid。
func (a *AutoEnroller) UpdateFetchOptions(author, uid string, uids []string, isp string) {
	a.mu.Lock()
	hzm := a.hzm
	a.mu.Unlock()
	if hzm == nil {
		return
	}
	hzm.Author = strings.TrimSpace(author)
	hzm.SetUID(strings.TrimSpace(uid))
	if uids != nil {
		hzm.SetUIDs(uids)
	}
	hzm.ISP = strings.TrimSpace(isp)
}

// FetchSnapshot 当前取号参数快照（sid/author/单码/轮换池/isp）。
// UIDWatcher 派活前快照、收工后恢复用——参数是"用户配置的资产"，
// 值班轮临时覆盖必须原样还回来。
func (a *AutoEnroller) FetchSnapshot() (sid, author, uid string, uids []string, isp string) {
	a.mu.Lock()
	hzm := a.hzm
	sid = a.sid
	a.mu.Unlock()
	if hzm == nil {
		return sid, "", "", nil, ""
	}
	return sid, hzm.Author, hzm.UID(), hzm.UIDs(), hzm.ISP
}

// AutoRunOptions 一次自动加号任务的参数。
//
// 零值成员一律取默认值，所以调用方只填用户显式给的部分即可——这样新增参数
// 不会打破既有调用点。
type AutoRunOptions struct {
	// Want 目标成功数（必填，>0）。
	Want int
	// Workers 并发数。<=0 用 defaultWorkers，>maxWorkers 夹住。
	Workers int
	// PollCount 单个号收码轮询次数。<=0 用 a.pollCount（默认 18 次 = 90s）。
	PollCount int
	// MaxAttempts 总尝试次数上限。<=0 用 want*12（下限 20）。
	// 对接商质量差时可能试很多次才成一个，用户需要能放宽。
	MaxAttempts int
	// Groups 加号成功后新账号登记进这些分组（多归属）。空 = default。
	Groups []string
}

// AutoRun 对外入口（保持旧签名）。已在跑时返回错误。
// workers 为并发数（<=0 用默认值）。
func (a *AutoEnroller) AutoRun(n, workers int) error {
	return a.AutoRunWith(AutoRunOptions{Want: n, Workers: workers})
}

// AutoRunWith 带完整参数启动。已在跑时返回错误。
func (a *AutoEnroller) AutoRunWith(opts AutoRunOptions) error {
	n := opts.Want
	workers := opts.Workers
	if workers <= 0 {
		workers = defaultWorkers
	}
	if workers > maxWorkers {
		workers = maxWorkers
	}
	pollCount := opts.PollCount
	if pollCount <= 0 {
		pollCount = a.pollCount
	}
	if pollCount <= 0 {
		pollCount = defaultPollCount
	}
	if pollCount > maxPollCount {
		pollCount = maxPollCount
	}
	limit := opts.MaxAttempts
	if limit <= 0 {
		limit = n * maxPerSuccess
		if limit < minAttempts {
			limit = minAttempts
		}
	}
	if limit > maxAttemptsLimit {
		limit = maxAttemptsLimit
	}
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return errors.New("自动加号已在进行中")
	}
	// 快照本次启动检查用的阈值（SetLimits 运行中热改只影响后续轮次）。
	minBalance := a.minBalance
	// 开跑前先看余额：不够一次成功就别启动，省得用户以为在跑其实取不到号。
	if bal, err := a.hzm.Balance(context.Background()); err == nil && bal >= 0 && bal < minBalance {
		a.mu.Unlock()
		return fmt.Errorf("豪猪余额不足（当前 %.2f 元，低于阈值 %.2f 元），请充值后再启动", bal, minBalance)
	}
	a.running = true
	a.attempts = 0
	a.ok = 0
	a.fail = 0
	// runGroups 在启动时定格：任务期间用户改分组列表不影响本次任务的归属。
	a.runGroups = normalizeRunGroups(opts.Groups)
	// consumed 必须一起清零：它是"本次运行取了多少号"，漏掉它会残留上一次
	// 任务的计数——界面显示 1 而实际已取走 5 个号。号码是花钱的资产，
	// 这个数错得让人以为没消耗。
	a.consumed = 0
	// released 是"本次运行归还了多少号"的计数，每次重置。
	a.released = 0
	// 注意：账本 a.held **不在这里清空**。
	//
	// 它是"我们仍认为占着豪猪额度"的持久集合，正确性要求它跨任务存活：
	// 上一轮收尾时释放失败（或进程被杀）的号必须留着，下次启动/下次任务
	// 才能继续重试。清空等于把"这个号还没还"忘掉，而它正占着取号额度。
	// 条目只在**确认释放成功**（markReleased）时移除。
	a.logs = nil
	a.stopReason = ""
	a.workers = workers
	a.runPollCount = pollCount
	a.runMaxAttempts = limit
	// ctx/cancel 必须在置 running 之前就绪：否则"启动后立刻点停止"会
	// 撞上 a.cancel 还是 nil，Stop 静默失效（用户以为停了其实还在跑）。
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			// 收尾兜底释放：必须在 running 仍为 true 时做完。新的任务此时还
			// 起不来（AutoRun 会返回"已在进行中"），所以不会出现"我们在释放、
			// 它同时在取号"的额度竞争。
			a.releaseAllHeld()
			a.mu.Lock()
			a.running = false
			a.cancel = nil
			a.mu.Unlock()
		}()
		// 开跑前先清掉历史遗留：上一轮释放失败（或进程被杀）的号还占着豪猪
		// 的取号额度，不清掉的话本轮每次取号都会失败并报
		// 「余额不足,请释放拉黑后再取号」——看着像没钱，实际是旧号没还。
		if n := a.reclaimStuck(); n > 0 {
			a.logf("先归还了 %d 个历史遗留号码，取号额度已恢复", n)
		}
		reason := a.run(ctx, n, workers, pollCount, limit)
		reason = a.outcome(reason)
		ok, attempts := a.ok, a.attempts
		if reason != "" {
			a.logf("任务终止：%s", reason)
		} else {
			a.logf("完成：成功 %d / 尝试 %d（目标 %d，并发 %d，每号轮询 %d 次）",
				ok, attempts, n, workers, pollCount)
		}
	}()
	return nil
}

// Stop 请求停止正在运行的任务。没有在跑时返回 false。
//
// 运行中的 run 循环通过 ctx 感知取消：worker 会尽快退出，已在途的会话
// 由各自的超时收尾（不会把半截账号落盘）。
func (a *AutoEnroller) Stop(reason string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running || a.cancel == nil {
		return false
	}
	if strings.TrimSpace(reason) == "" {
		reason = "用户手动停止"
	}
	a.cancel()
	// 记下来是为了让前端在轮询里看到"为什么停了"；run 返回时若已有
	// 更具体的原因会覆盖它（setStop 只写非空值）。
	if a.stopReason == "" {
		a.stopReason = reason
	}
	return true
}

// run 并发主循环：workers 个 goroutine 各自串行地"取号→发码→收码→落盘"，
// 共享一个成功计数，达到目标数就一起停。
//
// 并发安全性：每个号在 SMSLogin 里是一个独立 session，创建时就绑定自己的
// 代理出口（applyProxy 在 newSession 里调一次），会话表有 mutex。所以并发
// 加号不会共用 IP、不会串号。
//
// 终止条件（任一 worker 触发即全体停止）：
//   - 成功数达到目标（正常完成）
//   - 豪猪余额不足 / 无号可取 / 项目不存在等致命错误
//   - 连续失败达到熔断阈值（跨 worker 累计——整个通道崩了）
//   - token 失效：重登一次（用 mutex 保证只登一次），再失败才停
//
// ctx 由 AutoRun 创建并注册到 a.cancel，让 Stop() 能中断（含"启动后立刻停止"）。
//
// pollCount 是每个号收码的轮询次数，limit 是总尝试次数——由 AutoRunWith 解析
// （含默认值与上限夹取）后传入，run 本身不再做参数决策。
func (a *AutoEnroller) run(ctx context.Context, want, workers, pollCount, limit int) string {
	var (
		got         atomic.Int64 // 成功数
		used        atomic.Int64 // 已用尝试数
		consecutive atomic.Int64 // 连续失败（成功即清零）
		quotaWaits  atomic.Int64 // 因"占用号到上限"而退避的次数（诊断用）
		inflight    atomic.Int64 // 在途尝试数（超发保护，见循环内注释）
		stopMu      sync.Mutex
		stopReason  string
	)
	setStop := func(reason string) {
		stopMu.Lock()
		if stopReason == "" {
			stopReason = reason
		}
		stopMu.Unlock()
	}
	stopped := func() bool {
		stopMu.Lock()
		defer stopMu.Unlock()
		return stopReason != ""
	}

	// ctx 来自 AutoRun（Stop() 通过它的 cancel 中断整个任务）。
	// 这里再派生一层：worker 内部"达到目标就全体停"用局部 cancel，
	// 不会误触发外部的停止语义。
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for {
				if stopped() || got.Load() >= int64(want) {
					return
				}
				// 超发保护：把"在途尝试"也计入目标判定。只看 got 的话，
				// want=6、got=5 时两个 worker 能同时通过检查并发起新尝试，
				// 双双成功就是 7——多消耗一个号（号是要花钱的）。
				// got + inflight >= want 时不再发起新的尝试；已在途的
				// 照常跑完（取消在途的会话会半途丢号，更糟）。
				if got.Load()+inflight.Load() >= int64(want) {
					// 不是停止，是"名额已被在途尝试预留"：等一下再看，
					// 在途的要是失败了名额会重新空出来。
					select {
					case <-ctx.Done():
						return
					case <-time.After(a.retryDelay):
					}
					continue
				}
				if used.Add(1) > int64(limit) {
					return
				}
				inflight.Add(1)
				// 余额检查：所有 worker 都查一遍，代价可忽略（一次 HTTP），
				// 但能保证钱不够时立刻全停。阈值每轮快照读取——SetLimits
				// 运行中热改后，下一个循环立刻按新值判定。
				if mb := a.currentMinBalance(); mb > 0 {
					bal, err := a.hzm.Balance(ctx)
					if err == nil && bal >= 0 && bal < mb {
						setStop(fmt.Sprintf("豪猪余额不足（%.2f 元 < %.2f），任务停止。请充值后重跑。", bal, mb))
						cancel()
						return
					}
				}
				tctx, tcancel := context.WithTimeout(ctx, 6*time.Minute)
				ok, usedPhone, err := a.tryOne(tctx, worker, pollCount)
				tcancel()
				inflight.Add(-1)

				// 只要真的取到了号就计入消耗：中途被取消的尝试也要算，
				// 否则"取了 3 个号后停止"会显示成 0 消耗，看不出号码去向。
				if usedPhone {
					a.mu.Lock()
					a.consumed++
					a.mu.Unlock()
				}

				// 整体取消（另一 worker 已达目标/命中致命错误）导致的失败不算
				// 真实尝试：不计入完成统计，也不计入熔断。
				if !ok && errors.Is(err, context.Canceled) && ctx.Err() != nil {
					return
				}

				a.mu.Lock()
				a.attempts++
				if ok {
					a.ok++
				} else {
					a.fail++
				}
				attempts, oks := a.attempts, a.ok
				a.mu.Unlock()

				if ok {
					consecutive.Store(0)
					if got.Add(1) >= int64(want) {
						cancel()
						return
					}
				} else {
					var ae *haozhuma.APIError
					// 每次失败都记日志：并发时尤其需要看清是"取不到号"
					// 还是"收不到码"，否则熔断原因无从判断。
					if err != nil {
						a.logf("[w%d] 第 %d 次尝试失败: %v", worker, attempts, err)
					}
					// 占用号数到上限：等一会儿再取（别的 worker 释放后就有额度）。
					// 豪猪的措辞是"您的余额不足,请释放拉黑后再取号"，与账户
					// 余额无关，别当成致命错误终止整个任务。
					if errors.As(err, &ae) && ae.QuotaExhausted() {
						quotaWaits.Add(1)
						select {
						case <-ctx.Done():
							return
						case <-time.After(3 * time.Second):
						}
						continue
					}
					// 致命错误：余额/无号/项目禁用等，重试无意义。
					if errors.As(err, &ae) && ae.Fatal() {
						setStop(fmt.Sprintf("豪猪致命错误，任务停止：%v", err))
						cancel()
						return
					}
					// token 失效：重登一次再继续；重登失败则停。
					if errors.As(err, &ae) && ae.TokenInvalid() {
						a.reloginOnce()
						continue
					}
					// 熔断阈值同样每轮快照：运行中调小阈值能立刻触发，调大
					// 立刻放宽（已累计的连续失败数保留）。
					if n := consecutive.Add(1); n >= int64(a.currentConsecutiveFails()) {
						setStop(fmt.Sprintf("连续 %d 个号失败（成功 %d/%d），熔断停止。若都是「取号失败」= 对接商没号；若是「未收到验证码」= 对接商号码收不到腾讯短信。失败号不扣费。",
							n, oks, want))
						cancel()
						return
					}
				}
				// 两次尝试间稍微歇一下，别把代理池/上游打得过密。
				delay := a.retryDelay
				if delay <= 0 {
					delay = 5 * time.Second
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(delay):
				}
			}
		}(w)
	}
	wg.Wait()
	stopMu.Lock()
	defer stopMu.Unlock()
	return stopReason
}

// outcome 合并本次运行的终止原因与外部 Stop() 写入的原因。
//
// Stop() 只写 a.stopReason（run 内部的 stopReason 是另一个变量），
// 所以这里以内部原因为准，内部为空时保留外部写的"用户手动停止"。
func (a *AutoEnroller) outcome(internal string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if internal != "" {
		a.stopReason = internal
	} else if a.stopReason == "" {
		// 既没内部原因也没外部原因 = 正常达标签。
		a.stopReason = ""
	}
	return a.stopReason
}

// trackHeld 记下"这个号目前被我们占着"，并立刻落盘。
//
// 落盘不能攒着批量写：进程随时可能被 SIGKILL（容器重启就是），
// 那一刻内存里的账本会直接消失，而号还占着豪猪的额度。
func (a *AutoEnroller) trackHeld(phone string) {
	a.mu.Lock()
	if a.held == nil {
		a.held = make(map[string]bool)
	}
	a.held[phone] = true
	a.persistLedgerLocked()
	a.mu.Unlock()
}

// markReleased 标记该号已释放（从 held 移出），并同步落盘。
func (a *AutoEnroller) markReleased(phone string) {
	a.mu.Lock()
	if _, ok := a.held[phone]; ok {
		delete(a.held, phone)
		a.released++
		a.persistLedgerLocked()
	}
	a.mu.Unlock()
}

// heldPhones 返回当前仍占着的号码快照。
func (a *AutoEnroller) heldPhones() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.held))
	for p := range a.held {
		out = append(out, p)
	}
	return out
}

// persistLedgerLocked 把账本写盘（调用方必须持锁）。
//
// 写的只有号码本身，不含 token/凭据；号码是已付费资产，所以文件权限收紧到
// 0600。失败只记日志：账本写不下去不该中断加号主流程。
func (a *AutoEnroller) persistLedgerLocked() {
	if a.ledgerPath == "" {
		return
	}
	phones := make([]string, 0, len(a.held))
	for p := range a.held {
		phones = append(phones, p)
	}
	raw, err := json.Marshal(phones)
	if err != nil {
		log.Printf("[auto-enroll] 账本序列化失败: %v", err)
		return
	}
	// 先写临时文件再 rename：SIGKILL 可能发生在写到一半时，留下半个 JSON
	// 会让下次启动读不出来，反而丢掉本该释放的号。
	tmp := a.ledgerPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("[auto-enroll] 账本写入失败: %v", err)
		return
	}
	if err := os.Rename(tmp, a.ledgerPath); err != nil {
		log.Printf("[auto-enroll] 账本替换失败: %v", err)
	}
}

// reclaim 释放给定的号码，把"仍然卡住"的号写回账本。
//
// 不能一律清空账本：清空等于把释放失败的号永久遗忘，而它正占着豪猪的
// 取号额度——那正是我们要修的问题。只有确认"号已不在豪猪手里"才算归还。
//
// 遗留号只释放、不拉黑：账本里可能混有接码已成功的号（成功路径 finish(false)
// 释放失败也会留在账本），拉黑它会让号主以后在本项目收不到码。遗留号里
// 真正收不到码的，下次被取到时走 tryOne 失败路径自然会被拉黑，不会反复。
//
// report 为 true 时用 a.logf（会进控制台日志），否则用标准 log（启动阶段）。
func (a *AutoEnroller) reclaim(phones []string, report bool) int {
	say := func(format string, args ...any) {
		if report {
			a.logf(format, args...)
			return
		}
		log.Printf("[auto-enroll] "+format, args...)
	}
	bg := context.Background()
	// 释放用当前 sid：账本里可能是切换前旧项目取的号，用新 sid 释放会报
	// "手机号不存在"→ goneUpstream 视为归还（豪猪侧到期自动回收，无泄漏）。
	sid := a.currentSid()
	var done int
	var stuck []string
	for _, phone := range phones {
		rerr := a.hzm.Release(bg, sid, phone)
		switch {
		case rerr == nil:
			done++
		case goneUpstream(rerr):
			// 豪猪说这个号根本不在它那儿（"手机号不存在"）——等于已经不占了，
			// 再重试也没有意义，算归还。
			say("遗留号 %s 已不在豪猪（%v），视为已归还", maskPhone(phone), rerr)
			done++
		default:
			say("遗留号释放失败 %s: %v（保留在账本，稍后重试）", maskPhone(phone), rerr)
			stuck = append(stuck, phone)
		}
	}
	next := make(map[string]bool, len(stuck))
	for _, p := range stuck {
		next[p] = true
	}
	a.mu.Lock()
	// 以"仍卡住"的集合为准，而不是简单清空。
	a.held = next
	a.persistLedgerLocked()
	a.mu.Unlock()
	say("遗留号码补释放完成：%d/%d 个已归还", done, len(phones))
	if len(stuck) > 0 && a.ledgerPath != "" {
		say("%d 个号仍占用取号额度（已记入账本，稍后重试；若持续失败可在豪猪后台手动释放）", len(stuck))
	}
	return done
}

// reclaimStuck 在任务开始前重试历史遗留的号码。
//
// 为什么开跑前要清：上一轮释放失败（或进程被杀）的号仍占着豪猪的取号额度，
// 不清掉的话本轮每次取号都会失败并报「余额不足,请释放拉黑后再取号」——
// 看着像账户没钱，实际是旧号没还。
func (a *AutoEnroller) reclaimStuck() int {
	left := a.heldPhones()
	if len(left) == 0 {
		return 0
	}
	return a.reclaim(left, true)
}

// ReclaimOrphans 从账本读回"上次进程没来得及释放"的号码并补释放一次。
//
// 场景：容器重启是 SIGKILL，defer 不执行，内存账本直接消失，号就永远留在
// 豪猪那边占额度——后续每次取号都会返回「您的余额不足,请释放拉黑后再取号」，
// 看起来像账户没钱。启动时补一次就恢复干净。
//
// 必须在服务开始接单前调用（main 里）。返回补释放的号码数。
func (a *AutoEnroller) ReclaimOrphans() int {
	if a.ledgerPath == "" {
		return 0
	}
	raw, err := os.ReadFile(a.ledgerPath)
	if err != nil {
		// 文件不存在是正常情况（从没跑过加号）。
		if !os.IsNotExist(err) {
			log.Printf("[auto-enroll] 读取遗留账本失败: %v", err)
		}
		return 0
	}
	var phones []string
	if err := json.Unmarshal(raw, &phones); err != nil {
		log.Printf("[auto-enroll] 遗留账本损坏（跳过）: %v", err)
		return 0
	}
	if len(phones) == 0 {
		return 0
	}
	log.Printf("[auto-enroll] 发现 %d 个上次未释放的号码（进程曾非正常结束），正在补释放", len(phones))
	return a.reclaim(phones, false)
}

// goneUpstream 判断豪猪是否在说"这个号不在我这儿"（已经释放/过期）。
// 这类错误重试无意义，应当视为已归还，否则账本会永远留着它。
//
// 两种实测文案：
//   - "很抱歉,手机号不存在" —— 号已彻底不在豪猪（2026-09 实测）；
//   - code=-1 msg="释放失败" —— 无此占用记录的通用拒绝。实测场景：
//     一键释放（cancelAllRecv）后账本没同步清空、或占用早已过期，
//     对这些号逐个 cancelRecv 时豪猪秒回"释放失败"（51 个号 1 秒内
//     全部同文案，不可能是网络抖动）。它们同样不在豪猪手里，重试无意义。
func goneUpstream(err error) bool {
	if err == nil {
		return false
	}
	var ae *haozhuma.APIError
	if !errors.As(err, &ae) {
		return false
	}
	if strings.Contains(ae.Msg, "不存在") || strings.Contains(ae.Msg, "没有这个") {
		return true
	}
	// "释放失败"只对 cancelRecv 有意义；黑名单等其他接口同文案时
	// 不能一概当作"不在豪猪"，用 API 名收紧。
	return ae.API == "cancelRecv" && ae.Code == "-1" && strings.Contains(ae.Msg, "释放失败")
}

// SetLedgerPath 设置账本落盘路径（空 = 关闭持久化）。
func (a *AutoEnroller) SetLedgerPath(path string) {
	a.mu.Lock()
	a.ledgerPath = path
	a.mu.Unlock()
}

// smsDebug 写一条 SMS 诊断日志（完整手机号 + 完整短信原文）。
//
// 与 logf 的区别：logf 进控制台回显、强制脱敏；smsDebug 只落服务器本机
// 文件（data/ 卷，权限 0600），不回传任何 HTTP 接口——用于"验证码错误"
// 类问题的离线归因（收到的是通知短信还是真码、旧短信还是新短信）。
//
// 文件上限 maxSMSDebugBytes：超限时丢掉前一半（保留最近记录），从完整
// 行边界开始。写失败静默忽略——诊断日志不能影响加号主流程。
//
// 并发：多 worker 同时收码时会并发调用。轮转用"读-改-写"整个文件，
// 竞争窗口内最多丢一条诊断行或轮转多做一次，无碍；不值得为此加锁。
func (a *AutoEnroller) smsDebug(format string, args ...any) {
	a.mu.Lock()
	path := a.smsDebugPath
	a.mu.Unlock()
	if path == "" {
		return
	}
	line := time.Now().Format("2006-01-02 15:04:05 ") + fmt.Sprintf(format, args...) + "\n"
	_ = smsDebugWrite(path, line)
}

// smsDebugWrite 追加一行，超上限先轮转。
func smsDebugWrite(path, line string) error {
	if fi, err := os.Stat(path); err == nil && fi.Size()+int64(len(line)) > maxSMSDebugBytes {
		if err := rotateSMSDebug(path); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line)
	return err
}

// rotateSMSDebug 丢掉文件前一半内容，保留从完整行开始的后一半。
func rotateSMSDebug(path string) error {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return err
	}
	keep := data[len(data)/2:]
	// 从第一个换行后开始（丢掉被切成两半的首行）。
	if idx := bytes.IndexByte(keep, '\n'); idx >= 0 && idx < len(keep)-1 {
		keep = keep[idx+1:]
	}
	return os.WriteFile(path, keep, 0600)
}

// maxSMSDebugBytes SMS 诊断日志的单文件上限（1 MiB）。手机号+短信原文
// 每条约 200 字节，1 MiB ≈ 5000 条记录，足够覆盖一次大规模加号任务。
const maxSMSDebugBytes = 1 << 20

// SetSMSDebugPath 设置 SMS 诊断日志路径（空 = 关闭）。由 main 注入，
// 放在 data/ 卷（与账本同目录）。
func (a *AutoEnroller) SetSMSDebugPath(path string) {
	a.mu.Lock()
	a.smsDebugPath = path
	a.mu.Unlock()
}

// ReleaseAllHeld 一键释放豪猪名下所有占用号码（cancelAllRecv）。
//
// 与逐号兜底（releaseAllHeld）的区别：cancelAllRecv 是平台侧全量操作，
// 能把**账本之外**的号也放掉（手动测试留下的、别的进程取的、账本文件
// 丢失前的）。代价是没有逐号确认，所以调用后把整个账本清空——平台已经
// 没有任何占用了，账本再留着只会让下次启动对已释放的号空转。
//
// 任务运行中拒绝执行（409）：cancelAllRecv 会把正在收码的在途号码一起
// 放掉，等码等一半号没了，所有 worker 白等。
//
// 返回 (执行了释放, 前账本遗留数)。
func (a *AutoEnroller) ReleaseAllHeld(ctx context.Context) (bool, int, error) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return false, 0, ErrBusy
	}
	heldBefore := len(a.held)
	a.mu.Unlock()

	if err := a.hzm.ReleaseAll(ctx); err != nil {
		return false, heldBefore, err
	}
	// 平台侧已全量释放：账本整体清空，released 补记本批。
	a.mu.Lock()
	a.held = map[string]bool{}
	a.released += heldBefore
	a.persistLedgerLocked()
	a.mu.Unlock()
	return true, heldBefore, nil
}

// ErrBusy 一键释放与运行中的任务冲突。
var ErrBusy = errors.New("自动加号任务正在运行，等它结束后再一键释放")

// releaseAllHeld 任务收尾兜底：把账本里仍未确认释放的号全部释放。
//
// 必须兜底的原因：取号后就占用了豪猪的并发额度，额度满了会返回
// 「您的余额不足,请释放拉黑后再取号」，导致**后续所有取号都失败**——
// 看起来像账户没钱，实际是号没还。正常路径都会释放，但进程被 kill、
// 或单次释放请求失败时就会漏（实测：一轮任务结束后额度仍被占着，
// 下一轮 14 次尝试全部失败）。
//
// 不拉黑：账本里可能混有接码已成功的号（成功路径 finish(false) 释放
// 失败也会留下），拉黑它会让号主以后在本项目收不到码。失败路径的号
// 在 tryOne 里已经被拉黑过了，这里的号要么是"释放请求失败"要么是
// "接码成功但没还回去"，两种都不该拉黑。
//
// 释放失败只记日志、不报错——收尾阶段没有更合适的处理方式，而且这条
// 路径本就是"正常释放没成功"时的补救。
//
// 释放失败的号会**留在账本里**（markReleased 只在成功时调用）：账本不随
// 任务结束清空，下次任务开跑前（reclaimStuck）或进程下次启动时
// （ReclaimOrphans）会再试。删掉它们等于把"这个号还占着额度"忘了，
// 而那正是本函数要解决的问题。
func (a *AutoEnroller) releaseAllHeld() {
	left := a.heldPhones()
	if len(left) == 0 {
		return
	}
	a.logf("任务结束，兜底释放 %d 个仍占用的号码", len(left))
	bg := context.Background()
	// 任务中途切过 sid 时，在途号属于旧项目：当前 sid 释放报"手机号不
	// 存在"→ 失败留账本，下次启动 reclaim 走 goneUpstream 判定收尾。
	sid := a.currentSid()
	var failed int
	for _, phone := range left {
		if err := a.hzm.Release(bg, sid, phone); err != nil {
			a.logf("兜底释放 %s 失败（号仍占着豪猪额度）: %v", phone, err)
			failed++
			continue
		}
		a.markReleased(phone)
	}
	if failed == 0 {
		a.logf("兜底释放完成：%d 个号码已全部归还", len(left))
	} else {
		a.logf("兜底释放结束：%d 个成功，%d 个失败（失败的号已留在账本，下次启动会重试）",
			len(left)-failed, failed)
	}
}

// reloginOnce 保证并发的 token 失效只触发一次重登。
func (a *AutoEnroller) reloginOnce() {
	a.reloginMu.Lock()
	defer a.reloginMu.Unlock()
	// 别的 worker 刚登过就不用再登（token 已经在几秒前刷新）。
	if time.Since(a.lastRelogin) < 30*time.Second {
		return
	}
	if err := a.hzm.Relogin(); err != nil {
		a.logf("豪猪 token 重登失败: %v", err)
		return
	}
	a.lastRelogin = time.Now()
	a.logf("豪猪 token 已重登，继续")
}

// tryOne 走一个号的完整流程。返回是否成功加了号、是否真的取到了号。
// worker 只用于日志标记，方便并发时区分是哪个 worker 的动作。
//
// usedPhone 与 ok 分开的原因：即使尝试被中途取消，号也已经从豪猪取走了
// （并被拉黑），调用方需要把它算进"号码消耗"，否则统计会漏报。
//
// 号码处置（2026-09 与豪猪官方 SDK next_code() 语义核对后调整）：
//   - 接码成功 → 只释放，不拉黑（官方 SDK 同款行为；拉黑已成功的号会让
//     号主以后在本项目收不到码，过于激进）；
//   - 收不到码/发码失败/验码失败/已在号池 → 拉黑 + 释放（黑名单语义就是
//     "这个号在本项目收不到码，别再发给我"）。
//
// 黑名单在 release 之前调用（豪猪要求的顺序）。
func (a *AutoEnroller) tryOne(ctx context.Context, worker, pollCount int) (ok bool, usedPhone bool, err error) {
	// sid 在函数开头快照一次：取号、收码、释放/拉黑全流程必须用同一个
	// 项目 ID（号码归属项目；中途切 sid 会让释放报"手机号不存在"）。
	sid := a.currentSid()
	// 1) 豪猪取号。
	phone, err := a.hzm.GetPhone(ctx, sid)
	if err != nil {
		// 没取到号 = 没有消耗。
		return false, false, fmt.Errorf("取号失败: %w", err)
	}
	// 从这里开始，号码已经被占用，任何退出路径都要计入消耗。
	usedPhone = true
	// 记进账本：任务收尾时会按它兜底释放，保证不会把号留在豪猪那边占额度。
	a.trackHeld(phone)
	// 对接码池自动生命周期：GetPhone 已把失效的码移出轮换池（drained），
	// 这里消费走——更新 config + 从豪猪账户移出（handler 注入的回调）。
	if drained := a.hzm.TakeDrainedUIDs(); len(drained) > 0 {
		a.logf("对接码 %v 已用完/失效，自动移出轮换池（池剩 %d 个）", drained, len(a.hzm.UIDs()))
		if cb := a.uidsDrainedCallback(); cb != nil {
			go cb(drained) // 异步：H5 慢，不挡取号流水线
		}
	}
	// 配置里钉死的对接码被上游删掉了：已经自动退回平台分配，但要让你知道
	// 配置里的值已经没用了（否则会以为号段变化是别的原因）。
	if bad := a.hzm.TakeUnknownUID(); bad != "" {
		a.logf("配置的对接码 %s 已失效（上游不存在），已自动改用平台分配；建议更新配置", bad)
	}
	a.logf("[w%d] 取号 %s", worker, phone)

	// finish 统一收尾。任何退出路径都要走它；释放成功才把号从账本里销掉，
	// 失败则留在账本上，由任务收尾的 releaseAllHeld 再兜一次（额度被占着
	// 会让后续取号全部失败）。
	//
	// 拉黑策略（与豪猪官方 SDK next_code() 的语义对齐，2026-09 核对）：
	//   - blacklist=true 的路径：收不到码/发码失败/验码失败——这个号在
	//     本项目上"废了"，拉黑避免下次又取到同一个（黑名单语义就是
	//     "别再发给我"）；
	//   - blacklist=false（接码成功）：只释放不拉黑。豪猪官方 SDK 成功
	//     后仅 release；拉黑已成功的号会让号主（真实用户）以后在这个
	//     项目上收不到码，过于激进。
	//
	// 关于"拉黑后是否还要 release"：豪猪官方 SDK 超时分支只调
	// addBlacklist、不补 cancelRecv——说明 addBlacklist 自带释放语义
	// （拉黑即收回号码、归还占用额度；平台文案「请释放拉黑后再取号」
	// 把两者并列也印证这一点）。这里拉黑后仍然补一次 release：对已
	// 归还的号豪猪返回"手机号不存在"，reclaim 的 goneUpstream 判定
	// 会把它当成功处理，双保险没有副作用。
	finish := func(blacklist bool) {
		bg := context.Background()
		if blacklist {
			if err := a.hzm.Blacklist(bg, sid, phone); err != nil {
				a.logf("拉黑 %s 失败: %v", phone, err)
			}
		}
		if err := a.hzm.Release(bg, sid, phone); err != nil {
			a.logf("释放 %s 失败: %v（留待任务收尾重试）", phone, err)
			return
		}
		a.markReleased(phone)
	}

	// 2) 已在号池里的号：拉黑（避免反复取到同一个）+ 换下一个。
	if _, exists := a.find(phone); exists {
		a.logf("号 %s 已在号池，拉黑换下一个", phone)
		finish(true)
		return false, true, nil
	}

	// 3) 发码（走本服务 SMSLogin：代理池 + 风控处理都在里面）。
	send, err := a.sms.Send(ctx, phone, "cn")
	if err != nil {
		var ue *smslogin.Error
		if errors.As(err, &ue) && ue.Retryable {
			a.logf("号 %s 发码被拒（可重试）: %s", phone, ue.Msg)
		} else {
			a.logf("号 %s 发码失败: %v", phone, err)
		}
		finish(true)
		return false, true, err
	}
	sess := send.SessionID
	if len(sess) > 8 {
		sess = sess[:8]
	}
	a.logf("已发码 session=%s… 等短信", sess)

	// 4) 轮询豪猪收码。轮询次数由调用方传入（用户可调），默认 18 次 × 5s。
	code, waitErr := a.pollCode(ctx, sid, phone, pollCount)

	// 5) 验码（Verify 内部走完整 12 步并落凭据）。
	if waitErr == nil && code != "" {
		creds, verr := a.sms.Verify(ctx, send.SessionID, code)
		if verr == nil {
			// 6) 落盘。
			acc := accountCredential{
				UID:          creds.UID,
				Nickname:     creds.Nickname,
				EnterpriseID: creds.EnterpriseID,
				Domain:       creds.Domain,
				AccessToken:  creds.AccessToken,
				RefreshToken: creds.RefreshToken,
				ExpiresIn:    creds.ExpiresIn,
			}
			_, _, perr := a.persist(acc, creds.Region)
			if perr != nil {
				a.logf("号 %s 登录成功但落盘失败: %v", phone, perr)
				// 接码已成功（豪猪已扣费），落盘失败不拉黑——只释放。
				finish(false)
				return false, true, perr
			}
			// 接码成功：只释放不拉黑（对齐豪猪官方 SDK next_code 语义）。
			a.smsDebug("[%s] 验码成功 phone=%s code=%s uid=%s", sid, phone, code, creds.UID)
			// 分组登记失败只告警不影响加号结果：账号已落盘可用，分组是
			// 归属元数据，用户可以在账号列表里事后补设。
			a.mu.Lock()
			runGroups := a.runGroups
			a.mu.Unlock()
			if err := a.setAccountGroups(creds.UID, runGroups); err != nil {
				a.logf("号 %s 加号成功但分组登记失败: %v（可在账号列表手动补设）", phone, err)
			}
			// 成长任务联动（可选，异步）：注册成功 → 自动跑一遍 17 项任务
			// 自动化（约 +1950 积分）。回调内自带 per-account 锁与节流，这里
			// 只负责触发。开关在 handler 侧（schedule.autoenroll_growth_tasks）。
			if cb := a.enrolledCallback(); cb != nil {
				go cb(creds.UID)
			}
			a.logf("号 %s 加号成功 uid=%s…（已释放）", phone, shortUID(creds.UID))
			finish(false)
			return true, true, nil
		}
		a.logf("号 %s 验码失败: %v", phone, verr)
		// 诊断日志：验码失败的号 + 提交的码 + 上游错误全文——结合
		// pollCode 记的短信原文即可归因（码从哪条短信来、上游为何拒绝）。
		a.smsDebug("[%s] 验码失败 phone=%s code=%s err=%v", sid, phone, code, verr)
		finish(true)
		return false, true, verr
	}

	// 等不到码：拉黑 + 释放，换下一个号。
	a.logf("号 %s 未收到验证码: %v（已拉黑）", phone, waitErr)
	a.smsDebug("[%s] 收码超时 phone=%s err=%v", sid, phone, waitErr)
	finish(true)
	return false, true, waitErr
}

// pollCode 轮询豪猪 getMessage，直到拿到验证码或轮询次数用尽。
//
// 次数优先：count > 0 时精确轮 count 次；count <= 0 时退回按 pollTimeout 推导
// （默认 90s / 5s = 18 次）。
//
// 默认超时用 90s 而不是上游的 60s 有效期：豪猪的短信入库本身有延迟，
// 偶尔会比 60s 晚一点到，多留 30s 能捞回这部分。代价只是失败时多等 30s，
// 而失败不扣费。
//
// "等待"是正常轮询状态（豪猪返回 code=-1 msg=等待短信）——GetMessage 把它
// 当成功返回空 sms，这里只记轮次。
func (a *AutoEnroller) pollCode(ctx context.Context, sid, phone string, count int) (string, error) {
	pollInterval := a.pollInterval
	if pollInterval <= 0 {
		pollInterval = 5 * time.Second
	}
	// 次数优先；没给次数才回到"时长 ÷ 间隔"的老算法。
	if count <= 0 {
		wait := a.pollTimeout
		if wait <= 0 {
			wait = 90 * time.Second
		}
		count = int(wait / pollInterval)
		if count < 1 {
			count = 1
		}
	}
	// 兜底上限：次数由外部传入，给个荒唐的值（比如 100000）会把任务挂死，
	// 而每个号都被豪猪占着不放。这里夹住，超出的部分忽略。
	if count > maxPollCount {
		count = maxPollCount
	}
	var polls int
	for polls < count {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		sms, err := a.hzm.GetMessage(ctx, sid, phone)
		if err != nil {
			// "等待短信" 是正常轮询状态，不是错误：GetMessage 把豪猪的
			// code=-1/msg=等待短信 包成 APIError 返回，必须用 errors.As 取出来判断。
			// 之前这里漏了 errors.As，ae 恒为 nil，于是每次"还在等待"都会直接
			// 当成失败返回，白白作废一个已经付费的号码并提前结束轮询。
			var ae *haozhuma.APIError
			if errors.As(err, &ae) && ae.Waiting() {
				// 正常等待，继续轮。
			} else {
				return "", err
			}
		}
		polls++
		if code := haozhuma.ExtractCode(sms); code != "" {
			// 诊断日志（本机文件，不脱敏）：完整短信原文 + 提取到的码，
			// 用于"验证码错误"归因（通知短信混入 / 旧短信 / 真码过期）。
			a.smsDebug("[%s] 轮 %d 收到短信 phone=%s code=%s sms=%q", a.currentSid(), polls, phone, code, sms)
			return code, nil
		}
		// 等待期间收到过非验证码短信也记一条（ExtractCode 没抠出码的原文），
		// 排查"通知短信抢先到达"时能看到它长什么样。
		if strings.TrimSpace(sms) != "" {
			a.smsDebug("[%s] 轮 %d 无码短信 phone=%s sms=%q", a.currentSid(), polls, phone, sms)
		}
		// 最后一轮之后不用再等：没有下一次查询了。
		if polls >= count {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	return "", fmt.Errorf("验证码等待超时（轮询 %d 次未收到短信）", polls)
}

func shortUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// maskPhone 保留前 3 后 4 位：运营商和尾号足够让操作者认出是哪一个号，
// 但不构成一个可直接拨打的号码。
func maskPhone(phone string) string {
	// 去掉 +86 / 86 等前缀后按数字串处理。
	digits := strings.TrimLeft(phone, "+")
	if strings.HasPrefix(digits, "86") && len(digits) > 11 {
		digits = digits[2:]
	}
	if len(digits) < 7 {
		return strings.Repeat("*", len(digits))
	}
	return digits[:3] + strings.Repeat("*", len(digits)-7) + digits[len(digits)-4:]
}

// phoneRe 匹配日志里的手机号。豪猪取号返回的是 11 位中国手机号（1 开头），
// 这里同时容忍带 +86 的写法。
var phoneRe = regexp.MustCompile(`(?:\+?86)?1[3-9]\d{9}`)

// maskPhonesIn 把字符串里出现的手机号统一替换成脱敏形式。
func maskPhonesIn(s string) string {
	return phoneRe.ReplaceAllStringFunc(s, maskPhone)
}
