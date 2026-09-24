// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 顶层配置。
type Config struct {
	Listen           string `json:"listen"`            // ":7863"
	APIKey           string `json:"api_key"`           // 空 = 不鉴权
	FrontendPassword string `json:"frontend_password"` // 前端控制台密码；空 = 不启用
	AuthDir          string `json:"auth_dir"`          // ./auths
	StateFile        string `json:"state_file"`        // ./data/state.json
	Region           string `json:"region"`            // "cn" / "global" / "all"
	// FrontendDir 静态控制台资源目录，默认 "frontend"（相对进程工作目录）。
	// 只有 index.html 与 assets/index-<hash>.{js,css} 会被放行。
	FrontendDir string `json:"frontend_dir"`
	// TrustedProxies 允许其 X-Forwarded-For / X-Real-IP 生效的反向代理地址
	// （单地址或 CIDR）。默认空 = 谁都不信，直接用 RemoteAddr。
	// 只有服务确实只在代理后面可达时才配置：否则攻击者伪造这两个头就能
	// 绕开基于 IP 的解锁锁定。
	TrustedProxies []string `json:"trusted_proxies"`

	// RequestLogRetentionDays 请求日志保留天数（时间条件）；0 = 不限时间。
	// RequestLogRetentionRows 请求日志最大条数；0 = 不限条数。
	// 两者同时为 0 时退化为旧默认（1 万条）。WebUI 可改（即时生效）。
	RequestLogRetentionDays int `json:"request_log_retention_days"`
	RequestLogRetentionRows int `json:"request_log_retention_rows"`

	// MaxRequestBodyMiB 请求体大小上限（MiB），默认 8。只作用于
	// /v1/chat/completions 与 /v1/responses 的请求体（会全量读进内存）。
	// WebUI 可改（即时生效）；范围 1-64。
	MaxRequestBodyMiB int `json:"max_request_body_mib"`

	// Pprof pprof 性能分析端点（net/http/pprof）。
	Pprof struct {
		// Enabled 是否启用；默认 false（生产默认不开）。
		Enabled bool `json:"enabled"`
		// Addr 监听地址，默认 "127.0.0.1:6060"。必须是显式地址（host:port），
		// pprof 无鉴权，绝不能绑定 0.0.0.0（除非前面还有一层带鉴权的反代）。
		Addr string `json:"addr"`
	} `json:"pprof"`

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown 三个历史键已退役：
		// 硬冷却固定为次日 04:00（CooldownUntilTomorrow4AM），连续错误语义并入熔断器。
		// 旧 config 中的这些键因 JSON 未知字段而自然忽略，不报错。
		SoftRate string `json:"soft_rate"` // "60s"
	} `json:"cooldown"`

	Schedule struct {
		CheckinHours   []int `json:"checkin_hours"`   // [9,21]
		TravelHours    []int `json:"travel_hours"`    // [9,21]：猫猫旅行（领养/派出/领奖）
		ActivityHours  []int `json:"activity_hours"`  // [10]：对话活跃上报（点亮连登+解锁领养）
		KeepaliveHours []int `json:"keepalive_hours"` // [22]
		BlackcatHours  []int `json:"blackcat_hours"`  // [23]：夜猫子（23:00–08:00 计数窗口）
		// *Enabled 各排程总开关；false = 真正关闭（hours 原样保留，改回 true 即恢复）。
		// 注意：空数组/null 的 hours 语义是「未配置 → 回落默认」，不是禁用。
		CheckinEnabled   bool `json:"checkin_enabled"`   // 缺省 true
		TravelEnabled    bool `json:"travel_enabled"`    // 缺省 true
		ActivityEnabled  bool `json:"activity_enabled"`  // 缺省 true
		KeepaliveEnabled bool `json:"keepalive_enabled"` // 缺省 true
		BlackcatEnabled  bool `json:"blackcat_enabled"`  // 缺省 true
		// AutoenrollGrowthTasks 自动加号注册成功后自动跑一遍成长任务（17 项，约
		// +1950 积分）。默认 false：任务链含数条真实短对话，用户显式开启。
		AutoenrollGrowthTasks bool `json:"autoenroll_growth_tasks"`
	} `json:"schedule"`

	Upstream struct {
		// TimeoutSeconds 非流式请求的**整请求**上限（含读完响应体），默认 120。
		// 它同时约束控制面 JSON 调用（刷新 token、对账、签到等）。
		TimeoutSeconds int `json:"timeout_seconds"`
		// StreamTimeoutSeconds 流式请求的总时长上限，0 = 不限（默认）。
		// 流式默认不设总上限：Client.Timeout 语义是"整请求含响应体"，用在流式上
		// 等于给回答长度设死限，会把本来能继续输出的长回答掐断。
		// 需要兜底（防跑飞的流长期占住账号租约）时再设一个较大的值。
		StreamTimeoutSeconds int `json:"stream_timeout_seconds"`
		// StreamIdleSeconds 流式请求两次成功读取之间的最大间隔，默认 120。
		// 上游持续产出时永不触发；上游卡住不发数据时据此尽快失败。
		// 未设置或 0 = 用默认 120；显式设 -1 = 关闭该项检查。
		StreamIdleSeconds int `json:"stream_idle_seconds"`
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints 出站请求体黑名单指纹脱敏（默认 true；false 完全还原）。
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
		// Passthrough enables raw upstream SSE forwarding for streaming chat completions.
		Passthrough bool `json:"passthrough"`
		// CodexCompat 改写 Codex CLI 系统提示词中的身份句（"open source"
		// → "open-source"），绕开上游内容审核的逐字指纹匹配。句子不存在
		// 时原样透传。默认 false。
		CodexCompat bool `json:"codex_compat"`
		// ResponsesAPI 是否暴露 Responses 端点（/v1/responses、/responses）。
		// 关闭时返回 404（表现为端点不存在）。默认 true。
		// 用途：只用 Chat Completions 的部署关掉它，缩小攻击面与日志噪音
		// （Responses 带会话历史存储，比 chat 多一块状态）。
		ResponsesAPI bool `json:"responses_api"`
	} `json:"features"`

	Billing struct {
		// Values are credits per 1,000 tokens. Zero leaves unknown usage unestimated.
		InputCreditsPer1KTokens       float64 `json:"input_credits_per_1k_tokens"`
		OutputCreditsPer1KTokens      float64 `json:"output_credits_per_1k_tokens"`
		CachedInputCreditsPer1KTokens float64 `json:"cached_input_credits_per_1k_tokens"`
	} `json:"billing"`

	Upstash struct {
		URL   string `json:"url"`   // 空 = 纯内存模式；支持完整 rediss:// URL 或 https://xxx.upstash.io host
		Token string `json:"token"` // url 非完整连接串时用于组装 rediss://default:<token>@<host>:6379
	} `json:"upstash"`

	SMS struct {
		// TwoCaptchaKey 是 2captcha 的 clientKey。仅在中国区短信直登遇到
		// need_captcha 时使用；留空则该场景提示改用浏览器授权，不影响其他流程。
		TwoCaptchaKey string `json:"two_captcha_key"`
		// Proxy 把整条短信登录链路放到住宅代理后面：同一 IP 半小时内只能注册
		// 一个号，多次注册会被上游风控。每次登录换一个随机粘性会话（sid），
		// 天然拿到不同出口 IP；号池的日常 API 调用不走这个代理。
		Proxy struct {
			// URL 形如 http://用户名:密码@网关:端口。与 file 二选一；file 优先。
			URL string `json:"url"`
			// File 代理名单路径（每行 host:port:user:pass 或完整 URL）。
			// 每次登录换一条，用过的条目进入 cooldown。
			File string `json:"file"`
			// Cooldown 同一条代理再次用于注册的最短间隔，默认 "30m"。
			Cooldown string `json:"cooldown"`
			// Region 可选 ISO 3166-1 两位码（如 HK）。仅在 inject_sid 时拼进用户名。
			Region string `json:"region"`
			// InjectSID 为 true 时按 1024proxy 约定改写用户名（每次登录换 sticky IP）。
			// 普通静态代理必须为 false，否则账密会被改坏。
			InjectSID bool `json:"inject_sid"`
			// StickyMinutes 粘性时长 1-120，默认 30。一次登录十几个请求必须
			// 落在同一个 IP 上，时长要盖过登录全程。
			StickyMinutes int `json:"sticky_minutes"`
		} `json:"proxy"`
		// Haozhuma 豪猪接码平台（haozhuma.com），用于自动加号：取号 →
		// 走本服务的短信直登链 → 收码 → 落盘账号。
		Haozhuma struct {
			// User / Pass 豪猪 API 账号密码（login 换 token 用）。
			User string `json:"user"`
			Pass string `json:"pass"`
			// Token 已有 token 时直接用，跳过 login。
			Token string `json:"token"`
			// Sid 项目 ID。52283 = 腾讯科技[限对接]。
			Sid string `json:"sid"`
			// Author 「[限对接]」项目的对接方标识。留空则不下发。
			// 注意：实测 52283 项目带 author=adminzfz 反而取不到号，默认留空。
			Author string `json:"author"`
			// UID 指定对接码（豪猪后台的"对接码 UID"）。一个 sid 下可能挂了
			// 多个对接商，质量不一：不指定时平台随机分配，可能撞上没号的那个。
			// 实测指定有号的对接码成功率 6/6，随机只有 3/6。
			UID string `json:"uid"`
			// UIDs 多对接码轮换池（2026-09）。配置后 GetPhone 在这些码间
			// round-robin；某码失效自动出池（并经 H5 从账户移除），池空退
			// 回单 uid / 平台自动分配。WebUI 对接码浮窗多选写这里，uid 字段
			// 保留兼容旧配置（池非空时忽略单值）。
			UIDs []string `json:"uids"`
			// ISP 取号运营商优先级：1=移动 2=联通 3=电信，逗号分隔依次降级，
			// 最后自动退回"不限"。留空表示直接不限。
			ISP string `json:"isp"`
			// H5Session 豪猪网页版（h5.haozhuma.com）的 PHPSESSID Cookie。
			// 只服务 WebUI 的项目搜索/对接码选择增强（P1）；未配置时选择器
			// 退化为手填。与接码 API 的 token 无关，失效不影响任务链路。
			H5Session string `json:"h5_session"`
		} `json:"haozhuma"`
	} `json:"sms"`

	// AutoEnroll 自动加号的任务控制参数（WebUI「高级设置」）。指针类型区分
	// "未配置"（走环境变量/默认值兜底）与"显式配置"；运行期可经 WebUI 热改。
	AutoEnroll struct {
		// MinBalance 余额保护阈值（元）。低于此值不再开始新的取号；
		// 0 = 关闭保护。
		MinBalance *float64 `json:"min_balance"`
		// ConsecutiveFails 连续失败熔断阈值：连续这么多个号都失败时停止
		// 任务（通道坏了/项目被限，继续跑也不会有结果）。
		ConsecutiveFails *int `json:"consecutive_fails"`
		// RetryDelaySeconds 两次取号尝试之间的间隔（秒）。
		RetryDelaySeconds *int `json:"retry_delay_seconds"`
		// Watch 对接码监控 + 额度化自动加号（UID Watcher，2026-09）。
		// 字段校验/默认值见 internal/server.WatchConfig；这里只承载
		// JSON 结构（Enable 与项目列表校验在 handler PUT 入口做，
		// 启动时宽松加载——坏配置只影响 watcher 不该挡服务启动）。
		Watch WatchJSON `json:"watch"`
	} `json:"autoenroll"`

	// ReqProxy 实际请求账号的代理池（新模块，与 sms.proxy 的注册代理完全独立）。
	// 订阅/节点/规则/绑定全部在 WebUI 管理（/admin/reqproxy/*），这里只有
	// 基础设施参数；不配即用默认值。enabled 开关在模块自己的 state.json 里。
	ReqProxy struct {
		// StateFile 模块状态文件。空 = state.json 同目录下 reqproxy/state.json。
		StateFile string `json:"state_file"`
		// HealthInterval 周期测速间隔，默认 "15m"。
		HealthInterval string `json:"health_interval"`
		// LatencyTimeout 单次测速超时，默认 "5s"。
		LatencyTimeout string `json:"latency_timeout"`
		// UnhealthyCooldown 节点摘除后的复测冷却，默认 "10m"。
		UnhealthyCooldown string `json:"unhealthy_cooldown"`
		// PortMin / PortMax 槽位本地端口段，默认 31080-31999。
		PortMin int `json:"port_min"`
		PortMax int `json:"port_max"`
	} `json:"reqproxy"`

	Postgres struct {
		DSN              string `json:"dsn"`
		MaxOpenConns     int    `json:"max_open_conns"`
		MaxIdleConns     int    `json:"max_idle_conns"`
		ConnMaxLifetime  string `json:"conn_max_lifetime"`
		ConnMaxIdleTime  string `json:"conn_max_idle_time"`
		FallbackToSQLite bool   `json:"fallback_to_sqlite"`
	} `json:"postgres"`

	Pool struct {
		MaxInFlight        int     `json:"max_in_flight"`        // 单账号最大在途请求数，0 = 不限
		BreakerThreshold   int     `json:"breaker_threshold"`    // 连续失败次数触发熔断，默认 3
		BreakerCooldown    string  `json:"breaker_cooldown"`     // 基础熔断时长，默认 "30m"
		BreakerCooldownMax string  `json:"breaker_cooldown_max"` // 指数退避封顶，默认 "6h"
		IdleWeightPerHour  float64 `json:"idle_weight_per_hour"` // 闲置补偿：每小时未用 +0.5 权重
		IdleWeightMax      float64 `json:"idle_weight_max"`      // 闲置补偿封顶，默认 5.0
	} `json:"pool"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`     // 默认 true
		TTL        string `json:"ttl"`         // 会话绑定 TTL，默认 "30m"
		GCInterval string `json:"gc_interval"` // 会话 GC 周期，默认 "5m"
		// Salt 给会话键 HMAC 加盐（hex）。为空则启动时生成随机盐并在日志告警：
		// 会话粘性可用，但重启后全部重新分配，且键可被预计算。
		Salt string `json:"salt"`
		// MaxEntries 粘性绑定表上限，默认 10000。TTL 无法在单个窗口内封顶。
		MaxEntries int `json:"max_entries"`
	} `json:"session_sticky"`

	// 解析后
	SoftRateDur          time.Duration `json:"-"`
	BreakerCooldownDur   time.Duration `json:"-"`
	BreakerCooldownMaxD  time.Duration `json:"-"`
	SessionTTL           time.Duration `json:"-"`
	SessionGCInterval    time.Duration `json:"-"`
	SMSProxyCooldownDur  time.Duration `json:"-"`
	ReqProxyHealthDur    time.Duration `json:"-"`
	ReqProxyLatencyDur   time.Duration `json:"-"`
	ReqProxyUnhealthyDur time.Duration `json:"-"`
	PostgresMaxLifetime  time.Duration `json:"-"`
	PostgresMaxIdleTime  time.Duration `json:"-"`
	// ScheduleEnabled 解析后的排程开关（JSON 里缺省 false，这里归一为「缺省=开」）。
	ScheduleEnabled struct {
		Checkin, Travel, Activity, Keepalive, Blackcat bool
		AutoenrollGrowthTasks                          bool
	} `json:"-"`
	// HasScheduleEnabled 区分「配置文件显式写了 false」与「没写」（JSON bool 零值歧义）。
	HasScheduleEnabled map[string]bool `json:"-"`
	// StreamTimeoutDur 流式总时长上限；0 = 不限。
	StreamTimeoutDur time.Duration `json:"-"`
	// StreamIdleDur 流式空闲上限；0 = 不检查。
	StreamIdleDur time.Duration `json:"-"`
	// PprofAddr 解析后的 pprof 监听地址；Enabled=false 时为空。
	PprofAddr string `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		// 只监听回环：默认部署是本机给编辑器/脚本用的，",7863" 会 bind 所有网卡。
		// 需要对外暴露（含容器）时显式设置 listen 或 WB2A_LISTEN=:7863。
		Listen:    "127.0.0.1:7863",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
		Region:    "cn",
		// 静态控制台资源目录。空表示用 "frontend"（NewHandler 内部兜底）。
		FrontendDir: "frontend",
	}
	c.Cooldown.SoftRate = "60s"
	c.Schedule.CheckinHours = []int{9, 21}
	c.Schedule.TravelHours = []int{9, 21}
	c.Schedule.ActivityHours = []int{10}
	c.Schedule.KeepaliveHours = []int{22}
	c.Schedule.BlackcatHours = []int{23}
	c.Schedule.CheckinEnabled = true
	c.Schedule.TravelEnabled = true
	c.Schedule.ActivityEnabled = true
	c.Schedule.KeepaliveEnabled = true
	c.Schedule.BlackcatEnabled = true
	c.Upstream.TimeoutSeconds = 120
	// 流式默认不限总时长，只守空闲窗口。这正是本次改造的目的：长回答不再被一个
	// 整请求超时掐断，而上游卡住时仍会在 StreamIdleSeconds 内失败。
	c.Upstream.StreamTimeoutSeconds = 0
	c.Upstream.StreamIdleSeconds = 120
	c.Features.SanitizeBlacklistFingerprints = true
	c.Features.Passthrough = false
	c.Features.CodexCompat = false
	c.Features.ResponsesAPI = true
	// 请求体上限默认 8 MiB（与历史硬编码一致）。0 = 默认，见 normalize。
	c.MaxRequestBodyMiB = 8
	c.Pool.MaxInFlight = 3
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	c.Postgres.MaxOpenConns = 16
	c.Postgres.MaxIdleConns = 8
	c.Postgres.ConnMaxLifetime = "30m"
	c.Postgres.ConnMaxIdleTime = "5m"
	c.Pprof.Addr = "127.0.0.1:6060"
	return c
}

// Load 从文件读，再用 WB2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
		// 二次探测 schedule.*_enabled 是否显式出现：JSON bool 的零值歧义
		// （false 既可能是"显式关闭"也可能是"没写"），Default 已把没写的
		// 补成 true，Unmarshal 后无法区分——必须在原始 JSON 里查 key。
		var probe struct {
			Schedule map[string]json.RawMessage `json:"schedule"`
		}
		if json.Unmarshal(raw, &probe) == nil && probe.Schedule != nil {
			c.HasScheduleEnabled = map[string]bool{}
			for _, key := range []string{"checkin_enabled", "travel_enabled", "activity_enabled", "keepalive_enabled", "blackcat_enabled", "autoenroll_growth_tasks"} {
				if v, ok := probe.Schedule[key]; ok {
					var b bool
					_ = json.Unmarshal(v, &b)
					c.HasScheduleEnabled[key] = b
				}
			}
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WB2A_FRONTEND_PASSWORD"); v != "" {
		c.FrontendPassword = v
	}
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_REGION"); v != "" {
		c.Region = v
	}
	if v := os.Getenv("WB2A_FRONTEND_DIR"); v != "" {
		c.FrontendDir = v
	}
	if v := os.Getenv("WB2A_SESSION_SALT"); v != "" {
		c.SessionSticky.Salt = v
	}
	// 逗号分隔：WB2A_TRUSTED_PROXIES=10.0.0.1,10.0.0.0/8
	if v := os.Getenv("WB2A_TRUSTED_PROXIES"); v != "" {
		var proxies []string
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				proxies = append(proxies, part)
			}
		}
		c.TrustedProxies = proxies
	}
	if v := os.Getenv("WB2A_POSTGRES_DSN"); v != "" {
		c.Postgres.DSN = v
	}
	if v := os.Getenv("WB2A_POSTGRES_MAX_OPEN_CONNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Postgres.MaxOpenConns = n
		}
	}
	if v := os.Getenv("WB2A_POSTGRES_MAX_IDLE_CONNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Postgres.MaxIdleConns = n
		}
	}
	if v := os.Getenv("WB2A_POSTGRES_CONN_MAX_LIFETIME"); v != "" {
		c.Postgres.ConnMaxLifetime = v
	}
	if v := os.Getenv("WB2A_POSTGRES_CONN_MAX_IDLE_TIME"); v != "" {
		c.Postgres.ConnMaxIdleTime = v
	}
	if v := os.Getenv("WB2A_POSTGRES_FALLBACK_SQLITE"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Postgres.FallbackToSQLite = b
		}
	}
	if v := os.Getenv("WB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_STREAM_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.StreamTimeoutSeconds = n
		}
	}
	// 允许 -1（关闭空闲检查），所以不能只接受正数。
	if v := os.Getenv("WB2A_STREAM_IDLE_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.StreamIdleSeconds = n
		}
	}
	if v := os.Getenv("WB2A_SANITIZE_FINGERPRINTS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.SanitizeBlacklistFingerprints = b
		}
	}
	if v := os.Getenv("WB2A_PASSTHROUGH"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.Passthrough = b
		}
	}
	if v := os.Getenv("WB2A_PPROF_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Pprof.Enabled = b
		}
	}
	if v := os.Getenv("WB2A_PPROF_ADDR"); v != "" {
		c.Pprof.Addr = v
	}
	if v := os.Getenv("WB2A_CODEX_COMPAT"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.CodexCompat = b
		}
	}
	if v := os.Getenv("WB2A_RESPONSES_API"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.ResponsesAPI = b
		}
	}
	if v := os.Getenv("WB2A_MAX_REQUEST_BODY_MIB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxRequestBodyMiB = n
		}
	}
	if v := os.Getenv("WB2A_INPUT_CREDITS_PER_1K"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			c.Billing.InputCreditsPer1KTokens = n
		}
	}
	if v := os.Getenv("WB2A_OUTPUT_CREDITS_PER_1K"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			c.Billing.OutputCreditsPer1KTokens = n
		}
	}
	if v := os.Getenv("WB2A_CACHED_INPUT_CREDITS_PER_1K"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			c.Billing.CachedInputCreditsPer1KTokens = n
		}
	}
}

func (c *Config) normalize() error {
	var err error
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	if strings.TrimSpace(c.SMS.Proxy.Cooldown) == "" {
		c.SMS.Proxy.Cooldown = "30m"
	}
	if c.SMSProxyCooldownDur, err = time.ParseDuration(c.SMS.Proxy.Cooldown); err != nil {
		return fmt.Errorf("sms.proxy.cooldown: %w", err)
	}
	// reqproxy 基础设施参数（全可选：空串/零值 = 默认）
	if s := strings.TrimSpace(c.ReqProxy.HealthInterval); s != "" {
		if c.ReqProxyHealthDur, err = time.ParseDuration(s); err != nil {
			return fmt.Errorf("reqproxy.health_interval: %w", err)
		}
	}
	if s := strings.TrimSpace(c.ReqProxy.LatencyTimeout); s != "" {
		if c.ReqProxyLatencyDur, err = time.ParseDuration(s); err != nil {
			return fmt.Errorf("reqproxy.latency_timeout: %w", err)
		}
	}
	if s := strings.TrimSpace(c.ReqProxy.UnhealthyCooldown); s != "" {
		if c.ReqProxyUnhealthyDur, err = time.ParseDuration(s); err != nil {
			return fmt.Errorf("reqproxy.unhealthy_cooldown: %w", err)
		}
	}
	if c.Postgres.ConnMaxLifetime == "" {
		c.Postgres.ConnMaxLifetime = "30m"
	}
	if c.Postgres.ConnMaxIdleTime == "" {
		c.Postgres.ConnMaxIdleTime = "5m"
	}
	if c.PostgresMaxLifetime, err = time.ParseDuration(c.Postgres.ConnMaxLifetime); err != nil {
		return fmt.Errorf("postgres.conn_max_lifetime: %w", err)
	}
	if c.PostgresMaxIdleTime, err = time.ParseDuration(c.Postgres.ConnMaxIdleTime); err != nil {
		return fmt.Errorf("postgres.conn_max_idle_time: %w", err)
	}
	if c.Postgres.MaxOpenConns <= 0 {
		c.Postgres.MaxOpenConns = 16
	}
	if c.Postgres.MaxIdleConns <= 0 || c.Postgres.MaxIdleConns > c.Postgres.MaxOpenConns {
		c.Postgres.MaxIdleConns = c.Postgres.MaxOpenConns / 2
		if c.Postgres.MaxIdleConns < 1 {
			c.Postgres.MaxIdleConns = 1
		}
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	// 流式：总时长 <=0 = 不限（默认，长回答不被掐断）；空闲窗口 0 = 默认 120s，
	// 负数 = 显式关闭。用 -1 而不是 0 表示"关闭"，是为了不让漏配/写 0 意外关掉
	// 唯一的流式保护——关掉空闲检查后，上游卡住只能靠调用方断开或总上限兜底。
	if secs := c.Upstream.StreamTimeoutSeconds; secs > 0 {
		c.StreamTimeoutDur = time.Duration(secs) * time.Second
	} else {
		c.StreamTimeoutDur = 0
	}
	switch {
	case c.Upstream.StreamIdleSeconds < 0:
		c.StreamIdleDur = 0 // 显式关闭
	case c.Upstream.StreamIdleSeconds == 0:
		c.StreamIdleDur = 120 * time.Second
	default:
		c.StreamIdleDur = time.Duration(c.Upstream.StreamIdleSeconds) * time.Second
	}
	c.Billing.InputCreditsPer1KTokens = validCreditRate(c.Billing.InputCreditsPer1KTokens)
	c.Billing.OutputCreditsPer1KTokens = validCreditRate(c.Billing.OutputCreditsPer1KTokens)
	c.Billing.CachedInputCreditsPer1KTokens = validCreditRate(c.Billing.CachedInputCreditsPer1KTokens)
	// 请求体上限：0/负数 = 未配置 → 默认 8 MiB；显式配置时必须在 1-64。
	// 上限不能开放到任意大：请求体会全量读进内存，无界上限等于自拒式
	// 内存耗尽（32 位平台上 int 溢出也会在这里被范围检查拦下）。
	if c.MaxRequestBodyMiB == 0 {
		c.MaxRequestBodyMiB = 8
	}
	if c.MaxRequestBodyMiB < 1 || c.MaxRequestBodyMiB > 64 {
		return fmt.Errorf("max_request_body_mib must be 1-64, got %d", c.MaxRequestBodyMiB)
	}
	c.Region = strings.ToLower(strings.TrimSpace(c.Region))
	if c.Region == "" {
		c.Region = "cn"
	}
	// 排程开关归一：没显式写 false 的项保持启用（Default 已设 true）。
	c.ScheduleEnabled.Checkin = c.Schedule.CheckinEnabled
	c.ScheduleEnabled.Travel = c.Schedule.TravelEnabled
	c.ScheduleEnabled.Activity = c.Schedule.ActivityEnabled
	c.ScheduleEnabled.Keepalive = c.Schedule.KeepaliveEnabled
	c.ScheduleEnabled.Blackcat = c.Schedule.BlackcatEnabled
	c.ScheduleEnabled.AutoenrollGrowthTasks = c.Schedule.AutoenrollGrowthTasks
	// 小时范围校验：仅对启用中的排程校验（禁用项允许保留非法占位值不报错，
	// 改回 enabled 时再被校验拦下）。0-23 整点。
	for _, pair := range []struct {
		name    string
		hours   []int
		enabled bool
	}{
		{"schedule.checkin_hours", c.Schedule.CheckinHours, c.ScheduleEnabled.Checkin},
		{"schedule.travel_hours", c.Schedule.TravelHours, c.ScheduleEnabled.Travel},
		{"schedule.activity_hours", c.Schedule.ActivityHours, c.ScheduleEnabled.Activity},
		{"schedule.keepalive_hours", c.Schedule.KeepaliveHours, c.ScheduleEnabled.Keepalive},
		{"schedule.blackcat_hours", c.Schedule.BlackcatHours, c.ScheduleEnabled.Blackcat},
	} {
		if !pair.enabled {
			continue
		}
		for _, h := range pair.hours {
			if h < 0 || h > 23 {
				return fmt.Errorf("%s: 小时必须在 0-23 之间（got %d）", pair.name, h)
			}
		}
	}
	if c.Region == "mixed" {
		c.Region = "all"
	}
	if c.Region != "cn" && c.Region != "global" && c.Region != "all" {
		return fmt.Errorf("region must be cn, global, or all, got %q", c.Region)
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// pprof：只有显式开启才生效。地址默认 127.0.0.1:6060；显式给 ":port"
	// 或 0.0.0.0 拒绝——pprof 端点无鉴权，暴露出去等于把堆内存/goroutine
	// 全量数据（通常含密钥）送给任何能连到该端口的人。
	if c.Pprof.Enabled {
		addr := strings.TrimSpace(c.Pprof.Addr)
		if addr == "" {
			addr = "127.0.0.1:6060"
		}
		// 配置文件里显式写了空串 = 想要默认地址之外的错误写法，直接报错
		// 提示正确写法，而不是静默落到默认值（静默会让 ":6060" 看起来"能用"）。
		if strings.TrimSpace(c.Pprof.Addr) == "" && c.Pprof.Addr != "" {
			return fmt.Errorf("pprof.addr must be an explicit host:port (e.g. 127.0.0.1:6060), got %q", c.Pprof.Addr)
		}
		host, _, err := net.SplitHostPort(addr)
		if err != nil || host == "" || host == "0.0.0.0" || host == "::" {
			return fmt.Errorf("pprof.addr must be an explicit host:port bound to a specific interface (e.g. 127.0.0.1:6060), got %q", c.Pprof.Addr)
		}
		c.PprofAddr = addr
	}
	return nil
}

// WatchJSON config.json 里 autoenroll.watch 节的 JSON 结构。
// 与 server.WatchConfig 字段一致但独立声明：cmd/server 不 import
// internal/server（保持 main 的依赖面干净），main 加载后翻译过去。
type WatchJSON struct {
	Enabled         bool             `json:"enabled"`
	IntervalSeconds int              `json:"interval_seconds"`
	WantPerTrigger  int              `json:"want_per_trigger"`
	Workers         int              `json:"workers"`
	Groups          []string         `json:"groups"`
	Projects        []WatchProjectJSON `json:"projects"`
}

// WatchProjectJSON 单个监控项目（同上，翻译用）。
type WatchProjectJSON struct {
	Sid      string  `json:"sid"`
	HexSID   string  `json:"hex_sid"`
	Name     string  `json:"name"`
	MaxPrice float64 `json:"max_price"`
	MinStock int     `json:"min_stock"`
	Enabled  bool    `json:"enabled"`
}

func validCreditRate(value float64) float64 {
	if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return value
}
