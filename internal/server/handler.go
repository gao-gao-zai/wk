// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/groups"
	"workbuddy2api/internal/haozhuma"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/smslogin"
	"workbuddy2api/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool             *pool.Pool
	Upstream         *upstream.Client
	APIKey           string // 管理员密钥（空 = 无管理员密钥；多密钥体系下的全权限密钥）
	FrontendPassword string // 前端控制台密码；空 = 不启用前端密码
	// Groups 分组 + 多密钥存储（可选；nil = 分组功能关闭，仅管理员密钥）。
	Groups *groups.Store
	ConfigPath       string // 配置文件路径，供控制台保存签到配置
	AuthDir          string
	Region           string
	LoginBin         string // OAuth 登录辅助程序路径
	CheckinNow       func()
	CreditRefreshNow func()
	// CheckinAccount / KeepaliveAccount 按单账号执行签到/保活，供控制台手动触发。
	CheckinAccount   func(uid string) scheduler.AccountResult
	KeepaliveAccount func(uid string) scheduler.AccountResult
	// SMSLogin 短信直登（中国区）。nil = 关闭该入口，控制台回退到 OAuth 链接。
	SMSLogin *smslogin.Manager
	// HaozhumaClient 豪猪接码客户端（可选）。配置了 sms.haozhuma 时由 main
	// 注入；NewHandler 内部用它组装 AutoEnroll（persist/find 回调指向 handler）。
	HaozhumaClient *haozhuma.Client
	// HaozhumaSid 豪猪项目 ID（如 52283 腾讯科技[限对接]）。
	HaozhumaSid string
	// SetHaozhumaSid 运行时切换豪猪项目 ID。NewHandler 组装 AutoEnroll 后
	// 自动接上（见 NewHandler）；main 无需注入。nil = 自动加号未启用，
	// WebUI 改 sid 只落盘（重启后生效）。
	SetHaozhumaSid func(sid string)
	// SetRequestLogRetention 运行时更新请求日志保留策略（main 注入：推给
	// metricsstore 的 SetRetention）。nil = 未启用持久化存储，WebUI 改
	// 保留策略只落盘（重启后生效）。
	SetRequestLogRetention func(days, rows int)
	// AutoEnroll 豪猪自动加号（NewHandler 内部组装）。nil = 未启用该端点。
	AutoEnroll *AutoEnroller
	// AutoEnrollLedger 号码账本落盘路径。号码取走后会占住豪猪的并发额度，
	// 额度满了后续取号全部失败（报"余额不足,请释放拉黑后再取号"）。落盘后
	// 容器被 SIGKILL 重启也能在下次启动时补释放（见 ReclaimOrphans）。
	// 空 = 不落盘（只用内存账本，任务收尾仍会兜底释放）。
	AutoEnrollLedger string
	UpdateSchedule   func(checkinHours, keepaliveHours []int)
	MaxRotate        int // 单请求最多换号次数，默认 3
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429 冷却，默认 60s
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m
	// FrontendDir 静态控制台资源目录（默认 "frontend"，相对进程工作目录）。
	// 只放行 index.html 与 assets/index-<hash>.{js,css}，见 staticConsole。
	FrontendDir string
	// TrustedProxies 是允许其 X-Forwarded-For / X-Real-IP 生效的反向代理地址，
	// 支持单地址或 CIDR。空 = 谁都不信（安全默认，直接用 RemoteAddr）。
	// 只有确定服务一定在代理后面、且直连不可达时才配置，否则攻击者可以伪造
	// 头来绕开基于 IP 的解锁锁定。
	TrustedProxies  []string
	ResponseStore   ResponseStore
	MetricsStore    MetricsStore
	RequestLogStore RequestLogStore
	CompletionStore CompletionStore // optional atomic writer replacing separate metric/log writes
	CreditPolicy    CreditPolicy
	Passthrough     bool
	// DisableResponses 关闭 Responses 端点（/v1/responses、/responses
	// 返回 404）。反转语义（而不是 ResponsesAPI）是因为 Config 零值必须
	// 保持"端点开"——大量单测直接构造 Config{}，默认关会全部打挂。
	// main 从 features.responses_api 反转后传入。
	DisableResponses bool
	// SetSanitizeFingerprints / SetCodexCompat 把 WebUI 保存的特性开关
	// 推到 upstream Client（运行时即时生效）。可选：nil 时仅更新 handler 本地副本。
	SetSanitizeFingerprints func(bool)
	SetCodexCompat          func(bool)
	// SetUpstreamTimeouts 把 WebUI 保存的上游超时推到 upstream Client。
	// 参数单位：秒；streamTotal/streamIdle 0 = 不限/不查（与 config 语义一致，
	// 但不支持 -1 关闭——WebUI 上「0」即代表关闭，落盘时再翻译回 -1）。
	// 可选：nil 时仅落盘（重启后生效）。
	SetUpstreamTimeouts func(requestSecs, streamTotalSecs, streamIdleSecs int)
}

// ResponseStore is the optional Redis-backed persistence used by
// previous_response_id across restarts and replicas.
type ResponseStore interface {
	SaveResponse(id string, data []byte, ttl time.Duration)
	LoadResponse(id string) ([]byte, bool)
}

// Handler 主路由。
type Handler struct {
	cfg             Config
	mux             *http.ServeMux
	sessionsMu      sync.Mutex
	sessions        map[string]time.Time
	configMu        sync.Mutex
	responsesMu     sync.Mutex
	responseHistory map[string]storedResponse
	responseBytes   int
	unlockRateMu    sync.Mutex
	unlockAttempts  map[string]unlockAttempt
	// 全局解锁失败窗口（跨 IP），见 recordGlobalUnlockFailureLocked。
	unlockGlobalFailures     int
	unlockGlobalWindowStart  time.Time
	unlockGlobalBlockedUntil time.Time

	// featuresMu 保护运行时可变的特性开关与费率（WebUI 保存即时生效，
	// 同时落盘 config.json 供重启后保持）。读侧在请求热路径，写侧仅管理
	// 端点，锁竞争可忽略。
	featuresMu    sync.RWMutex
	passthrough   bool
	responsesOff  bool
	creditPolicy  CreditPolicy
	sanitizeHooks struct {
		// upstream SetSanitizeFingerprints / SetCodexCompat 回调；nil 时
		// （如单测直接构造 Handler）仅更新本地副本，不外呼。
		setSanitize func(bool)
		setCodex    func(bool)
	}
}

type unlockAttempt struct {
	windowStart  time.Time
	failures     int
	blockedUntil time.Time
}

type storedResponse struct {
	messages  []map[string]any
	parentID  string
	routeKey  string
	expiresAt time.Time
	size      int
}

type storedResponseWire struct {
	Messages []map[string]any `json:"messages"`
	ParentID string           `json:"parent_id,omitempty"`
	RouteKey string           `json:"route_key"`
}

const (
	responseHistoryTTL      = time.Hour
	maxResponseHistory      = 1024
	maxResponseHistoryBytes = 64 << 20
	maxRequestBodyBytes     = 8 << 20
	unlockFailureLimit      = 5
	unlockFailureWindow     = time.Minute
	unlockBlockDuration     = 5 * time.Minute
	// 全局解锁上限：per-IP 锁定挡不住换 IP 的暴力破解，这一层让整体速率有上界。
	unlockGlobalFailureLimit  = 50
	unlockGlobalWindow        = 5 * time.Minute
	unlockGlobalBlockDuration = 15 * time.Minute
)

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), sessions: make(map[string]time.Time), responseHistory: make(map[string]storedResponse), unlockAttempts: make(map[string]unlockAttempt)}
	// 运行时特性开关/费率从启动配置拷贝一份；WebUI 保存时经 updateFeatures
	// 同时改这里和 upstream Client（见 saveAdminConfig）。sanitizeHooks 为 nil
	// 时（单测直接构造 Handler）只更新本地副本。
	h.passthrough = cfg.Passthrough
	h.responsesOff = cfg.DisableResponses
	h.creditPolicy = cfg.CreditPolicy
	h.featuresMu.Lock()
	h.sanitizeHooks.setSanitize = cfg.SetSanitizeFingerprints
	h.sanitizeHooks.setCodex = cfg.SetCodexCompat
	h.featuresMu.Unlock()
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /v1/responses", h.withAuth(h.responses))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	// Some OpenAI-compatible clients append endpoint paths to a bare base URL.
	// Route these aliases through exactly the same authentication and handlers.
	h.mux.HandleFunc("POST /chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /responses", h.withAuth(h.responses))
	h.mux.HandleFunc("GET /models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /stats", h.withAuth(h.stats))
	h.mux.HandleFunc("GET /requests", h.withAuth(h.requests))
	h.mux.HandleFunc("GET /v1/requests", h.withAuth(h.requests))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	h.mux.HandleFunc("POST /admin/unlock", h.unlock)
	h.mux.HandleFunc("GET /admin/config", h.withFrontend(h.adminConfig))
	h.mux.HandleFunc("POST /admin/config", h.withFrontend(h.saveAdminConfig))
	// 分组 + 密钥管理（多密钥体系）。分组存储未启用时端点回 503。
	h.mux.HandleFunc("GET /admin/groups", h.withFrontend(h.adminGroups))
	h.mux.HandleFunc("POST /admin/groups", h.withFrontend(h.adminGroups))
	h.mux.HandleFunc("PUT /admin/groups/{name}", h.withFrontend(h.adminGroup))
	h.mux.HandleFunc("DELETE /admin/groups/{name}", h.withFrontend(h.adminGroup))
	h.mux.HandleFunc("GET /admin/accounts/{uid}/groups", h.withFrontend(h.adminAccountGroups))
	h.mux.HandleFunc("PUT /admin/accounts/{uid}/groups", h.withFrontend(h.adminAccountGroups))
	h.mux.HandleFunc("GET /admin/keys", h.withFrontend(h.adminKeys))
	h.mux.HandleFunc("POST /admin/keys", h.withFrontend(h.adminKeys))
	h.mux.HandleFunc("PUT /admin/keys/{id}", h.withFrontend(h.adminKey))
	h.mux.HandleFunc("DELETE /admin/keys/{id}", h.withFrontend(h.adminKey))
	h.mux.HandleFunc("POST /admin/checkin", h.withFrontend(h.runCheckin))
	h.mux.HandleFunc("POST /admin/credits/refresh", h.withFrontend(h.refreshCredits))
	h.mux.HandleFunc("POST /admin/account/url", h.withFrontend(h.accountURL))
	h.mux.HandleFunc("POST /admin/account/poll", h.withFrontend(h.accountPoll))
	// 短信直登：发码 + 验码落盘，省掉浏览器授权。
	h.mux.HandleFunc("POST /admin/account/sms/send", h.withFrontend(h.accountSMSSend))
	h.mux.HandleFunc("POST /admin/account/sms/verify", h.withFrontend(h.accountSMSVerify))
	h.mux.HandleFunc("POST /admin/account/sms/captcha", h.withFrontend(h.accountSMSSubmitCaptcha))
	h.mux.HandleFunc("POST /admin/account/sms/auto-enroll", h.withFrontend(h.accountSMSAutoEnroll))
	h.mux.HandleFunc("GET /admin/account/sms/auto-enroll", h.withFrontend(h.accountSMSAutoEnrollStatus))
	h.mux.HandleFunc("POST /admin/account/sms/auto-enroll/stop", h.withFrontend(h.accountSMSAutoEnrollStop))
	// 一键释放豪猪名下所有占用号码（cancelAllRecv）：额度被旧号占满时的
	// 手动兜底，对齐豪猪后台的"释放全部"按钮。
	h.mux.HandleFunc("POST /admin/account/sms/release-all", h.withFrontend(h.accountSMSReleaseAll))
	h.mux.HandleFunc("GET /admin/proxy/status", h.withFrontend(h.proxyStatus))
	h.mux.HandleFunc("POST /admin/account/{uid}/enable", h.withFrontend(h.enableAccount))
	h.mux.HandleFunc("POST /admin/account/{uid}/disable", h.withFrontend(h.disableAccount))
	h.mux.HandleFunc("POST /admin/account/{uid}/clear-cooldown", h.withFrontend(h.clearCooldownAccount))
	h.mux.HandleFunc("POST /admin/account/{uid}/checkin", h.withFrontend(h.checkinAccount))
	h.mux.HandleFunc("POST /admin/account/{uid}/keepalive", h.withFrontend(h.keepaliveAccount))
	h.mux.HandleFunc("DELETE /admin/account/{uid}", h.withFrontend(h.deleteAccount))
	// 自动加号：豪猪取号→短信直登→落盘。persist/find 回调指向本 handler，
	// 必须在 NewHandler 里组装（main 那边拿不到方法值）。
	if cfg.HaozhumaClient != nil && cfg.SMSLogin != nil && cfg.HaozhumaSid != "" {
		h.cfg.AutoEnroll = NewAutoEnroller(
			cfg.SMSLogin, cfg.HaozhumaClient, cfg.HaozhumaSid,
			func(cred accountCredential, region string) (map[string]any, int, error) {
				return h.persistAccount(cred, region)
			},
			func(mobile string) (string, bool) {
				st, ok := h.findAccountByMobile(mobile)
				if !ok {
					return "", false
				}
				return st.Nickname, true
			},
			func(uid string, gs []string) error {
				if h.cfg.Groups == nil {
					return nil // 分组存储未启用：登记是无操作
				}
				return h.cfg.Groups.SetAccountGroups(uid, gs)
			},
		)
		h.cfg.AutoEnroll.SetLedgerPath(cfg.AutoEnrollLedger)
		// WebUI 运行时切换项目 ID 直接推给 AutoEnroll（组装在 NewHandler
		// 内部，main 拿不到指针，这里自接回调最省事）。
		h.cfg.SetHaozhumaSid = h.cfg.AutoEnroll.SetSid
	}
	// Static console assets are served through an explicit allow-list (see
	// staticConsoleHandler) instead of http.FileServer(http.Dir("frontend")).
	// FileServer applied no filtering at all: it served every regular file under
	// the directory and generated an HTML index for any subdirectory lacking
	// index.html. Since the directory is resolved relative to the process working
	// directory, anything that landed in frontend/ — editor backups, a copied
	// config.json, an auth dumps file — became downloadable, and the whole
	// console bundle was readable without authenticating.
	h.mux.Handle("/", h.staticConsole())
	return h
}

// Console asset allow-list.
//
// Only the Vite build output is reachable: the HTML shell, and hashed
// JS/CSS bundles under assets/. Everything else under FrontendDir — including
// any stray file an operator or a backup tool happens to drop there — is 404.
// Hashed asset names are matched by pattern rather than enumerated, so a rebuild
// that changes the content hash does not require a code change.
var (
	consoleAssetNamePattern = regexp.MustCompile(`^index-[A-Za-z0-9_-]{6,}\.(js|css)$`)
	consoleAssetTypes       = map[string]string{
		".js":  "text/javascript; charset=utf-8",
		".css": "text/css; charset=utf-8",
	}
)

// staticConsole serves the console shell and its built assets from an allow-list.
// AutoEnroller 返回组装好的自动加号器（未启用豪猪时为 nil）。
// main 需要在开始服务前调用 ReclaimOrphans 补释放上次遗留的号码。
func (h *Handler) AutoEnroller() *AutoEnroller {
	return h.cfg.AutoEnroll
}

func (h *Handler) staticConsole() http.Handler {
	dir := h.cfg.FrontendDir
	if dir == "" {
		dir = "frontend"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// A catch-all GET-only static route must not double as a sink for
			// arbitrary methods destined for unknown paths.
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := path.Clean("/" + r.URL.Path)
		switch {
		case name == "/" || name == "/index.html":
			serveConsoleFile(w, r, filepath.Join(dir, "index.html"), "text/html; charset=utf-8")
		case strings.HasPrefix(name, "/assets/"):
			// Reject a trailing slash before taking path.Base: Base("/assets/x.js/")
			// is "x.js", which would let the odd path through the pattern below.
			if strings.HasSuffix(r.URL.Path, "/") {
				http.NotFound(w, r)
				return
			}
			asset := path.Base(name)
			// path.Clean collapsed any "../" already, and the pattern admits no
			// separator, so this cannot escape the assets/ directory.
			if !consoleAssetNamePattern.MatchString(asset) {
				http.NotFound(w, r)
				return
			}
			// The legacy v1 console (app.js/admin.css/styles.css) is deliberately
			// NOT served: its inline API keys and unauthenticated endpoints are the
			// subject of separate findings, and the React console replaces it.
			serveConsoleFile(w, r, filepath.Join(dir, "assets", asset), consoleAssetTypes[path.Ext(asset)])
		default:
			http.NotFound(w, r)
		}
	})
}

// serveConsoleFile writes one allow-listed file, or 404 when it is absent.
func serveConsoleFile(w http.ResponseWriter, r *http.Request, filename, contentType string) {
	info, err := os.Stat(filename)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	// The shell is what selects a hashed bundle, so it must not be cached
	// blindly; hashed assets are immutable by construction.
	if strings.HasSuffix(filename, "index.html") {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	http.ServeFile(w, r, filename)
}

// securityHeaders 统一给每个响应补上基础安全头。
//
// 这些不是"锦上添花"：控制台是一个能查看账号/改配置的页面，没有
// nosniff 时浏览器可能把 assets 当 HTML 解析，没有 frame-ancestors 时
// 任何站点都能把它套进 iframe 做点击劫持，没有 Referrer-Policy 时
// 控制台 URL 会随着外链把信息带出去。
func securityHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	// SAMEORIGIN 而不是 DENY：控制台自身没有需要嵌套的页面，
	// 但同源内嵌在本地部署（反代同名域）下是常见用法。
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	// API 响应返回 JSON，不应被任何页面当作脚本执行。
	//
	// style-src 必须带 'unsafe-inline'：Ant Design 用 cssinjs 在运行时注入
	// <style>，一律禁止会让控制台布局整个失效。样式注入的利用价值远低于脚本
	// 注入，这里是有意的取舍；script-src 依然保持 'self'（Vite 产物是外部文件，
	// 不需要内联脚本），那才是真正要守住的一条。
	h.Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data:; connect-src 'self'; frame-ancestors 'self'; "+
			"object-src 'none'; base-uri 'none'; form-action 'self'")
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w.Header())
	h.mux.ServeHTTP(w, r)
}

// authOK reports whether the request carries a valid credential.
//
// It is deliberately deny-by-default: if no credential is configured, neither
// validAPIKey nor frontendSession can validate anything, so every request is
// refused. An unconfigured service must never be an open service — the previous
// positive guard (`APIKey != ""`) made "no credential configured" mean "no
// authentication required", which turned every route public.
func (h *Handler) authOK(r *http.Request) bool {
	return h.validAPIKey(r) || h.frontendSession(r)
}

// requestScope 是一次请求的鉴权结果：密钥身份 + 分组作用域。
//
//   - admin（管理员）：config 全局 APIKey 命中或前端会话。可选所有分组
//     的账号；管理端点只对管理员开放（密钥即使是分组密钥也不能进管理面）。
//   - group（分组密钥）：keys.json 里的密钥命中。只能用绑定分组的账号；
//     不能访问 /admin/*。
type requestScope struct {
	kind     string // "admin" | "group"
	group    string // group 密钥的绑定分组（"" = 不限，但 kind=group 时恒非空）
	keyID    string // 密钥 id（日志归因用）
	adminKey bool   // true = 命中 config 全局 APIKey
}

type scopeKey struct{}

// scopeFromRequest 取请求的鉴权作用域；withAuth 已保证非 nil。
func scopeFromRequest(r *http.Request) requestScope {
	if s, ok := r.Context().Value(scopeKey{}).(requestScope); ok {
		return s
	}
	// 未走 withAuth 的路径（理论上不存在）按最保守处理：空分组 = 选不到任何账号。
	return requestScope{kind: "group", group: ""}
}

// resolveScope 解析 Bearer 密钥 → 作用域。顺序：
//  1. config 全局 APIKey（管理员）
//  2. keys.json 分组密钥
//
// 不命中返回 false。
func (h *Handler) resolveScope(r *http.Request) (requestScope, bool) {
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer ") {
		return requestScope{}, false
	}
	token := strings.TrimPrefix(authz, "Bearer ")
	if h.cfg.APIKey != "" && token == h.cfg.APIKey {
		return requestScope{kind: "admin", adminKey: true}, true
	}
	if h.cfg.Groups != nil {
		if k, err := h.cfg.Groups.LookupKey(token); err == nil {
			if k.Group == "" {
				// 不绑分组的密钥按管理员语义（全池可用）。创建入口
				// 不再产生这种密钥，但旧数据/手改文件可能存在。
				return requestScope{kind: "admin", keyID: k.ID}, true
			}
			return requestScope{kind: "group", group: k.Group, keyID: k.ID}, true
		}
	}
	return requestScope{}, false
}

// allowSet 把作用域转成选号过滤集合。管理员/无分组存储返回 nil（不过滤）；
// 分组密钥返回该分组的 uid 集合（可能为空 map = 该分组无账号，选号直接 nil）。
func (h *Handler) allowSet(s requestScope) map[string]bool {
	if s.kind != "group" || h.cfg.Groups == nil {
		return nil
	}
	return h.cfg.Groups.AccountsInGroup(s.group)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Bearer 密钥（管理员或分组密钥）→ 解析作用域注入 context。
		// 前端会话没有 Bearer，走管理语义（控制台本身不受分组限制）。
		scope, ok := h.resolveScope(r)
		if !ok && h.frontendSession(r) {
			scope, ok = requestScope{kind: "admin"}, true
		}
		if !ok {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), scopeKey{}, scope)))
	}
}

func (h *Handler) validAPIKey(r *http.Request) bool {
	if h.cfg.APIKey == "" {
		return false
	}
	authz := r.Header.Get("Authorization")
	return strings.HasPrefix(authz, "Bearer ") && strings.TrimPrefix(authz, "Bearer ") == h.cfg.APIKey
}

func (h *Handler) frontendSession(r *http.Request) bool {
	if h.cfg.FrontendPassword == "" {
		return false
	}
	c, err := r.Cookie("wb2api_frontend")
	if err != nil {
		return false
	}
	h.sessionsMu.Lock()
	defer h.sessionsMu.Unlock()
	expires, ok := h.sessions[c.Value]
	if !ok || time.Now().After(expires) {
		delete(h.sessions, c.Value)
		return false
	}
	return true
}

func (h *Handler) withFrontend(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Admin endpoints accept only the admin credential (global API key or
		// frontend session cookie). Group keys deliberately cannot reach the
		// management plane. Deny-by-default: see authOK.
		if !h.validAPIKey(r) && !h.frontendSession(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "frontend_locked", "message": "admin credential required"}})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), scopeKey{}, requestScope{kind: "admin"})))
	}
}

func (h *Handler) unlock(w http.ResponseWriter, r *http.Request) {
	// 用 clientHost 而不是裸的 RemoteAddr：只有配置了可信代理时才采信 XFF，
	// 否则攻击者能靠换头把每次尝试放进新的限流桶，锁定形同虚设。
	clientKey := h.clientHost(r)
	if retryAfter := h.unlockRetryAfter(clientKey); retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "error": "尝试次数过多，请稍后重试"})
		return
	}
	var req struct {
		Password string `json:"password"`
		// Key 管理员密钥（config api_key）。多密钥体系下控制台解锁的主路径：
		// 分组密钥不能解锁控制台（管理面只认管理员）。
		Key string `json:"key"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		h.recordUnlockFailure(clientKey)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "请求格式不正确"})
		return
	}
	// 双通道：管理员密钥（主）或前端密码（兼容保留——老部署的 config 里
	// 只配了 frontend_password 时仍可登录）。
	ok := req.Key != "" && h.cfg.APIKey != "" && req.Key == h.cfg.APIKey
	if !ok && h.cfg.FrontendPassword != "" && req.Password != "" && req.Password == h.cfg.FrontendPassword {
		ok = true
	}
	if !ok {
		h.recordUnlockFailure(clientKey)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "密钥或密码错误"})
		return
	}
	h.clearUnlockFailures(clientKey)
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, "session error", 500)
		return
	}
	token := hex.EncodeToString(b)
	h.sessionsMu.Lock()
	h.sessions[token] = time.Now().Add(24 * time.Hour)
	h.sessionsMu.Unlock()
	// Secure：仅在请求确实走 HTTPS 时设置。无条件设置会让纯 HTTP 的本地部署
	// 完全无法登录（浏览器直接丢弃 cookie）。TLS 可能终止在反向代理上，所以
	// 当且仅当对端是可信代理时才采信 X-Forwarded-Proto，否则攻击者只要加一个
	// 头就能让我们发出 Secure cookie 从而让控制台登录静默失效。
	secure := r.TLS != nil || (h.isTrustedProxy(remoteHost(r.RemoteAddr)) &&
		strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https"))
	http.SetCookie(w, &http.Cookie{
		Name: "wb2api_frontend", Value: token, Path: "/",
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: 86400,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// clientHost returns the address the request should be attributed to.
//
// r.RemoteAddr is the only value an attacker cannot forge, so it is the default.
// X-Forwarded-For / X-Real-IP are honoured **only** when the immediate peer is a
// configured trusted proxy: otherwise a client could pick its own rate-limit
// bucket per request by rotating a header, which defeats the unlock lockout
// entirely. Trusting XFF unconditionally (the common mistake) is strictly worse
// than ignoring it.
func (h *Handler) clientHost(r *http.Request) string {
	peer := remoteHost(r.RemoteAddr)
	if !h.isTrustedProxy(peer) {
		return peer
	}
	// Walk XFF right-to-left and return the first untrusted hop: the rightmost
	// entry is the one appended by our own trusted proxy, so it cannot be spoofed.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			candidate := remoteHost(strings.TrimSpace(parts[i]))
			if candidate == "" {
				continue
			}
			if !h.isTrustedProxy(candidate) {
				return candidate
			}
		}
	}
	if realIP := remoteHost(strings.TrimSpace(r.Header.Get("X-Real-IP"))); realIP != "" {
		return realIP
	}
	return peer
}

// isTrustedProxy reports whether host is one of the configured trusted proxies.
// An empty TrustedProxies list means "trust nobody" (the safe default).
func (h *Handler) isTrustedProxy(host string) bool {
	if host == "" {
		return false
	}
	for _, entry := range h.cfg.TrustedProxies {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if entry == host {
			return true
		}
		// Allow a CIDR entry so an operator can trust a whole subnet.
		if _, network, err := net.ParseCIDR(entry); err == nil {
			if ip := net.ParseIP(host); ip != nil && network.Contains(ip) {
				return true
			}
		}
	}
	return false
}

// remoteHost strips the port from a host:port pair, tolerating a bare host and
// bracketed IPv6 literals.
func remoteHost(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return strings.Trim(addr, "[]")
}

func (h *Handler) unlockRetryAfter(key string) int {
	if h.cfg.FrontendPassword == "" {
		return 0
	}
	now := time.Now()
	h.unlockRateMu.Lock()
	defer h.unlockRateMu.Unlock()
	h.cleanupUnlockAttemptsLocked(now)
	// 全局窗口：per-IP 锁定可以被换 IP / 换 XFF 绕过，这里再加一道总量限制，
	// 让分布式暴力破解也只能以很低的速率推进。
	if now.Before(h.unlockGlobalBlockedUntil) {
		seconds := int(time.Until(h.unlockGlobalBlockedUntil).Seconds())
		if seconds < 1 {
			seconds = 1
		}
		return seconds
	}
	attempt := h.unlockAttempts[key]
	if now.Before(attempt.blockedUntil) {
		seconds := int(time.Until(attempt.blockedUntil).Seconds())
		if seconds < 1 {
			seconds = 1
		}
		return seconds
	}
	return 0
}

func (h *Handler) recordUnlockFailure(key string) {
	if h.cfg.FrontendPassword == "" {
		return
	}
	now := time.Now()
	h.unlockRateMu.Lock()
	defer h.unlockRateMu.Unlock()
	h.cleanupUnlockAttemptsLocked(now)
	attempt := h.unlockAttempts[key]
	if attempt.windowStart.IsZero() || now.Sub(attempt.windowStart) >= unlockFailureWindow {
		attempt = unlockAttempt{windowStart: now}
	}
	attempt.failures++
	if attempt.failures >= unlockFailureLimit {
		attempt.blockedUntil = now.Add(unlockBlockDuration)
	}
	h.unlockAttempts[key] = attempt
	h.recordGlobalUnlockFailureLocked(now)
}

// recordGlobalUnlockFailureLocked 累计跨 IP 的失败数并触发全局封锁。
func (h *Handler) recordGlobalUnlockFailureLocked(now time.Time) {
	if h.unlockGlobalWindowStart.IsZero() || now.Sub(h.unlockGlobalWindowStart) >= unlockGlobalWindow {
		h.unlockGlobalWindowStart = now
		h.unlockGlobalFailures = 0
	}
	h.unlockGlobalFailures++
	if h.unlockGlobalFailures >= unlockGlobalFailureLimit {
		h.unlockGlobalBlockedUntil = now.Add(unlockGlobalBlockDuration)
		h.unlockGlobalFailures = 0
		h.unlockGlobalWindowStart = now
	}
}

func (h *Handler) clearUnlockFailures(key string) {
	h.unlockRateMu.Lock()
	delete(h.unlockAttempts, key)
	h.unlockRateMu.Unlock()
}

func (h *Handler) cleanupUnlockAttemptsLocked(now time.Time) {
	for key, attempt := range h.unlockAttempts {
		if now.Sub(attempt.windowStart) >= unlockFailureWindow && !now.Before(attempt.blockedUntil) {
			delete(h.unlockAttempts, key)
		}
	}
}

func (h *Handler) adminConfig(w http.ResponseWriter, r *http.Request) {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	if h.cfg.ConfigPath == "" {
		writeJSON(w, 200, map[string]any{})
		return
	}
	raw, err := os.ReadFile(h.cfg.ConfigPath)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	var c struct {
		Schedule struct {
			CheckinHours   []int `json:"checkin_hours"`
			KeepaliveHours []int `json:"keepalive_hours"`
		} `json:"schedule"`
		Region string `json:"region"`
		// Features/Billing 是 WebUI 可改的运行时开关与费率（第一批：
		// 布尔开关和估算系数，读时生效）。凭据/路径/存储类配置仍只能改文件。
		// responses_api 默认开：老配置文件里没有该字段时 unmarshal 得到
		// 零值 false，必须在解码前预置 true。
		Features struct {
			SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
			Passthrough                   bool `json:"passthrough"`
			CodexCompat                   bool `json:"codex_compat"`
			ResponsesAPI                  bool `json:"responses_api"`
		} `json:"features"`
		Billing struct {
			InputCreditsPer1KTokens       float64 `json:"input_credits_per_1k_tokens"`
			OutputCreditsPer1KTokens      float64 `json:"output_credits_per_1k_tokens"`
			CachedInputCreditsPer1KTokens float64 `json:"cached_input_credits_per_1k_tokens"`
		} `json:"billing"`
		// Upstream 超时（WebUI 可改；秒）。老配置没有该节时显示默认值。
		Upstream struct {
			TimeoutSeconds       int `json:"timeout_seconds"`
			StreamTimeoutSeconds int `json:"stream_timeout_seconds"`
			StreamIdleSeconds    int `json:"stream_idle_seconds"`
		} `json:"upstream"`
		// SMS 豪猪项目 ID（WebUI 可改，运行时生效）。
		SMS struct {
			Haozhuma struct {
				Sid string `json:"sid"`
			} `json:"haozhuma"`
		} `json:"sms"`
		// 请求日志保留策略（WebUI 可改，运行时生效）。config.json 里是
		// 平级的 request_log_retention_days / _rows；对前端组装成嵌套
		// request_log_retention 对象（与 POST 补丁形状一致）。
		RetentionDays int `json:"request_log_retention_days"`
		RetentionRows int `json:"request_log_retention_rows"`
	}
	c.Features.ResponsesAPI = true
	c.Upstream.TimeoutSeconds = 120
	c.Upstream.StreamIdleSeconds = 120
	if json.Unmarshal(raw, &c) != nil {
		writeJSON(w, 500, map[string]string{"error": "invalid config"})
		return
	}
	// 老配置里 stream_idle_seconds 用 -1 表示关闭：统一成 0（=关闭）给前端，
	// 避免输入框里出现负数。落盘时再翻译回 -1。
	if c.Upstream.StreamIdleSeconds < 0 {
		c.Upstream.StreamIdleSeconds = 0
	}
	// 保留策略：老配置（两个键都不存在）显示旧默认 1 万条，让前端表单
	// 有明确的初始值；双零（显式配了 0/0）同样按兜底语义回显默认值。
	if c.RetentionDays == 0 && c.RetentionRows == 0 {
		c.RetentionRows = 10000
	}
	resp := struct {
		Schedule     any `json:"schedule"`
		Region       any `json:"region"`
		Features     any `json:"features"`
		Billing      any `json:"billing"`
		Upstream     any `json:"upstream"`
		SMS          any `json:"sms"`
		Retention    any `json:"request_log_retention"`
	}{
		Schedule:  c.Schedule,
		Region:    c.Region,
		Features:  c.Features,
		Billing:   c.Billing,
		Upstream:  c.Upstream,
		SMS:       c.SMS,
		Retention: map[string]int{"days": c.RetentionDays, "rows": c.RetentionRows},
	}
	writeJSON(w, 200, resp)
}

// featuresPatch / billingPatch 是 POST /admin/config 的可选字段组。
// 指针类型区分"未提供"与"显式 false/0"：老前端只发 schedule 时两者为 nil，
// 落盘与运行时都不碰 features/billing。
type featuresPatch struct {
	SanitizeBlacklistFingerprints *bool `json:"sanitize_blacklist_fingerprints"`
	Passthrough                   *bool `json:"passthrough"`
	CodexCompat                   *bool `json:"codex_compat"`
	ResponsesAPI                  *bool `json:"responses_api"`
}

type billingPatch struct {
	InputCreditsPer1KTokens       *float64 `json:"input_credits_per_1k_tokens"`
	OutputCreditsPer1KTokens      *float64 `json:"output_credits_per_1k_tokens"`
	CachedInputCreditsPer1KTokens *float64 `json:"cached_input_credits_per_1k_tokens"`
}

// upstreamPatch 上游超时的可选字段组（WebUI「上游超时」卡片）。
// 三个值都是秒；语义与 config.json 的 upstream 节一致：
//   - timeout_seconds：非流式请求整请求上限（含控制面），默认 120；
//   - stream_timeout_seconds：流式总时长上限，0 = 不限（默认）；
//   - stream_idle_seconds：流式空闲上限，默认 120；-1 = 关闭检查。
type upstreamPatch struct {
	TimeoutSeconds       *int `json:"timeout_seconds"`
	StreamTimeoutSeconds *int `json:"stream_timeout_seconds"`
	StreamIdleSeconds    *int `json:"stream_idle_seconds"`
}

// smsPatch 豪猪接码设置的可选字段组（WebUI「自动加号」卡片）。当前只开放
// 项目 ID：账号/token 属于凭据，凭轮换走文件；uid（对接码钉死）依赖豪猪
// 后台的具体对接列表，WebUI 改错会让取号全挂，也不开放。
type smsPatch struct {
	Haozhuma struct {
		Sid *string `json:"sid"`
	} `json:"haozhuma"`
}

// retentionPatch 请求日志保留策略（WebUI「日志保留」卡片）。
// 指针区分"未提供"与"显式 0"：days=0 或 rows=0 表示对应条件不限。
type retentionPatch struct {
	Days *int `json:"days"`
	Rows *int `json:"rows"`
}

func (h *Handler) saveAdminConfig(w http.ResponseWriter, r *http.Request) {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	var req struct {
		CheckinHours   []int          `json:"checkin_hours"`
		KeepaliveHours []int          `json:"keepalive_hours"`
		Features       *featuresPatch `json:"features"`
		Billing        *billingPatch  `json:"billing"`
		Upstream       *upstreamPatch `json:"upstream"`
		SMS            *smsPatch      `json:"sms"`
		Retention      *retentionPatch `json:"request_log_retention"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&req) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	checkinHours, checkinOK := normalizeScheduleHours(req.CheckinHours)
	keepaliveHours, keepaliveOK := normalizeScheduleHours(req.KeepaliveHours)
	if !checkinOK || !keepaliveOK {
		writeJSON(w, 400, map[string]string{"error": "每项至少填写一个 0-23 的整数小时"})
		return
	}
	// 费率只接受非负有限值：负数/Inf/NaN 会污染积分估算与请求日志聚合。
	if req.Billing != nil {
		for _, v := range []*float64{req.Billing.InputCreditsPer1KTokens, req.Billing.OutputCreditsPer1KTokens, req.Billing.CachedInputCreditsPer1KTokens} {
			if v != nil && (*v < 0 || math.IsInf(*v, 0) || math.IsNaN(*v)) {
				writeJSON(w, 400, map[string]string{"error": "billing 费率必须是非负数值"})
				return
			}
		}
	}
	// 上游超时校验：三个值都是秒。
	// timeout_seconds：非流式整请求上限，必须为正（0 会让请求永远挂住）。
	// stream_timeout_seconds：流式总时长，0 = 不限（默认）。
	// stream_idle_seconds：流式空闲，0 = 关闭检查（落盘翻译回 -1）。
	if req.Upstream != nil {
		for _, pair := range []struct {
			v   *int
			ok  func(int) bool
			msg string
		}{
			{req.Upstream.TimeoutSeconds, func(v int) bool { return v >= 10 && v <= 3600 }, "timeout_seconds 需在 10-3600 秒之间"},
			{req.Upstream.StreamTimeoutSeconds, func(v int) bool { return v == 0 || (v >= 30 && v <= 86400) }, "stream_timeout_seconds 需为 0（不限）或 30-86400 秒"},
			{req.Upstream.StreamIdleSeconds, func(v int) bool { return v == 0 || (v >= 10 && v <= 3600) }, "stream_idle_seconds 需为 0（关闭）或 10-3600 秒"},
		} {
			if pair.v != nil && !pair.ok(*pair.v) {
				writeJSON(w, 400, map[string]string{"error": pair.msg})
				return
			}
		}
	}
	// 豪猪项目 ID：非空时必须是纯数字（豪猪 sid 是数字串，如 52283）。
	// 允许显式清空？不允许——清空等于关闭自动加号却让 UI 看起来还能用，
	// 那属于部署级变更，改配置文件。
	if req.SMS != nil && req.SMS.Haozhuma.Sid != nil {
		sid := strings.TrimSpace(*req.SMS.Haozhuma.Sid)
		if sid == "" {
			writeJSON(w, 400, map[string]string{"error": "豪猪项目 ID 不能为空（关闭自动加号请改配置文件）"})
			return
		}
		for _, r := range sid {
			if r < '0' || r > '9' {
				writeJSON(w, 400, map[string]string{"error": "豪猪项目 ID 必须是数字（如 52283）"})
				return
			}
		}
	}
	// 请求日志保留策略：天数 1-3650（10 年封顶），条数 100-1,000,000。
	// 0 合法（= 该条件不限）；负数无意义直接拒。双零 = 回到旧默认 1 万条
	//（metricsstore.Retention 的兜底语义），不需要在这里特判。
	if req.Retention != nil {
		for _, pair := range []struct {
			v   *int
			ok  func(int) bool
			msg string
		}{
			{req.Retention.Days, func(v int) bool { return v == 0 || (v >= 1 && v <= 3650) }, "retention days 需为 0（不限）或 1-3650 天"},
			{req.Retention.Rows, func(v int) bool { return v == 0 || (v >= 100 && v <= 1000000) }, "retention rows 需为 0（不限）或 100-1000000 条"},
		} {
			if pair.v != nil && !pair.ok(*pair.v) {
				writeJSON(w, 400, map[string]string{"error": pair.msg})
				return
			}
		}
	}
	raw, err := os.ReadFile(h.cfg.ConfigPath)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		writeJSON(w, 500, map[string]string{"error": "invalid config"})
		return
	}
	schedule := map[string]any{"checkin_hours": checkinHours, "keepalive_hours": keepaliveHours}
	doc["schedule"] = schedule

	// —— 落盘：只覆盖请求里显式给出的子字段，其余保留原值 ——
	if req.Features != nil {
		features, _ := doc["features"].(map[string]any)
		if features == nil {
			features = map[string]any{}
		}
		if req.Features.SanitizeBlacklistFingerprints != nil {
			features["sanitize_blacklist_fingerprints"] = *req.Features.SanitizeBlacklistFingerprints
		}
		if req.Features.Passthrough != nil {
			features["passthrough"] = *req.Features.Passthrough
		}
		if req.Features.CodexCompat != nil {
			features["codex_compat"] = *req.Features.CodexCompat
		}
		if req.Features.ResponsesAPI != nil {
			features["responses_api"] = *req.Features.ResponsesAPI
		}
		doc["features"] = features
	}
	if req.Billing != nil {
		billing, _ := doc["billing"].(map[string]any)
		if billing == nil {
			billing = map[string]any{}
		}
		if req.Billing.InputCreditsPer1KTokens != nil {
			billing["input_credits_per_1k_tokens"] = *req.Billing.InputCreditsPer1KTokens
		}
		if req.Billing.OutputCreditsPer1KTokens != nil {
			billing["output_credits_per_1k_tokens"] = *req.Billing.OutputCreditsPer1KTokens
		}
		if req.Billing.CachedInputCreditsPer1KTokens != nil {
			billing["cached_input_credits_per_1k_tokens"] = *req.Billing.CachedInputCreditsPer1KTokens
		}
		doc["billing"] = billing
	}
	if req.Upstream != nil {
		ups, _ := doc["upstream"].(map[string]any)
		if ups == nil {
			ups = map[string]any{}
		}
		if req.Upstream.TimeoutSeconds != nil {
			ups["timeout_seconds"] = *req.Upstream.TimeoutSeconds
		}
		if req.Upstream.StreamTimeoutSeconds != nil {
			ups["stream_timeout_seconds"] = *req.Upstream.StreamTimeoutSeconds
		}
		if req.Upstream.StreamIdleSeconds != nil {
			// WebUI 用 0 表示"关闭检查"；config 语义是 -1。翻译落盘，
			// GET 时再翻译回来（前端永远只见 0）。
			idle := *req.Upstream.StreamIdleSeconds
			if idle == 0 {
				idle = -1
			}
			ups["stream_idle_seconds"] = idle
		}
		doc["upstream"] = ups
	}
	if req.SMS != nil && req.SMS.Haozhuma.Sid != nil {
		sid := strings.TrimSpace(*req.SMS.Haozhuma.Sid)
		// 校验已保证非空数字。落盘保留 sms 节的其余字段（账号/token/uid）。
		sms, _ := doc["sms"].(map[string]any)
		if sms == nil {
			sms = map[string]any{}
		}
		hz, _ := sms["haozhuma"].(map[string]any)
		if hz == nil {
			hz = map[string]any{}
		}
		hz["sid"] = sid
		sms["haozhuma"] = hz
		doc["sms"] = sms
	}
	if req.Retention != nil && (req.Retention.Days != nil || req.Retention.Rows != nil) {
		// 未显式给出的字段沿用文件当前值（增量补丁语义，与 features 一致）。
		cur := struct {
			Days int `json:"request_log_retention_days"`
			Rows int `json:"request_log_retention_rows"`
		}{}
		_ = json.Unmarshal(raw, &cur)
		days, rows := cur.Days, cur.Rows
		if req.Retention.Days != nil {
			days = *req.Retention.Days
		}
		if req.Retention.Rows != nil {
			rows = *req.Retention.Rows
		}
		doc["request_log_retention_days"] = days
		doc["request_log_retention_rows"] = rows
	}

	out, _ := json.MarshalIndent(doc, "", "  ")
	if err := writeFileAtomic(h.cfg.ConfigPath, append(out, '\n'), 0600); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	restartRequired := h.cfg.UpdateSchedule == nil
	if h.cfg.UpdateSchedule != nil {
		h.cfg.UpdateSchedule(checkinHours, keepaliveHours)
	}
	// —— 运行时生效：开关与费率即时更新，不需要重启 ——
	updated := h.applyRuntimeFeatures(req.Features, req.Billing)
	h.applyRuntimeUpstreamTimeouts(req.Upstream, updated)
	if req.SMS != nil && req.SMS.Haozhuma.Sid != nil {
		sid := strings.TrimSpace(*req.SMS.Haozhuma.Sid)
		if h.cfg.SetHaozhumaSid != nil {
			h.cfg.SetHaozhumaSid(sid)
			updated["haozhuma_sid"] = sid
		} else {
			// 已落盘但没注入回调（老部署/测试）：重启后生效，如实告知。
			updated["haozhuma_sid"] = sid
			updated["haozhuma_sid_restart_required"] = true
		}
	}
	if req.Retention != nil && (req.Retention.Days != nil || req.Retention.Rows != nil) {
		cur := struct {
			Days int `json:"request_log_retention_days"`
			Rows int `json:"request_log_retention_rows"`
		}{}
		_ = json.Unmarshal(raw, &cur)
		days, rows := cur.Days, cur.Rows
		if req.Retention.Days != nil {
			days = *req.Retention.Days
		}
		if req.Retention.Rows != nil {
			rows = *req.Retention.Rows
		}
		if h.cfg.SetRequestLogRetention != nil {
			h.cfg.SetRequestLogRetention(days, rows)
			updated["request_log_retention"] = map[string]int{"days": days, "rows": rows}
		} else {
			updated["request_log_retention"] = map[string]int{"days": days, "rows": rows}
			updated["request_log_retention_restart_required"] = true
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "restart_required": restartRequired, "schedule": schedule, "updated": updated})
}

// applyRuntimeUpstreamTimeouts 把超时变更推到 upstream Client。
// 三个值只有请求里显式给出的才动；未给的保持 Client 当前值。
// Client 侧 SetStreamPolicy/HTTP.Timeout 自带并发安全（或经锁），
// 已在途请求不受影响——它们的看门狗建立时已取好策略副本。
func (h *Handler) applyRuntimeUpstreamTimeouts(p *upstreamPatch, updated map[string]any) {
	if p == nil {
		return
	}
	if h.cfg.SetUpstreamTimeouts == nil || h.cfg.Upstream == nil {
		return
	}
	// 读取 Client 当前值做基准：未显式给出的字段沿用现状而不是重置默认。
	cur := h.cfg.Upstream.RequestPolicy(true)     // 流式策略
	curReq := h.cfg.Upstream.RequestPolicy(false) // 非流式 = HTTP.Timeout
	total := int(cur.Total / time.Second)
	idle := int(cur.Idle / time.Second)
	reqSecs := int(curReq.Total / time.Second)
	if p.TimeoutSeconds != nil {
		reqSecs = *p.TimeoutSeconds
		updated["timeout_seconds"] = reqSecs
	}
	if p.StreamTimeoutSeconds != nil {
		total = *p.StreamTimeoutSeconds
		updated["stream_timeout_seconds"] = total
	}
	if p.StreamIdleSeconds != nil {
		idle = *p.StreamIdleSeconds
		updated["stream_idle_seconds"] = idle
	}
	h.cfg.SetUpstreamTimeouts(reqSecs, total, idle)
}

// applyRuntimeFeatures 把 WebUI 保存的特性开关/费率推到运行时状态：
// handler 本地副本（passthrough/responsesOff/creditPolicy）+ upstream Client
// （脱敏、Codex 改写）。返回实际生效的值供前端回显确认。
func (h *Handler) applyRuntimeFeatures(features *featuresPatch, billing *billingPatch) map[string]any {
	h.featuresMu.Lock()
	defer h.featuresMu.Unlock()
	updated := map[string]any{}
	if features != nil {
		if features.Passthrough != nil {
			h.passthrough = *features.Passthrough
			updated["passthrough"] = *features.Passthrough
		}
		if features.ResponsesAPI != nil {
			h.responsesOff = !*features.ResponsesAPI
			updated["responses_api"] = *features.ResponsesAPI
		}
		if features.SanitizeBlacklistFingerprints != nil {
			if h.sanitizeHooks.setSanitize != nil {
				h.sanitizeHooks.setSanitize(*features.SanitizeBlacklistFingerprints)
			}
			updated["sanitize_blacklist_fingerprints"] = *features.SanitizeBlacklistFingerprints
		}
		if features.CodexCompat != nil {
			if h.sanitizeHooks.setCodex != nil {
				h.sanitizeHooks.setCodex(*features.CodexCompat)
			}
			updated["codex_compat"] = *features.CodexCompat
		}
	}
	if billing != nil {
		if billing.InputCreditsPer1KTokens != nil {
			h.creditPolicy.InputPer1K = *billing.InputCreditsPer1KTokens
			updated["input_credits_per_1k_tokens"] = *billing.InputCreditsPer1KTokens
		}
		if billing.OutputCreditsPer1KTokens != nil {
			h.creditPolicy.OutputPer1K = *billing.OutputCreditsPer1KTokens
			updated["output_credits_per_1k_tokens"] = *billing.OutputCreditsPer1KTokens
		}
		if billing.CachedInputCreditsPer1KTokens != nil {
			h.creditPolicy.CachedInputPer1K = *billing.CachedInputCreditsPer1KTokens
			updated["cached_input_credits_per_1k_tokens"] = *billing.CachedInputCreditsPer1KTokens
		}
	}
	return updated
}

func normalizeScheduleHours(hours []int) ([]int, bool) {
	if len(hours) == 0 {
		return nil, false
	}
	seen := make(map[int]struct{}, len(hours))
	out := make([]int, 0, len(hours))
	for _, hour := range hours {
		if hour < 0 || hour > 23 {
			return nil, false
		}
		if _, exists := seen[hour]; exists {
			continue
		}
		seen[hour] = struct{}{}
		out = append(out, hour)
	}
	sort.Ints(out)
	return out, true
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	return writeFileAtomicWith(path, data, mode, os.Rename)
}

// writeFileAtomicWith replaces a regular file atomically. A single-file
// Docker bind mount cannot be replaced with rename from inside the container,
// so fall back to a durable in-place write when the replacement is rejected.
// The fallback keeps mounted config files writable on the host; directory
// mounted auth files continue to use the atomic path.
func writeFileAtomicWith(path string, data []byte, mode os.FileMode, rename func(string, string) error) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".wb2api-config-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := rename(tmpPath, path); err == nil {
		return nil
	} else {
		renameErr := err
		if err := writeFileInPlace(path, data, mode); err != nil {
			return fmt.Errorf("replace %s: %w; in-place fallback: %v", path, renameErr, err)
		}
		return nil
	}
}

func writeFileInPlace(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (h *Handler) runCheckin(w http.ResponseWriter, r *http.Request) {
	if h.cfg.CheckinNow == nil {
		writeJSON(w, 503, map[string]string{"error": "签到服务不可用"})
		return
	}
	go h.cfg.CheckinNow()
	writeJSON(w, 202, map[string]any{"ok": true, "message": "签到任务已启动"})
}

func (h *Handler) refreshCredits(w http.ResponseWriter, r *http.Request) {
	if h.cfg.CreditRefreshNow == nil {
		writeJSON(w, 503, map[string]string{"error": "积分刷新服务不可用"})
		return
	}
	go h.cfg.CreditRefreshNow()
	writeJSON(w, 202, map[string]any{"ok": true, "message": "上游积分刷新已启动"})
}

func normalizeLoginRegion(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "cn", "china":
		return "cn", nil
	case "global", "overseas", "international", "intl":
		return "global", nil
	default:
		return "", fmt.Errorf("登录区域只能选择 cn 或 global")
	}
}

func (h *Handler) loginRegion(r *http.Request) (string, error) {
	if raw := strings.TrimSpace(r.URL.Query().Get("region")); raw != "" {
		return normalizeLoginRegion(raw)
	}
	h.configMu.Lock()
	configured := strings.ToLower(strings.TrimSpace(h.cfg.Region))
	h.configMu.Unlock()
	if configured == "global" {
		return "global", nil
	}
	// all means the pool is mixed, but an OAuth request still needs one
	// concrete upstream host. Keep the historical CN default for API callers
	// that do not send the new query parameter.
	return "cn", nil
}

func (h *Handler) loginCommand(ctx context.Context, arg, region string) ([]byte, error) {
	bin := h.cfg.LoginBin
	if bin == "" {
		bin = "./login"
	}
	cmd := exec.CommandContext(ctx, bin, arg)
	cmd.Dir = filepath.Dir(h.cfg.ConfigPath)
	cmd.Env = setCommandEnv(os.Environ(), "WB2A_LOGIN_REGION", region)
	return cmd.Output()
}

func setCommandEnv(env []string, key, value string) []string {
	prefix := key + "="
	updated := false
	for i, item := range env {
		if strings.HasPrefix(item, prefix) {
			env[i] = prefix + value
			updated = true
		}
	}
	if !updated {
		env = append(env, prefix+value)
	}
	return env
}

func (h *Handler) accountURL(w http.ResponseWriter, r *http.Request) {
	region, err := h.loginRegion(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	out, err := h.loginCommand(r.Context(), "url", region)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": fmt.Sprintf("登录初始化失败: %v", err)})
		return
	}
	loginURL := strings.TrimSpace(string(out))
	if loginURL == "" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "登录初始化未返回授权链接"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": loginURL, "region": region})
}

func (h *Handler) accountPoll(w http.ResponseWriter, r *http.Request) {
	region, err := h.loginRegion(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	out, err := h.loginCommand(r.Context(), "poll", region)
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": "登录尚未完成，请先在浏览器完成授权"})
		return
	}
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Domain       string `json:"domain"`
		Region       string `json:"region"`
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterprise_id"`
		Nickname     string `json:"nickname"`
	}
	if json.Unmarshal(out, &result) != nil || result.AccessToken == "" || result.UID == "" {
		writeJSON(w, 409, map[string]string{"error": "登录尚未完成，请完成授权后重试"})
		return
	}
	if result.Region != "" {
		returnedRegion, regionErr := normalizeLoginRegion(result.Region)
		if regionErr != nil || returnedRegion != region {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "授权区域与请求区域不一致，请重新生成登录链接"})
			return
		}
	}
	if region == "global" && strings.TrimSpace(result.Domain) == "" {
		// Some international OAuth responses omit domain. Persisting the global
		// host is necessary because account.Region() drives every later request.
		result.Domain = "www.workbuddy.ai"
	}
	response, status, err := h.persistAccount(accountCredential{
		UID:          result.UID,
		Nickname:     result.Nickname,
		EnterpriseID: result.EnterpriseID,
		Domain:       result.Domain,
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresIn:    result.ExpiresIn,
	}, region)
	if err != nil {
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, status, response)
}

// accountCredential 是新增账号的落盘输入，OAuth 与短信直登共用。
type accountCredential struct {
	UID          string
	Nickname     string
	EnterpriseID string
	Domain       string
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
}

// persistAccount 把一份新凭证写入 auths/、热加载进账号池，并按需把配置提升为
// 混合区域。返回控制台响应体与 HTTP 状态码；err 非 nil 时 status 为其对应码。
//
// region 是本次登录所属区域，仅用于决定是否要把 config.region 提升为 all。
func (h *Handler) persistAccount(cred accountCredential, region string) (map[string]any, int, error) {
	if filepath.Base(cred.UID) != cred.UID || strings.ContainsAny(cred.UID, `/\`) {
		return nil, http.StatusBadRequest, errors.New("授权返回的 UID 无效")
	}
	if err := os.MkdirAll(h.cfg.AuthDir, 0700); err != nil {
		return nil, 500, err
	}
	expiresAt := int64(0)
	if cred.ExpiresIn > 0 {
		expiresAt = time.Now().Unix() + cred.ExpiresIn
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken": cred.AccessToken, "refreshToken": cred.RefreshToken,
			"expiresAt": expiresAt, "domain": cred.Domain,
		},
		"account": map[string]any{
			"uid": cred.UID, "enterpriseId": cred.EnterpriseID, "nickname": cred.Nickname,
		},
	}
	raw, _ := json.MarshalIndent(doc, "", "  ")
	path := filepath.Join(h.cfg.AuthDir, "workbuddy-"+cred.UID+".json")
	if err := writeFileAtomic(path, append(raw, '\n'), 0600); err != nil {
		return nil, 500, err
	}

	loadedMixed := false
	responseExisted := false
	existedDisabled := false
	if h.cfg.Pool != nil {
		// 重新登录已有账号时 upsert 只换凭证、保留 disabled/cooling 状态。
		// 若该账号此前被禁用，重登后依然不接流量，必须明确告知，否则用户
		// 以为登录成功却始终用不上。
		existed := h.cfg.Pool.AuthByUID(cred.UID) != nil
		if existed {
			responseExisted = true
			if st, ok := h.cfg.Pool.Status(cred.UID); ok && st.Disabled {
				existedDisabled = true
			}
		}
		if loaded, loadErr := auth.LoadDir(h.cfg.AuthDir, auth.RegionAll); loadErr == nil {
			regions := make(map[string]struct{}, 2)
			for _, loadedAuth := range loaded {
				regions[loadedAuth.Region()] = struct{}{}
			}
			loadedMixed = len(regions) > 1
			h.cfg.Pool.SyncToDir(loaded)
		} else {
			log.Printf("account add: reload auths failed: %v", loadErr)
		}
		h.cfg.Pool.Add(&auth.Auth{
			AccessToken: cred.AccessToken, RefreshToken: cred.RefreshToken, ExpiresAt: expiresAt,
			Domain: cred.Domain, UID: cred.UID, EnterpriseID: cred.EnterpriseID,
			Nickname: cred.Nickname, FilePath: path,
		})
	}
	// A newly added account may expose a different regional model catalogue.
	// Force the next /models request to fetch with the expanded pool.
	invalidateDynamicModelsCache()

	response := map[string]any{"ok": true, "uid": cred.UID, "nickname": cred.Nickname, "region": region}
	if responseExisted {
		// 让控制台区分"新增账号"与"刷新已有账号凭证"。
		response["existed"] = true
		if existedDisabled {
			// upsert 有意保留 disabled，重登不会自动启用；必须提示，
			// 否则用户会以为登录成功却始终用不上这个账号。
			response["disabled"] = true
			response["notice"] = "该账号已在号池中，凭证已更新；但它当前处于禁用状态，需要在账号列表中手动启用后才会接流量。"
		} else {
			response["notice"] = "该账号已在号池中，凭证已更新（积分与冷却状态保持不变）。"
		}
	}
	var configErr error
	if loadedMixed {
		configErr = h.promoteMixedRegion()
	} else {
		configErr = h.promoteMixedRegionIfNeeded(region)
	}
	if configErr != nil {
		// The account is already usable in the current process. Return a warning
		// so an unwritable config mount does not hide the restart persistence fix.
		response["warning"] = "账号已添加，但混合区域配置未能保存：" + configErr.Error()
	}
	return response, http.StatusOK, nil
}

// findAccountByMobile 在账号池里按手机号找已存在的中国区账号。
// 中国区账号的 nickname 就是运营商号码本身（无区号），所以两边都取国内号码再比。
// 找到时返回账号状态与 true；池为空或未匹配返回零值与 false。
func (h *Handler) findAccountByMobile(mobile string) (pool.Status, bool) {
	if h.cfg.Pool == nil {
		return pool.Status{}, false
	}
	want := smslogin.NationalNumber(mobile)
	if want == "" {
		return pool.Status{}, false
	}
	for _, st := range h.cfg.Pool.List() {
		if st.Region != "" && st.Region != "cn" {
			continue
		}
		if smslogin.NationalNumber(st.Nickname) == want {
			return st, true
		}
	}
	return pool.Status{}, false
}

// accountSMSSend 短信直登第一步：向指定手机号下发 OneID 短信验证码。
// 返回 session_id，验码时凭它取回本次登录的中间状态。
func (h *Handler) accountSMSSend(w http.ResponseWriter, r *http.Request) {
	if h.cfg.SMSLogin == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "短信登录未启用"})
		return
	}
	var req struct {
		Mobile string `json:"mobile"`
		Region string `json:"region"`
		// Force 表示用户已确认"该号码已在号池中，仍要重新登录"。
		Force bool `json:"force"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不正确"})
		return
	}
	region, err := normalizeLoginRegion(req.Region)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// 手机号已在号池中时先提示、不发短信：发一条短信是有成本的，而"已经有这个
	// 账号"通常意味着用户没必要重登。确认后带 force 再来，才真正走发码。
	// 注意必须在 Send 之前判断——发完码再提示等于白白消耗一条短信。
	if !req.Force {
		if st, ok := h.findAccountByMobile(req.Mobile); ok {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":    "该手机号已在号池中",
				"existing": true,
				"uid":      st.UID,
				"nickname": st.Nickname,
				"region":   st.Region,
				"disabled": st.Disabled,
			})
			return
		}
	}
	// 手机号与验证码属于敏感输入，失败原因要回给用户，但绝不写进日志。
	res, err := h.cfg.SMSLogin.Send(r.Context(), req.Mobile, region)
	if err != nil {
		writeJSON(w, smsLoginStatus(err), smsLoginErrorBody(err))
		return
	}
	writeJSON(w, http.StatusOK, smsSendBody(res))
}

// smsSendBody 统一发码回执。被要求人机校验时也要回 200：会话已经建立、
// 用户只需要接着在界面上过码，这不是错误。
func smsSendBody(res smslogin.SendResult) map[string]any {
	body := map[string]any{
		"ok": true, "session_id": res.SessionID, "mobile": res.Mobile,
		"status": res.Status, "expires_in": res.ExpiresIn, "region": res.Region,
	}
	if res.Captcha != nil {
		body["captcha"] = res.Captcha
	}
	return body
}

// accountSMSSubmitCaptcha 接收用户在界面上完成的人机校验结果，继续发码。
//
// 上游只认 captchaVerification 这个对象，不区分票据来自打码平台还是真人操作，
// 所以这里与自动过码共用同一条回灌路径。
func (h *Handler) accountSMSSubmitCaptcha(w http.ResponseWriter, r *http.Request) {
	if h.cfg.SMSLogin == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "短信登录未启用"})
		return
	}
	var req struct {
		SessionID string `json:"session_id"`
		Ticket    string `json:"ticket"`
		RandStr   string `json:"rand_str"`
		CloudType string `json:"cloud_type"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不正确"})
		return
	}
	res, err := h.cfg.SMSLogin.SubmitCaptcha(r.Context(), req.SessionID, req.Ticket, req.RandStr, req.CloudType)
	if err != nil {
		writeJSON(w, smsLoginStatus(err), smsLoginErrorBody(err))
		return
	}
	writeJSON(w, http.StatusOK, smsSendBody(res))
}

// accountSMSVerify 短信直登第二步：校验验证码，走完全部上游步骤并把账号落盘。
func (h *Handler) accountSMSVerify(w http.ResponseWriter, r *http.Request) {
	if h.cfg.SMSLogin == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "短信登录未启用"})
		return
	}
	var req struct {
		SessionID string `json:"session_id"`
		Code      string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不正确"})
		return
	}
	creds, err := h.cfg.SMSLogin.Verify(r.Context(), req.SessionID, req.Code)
	if err != nil {
		writeJSON(w, smsLoginStatus(err), smsLoginErrorBody(err))
		return
	}
	response, status, err := h.persistAccount(accountCredential{
		UID:          creds.UID,
		Nickname:     creds.Nickname,
		EnterpriseID: creds.EnterpriseID,
		Domain:       creds.Domain,
		AccessToken:  creds.AccessToken,
		RefreshToken: creds.RefreshToken,
		ExpiresIn:    creds.ExpiresIn,
	}, creds.Region)
	if err != nil {
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, status, response)
}

// accountSMSAutoEnroll 豪猪自动加号：后台循环"取号→发码→收码→验码落盘"。
// POST {"count": N, "workers": M} 启动；同一时刻只允许一个任务在跑。
// workers 并发数 1-8，默认 3（每个号一个独立代理出口，共享同一个号池）。
// accountSMSAutoEnrollStop 停止正在跑的自动加号任务。
//
// 没有这个端点时只能重启容器才能停下一个跑偏的任务（例如对接商全是空号，
// 循环一直在取号失败）。已在途的号会由各自的超时收尾，不会落半截账号。
func (h *Handler) accountSMSAutoEnrollStop(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AutoEnroll == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "自动加号未启用"})
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	// body 可选：没有 body 也能停。
	_ = json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req)

	if !h.cfg.AutoEnroll.Stop(req.Reason) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "note": "当前没有正在运行的任务"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": "已请求停止，正在收尾"})
}

// proxyStatus 导出登录代理池状态，供「代理池」页展示。
//
// 只报告配置与冷却情况，**绝不返回代理密码**：这些凭据对控制台没有
// 使用价值，但会留在浏览器历史/日志里。
func (h *Handler) proxyStatus(w http.ResponseWriter, r *http.Request) {
	if h.cfg.SMSLogin == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "reason": "短信直登未启用"})
		return
	}
	st := h.cfg.SMSLogin.ProxyStatus()
	if st == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false,
			"reason":  "未配置登录代理（直连模式）",
		})
		return
	}
	st["enabled"] = true
	writeJSON(w, http.StatusOK, st)
}

// accountSMSAutoEnroll 启动自动加号任务。
func (h *Handler) accountSMSAutoEnroll(w http.ResponseWriter, r *http.Request) {
	if h.cfg.SMSLogin == nil || h.cfg.AutoEnroll == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "自动加号未启用（缺少豪猪配置）"})
		return
	}
	var req struct {
		Count   int `json:"count"`
		Workers int `json:"workers"`
		// PollCount 每个号等验证码的轮询次数（不填用后端默认 18 次 = 90s）。
		PollCount int `json:"poll_count"`
		// MaxAttempts 总尝试次数上限（不填按目标数推导：count*12，下限 20）。
		MaxAttempts int `json:"max_attempts"`
		// Groups 新账号登记的分组（多选；不填/空 = default）。
		Groups []string `json:"groups"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不正确"})
		return
	}
	if req.Count < 1 || req.Count > 50 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "count 需在 1-50 之间"})
		return
	}
	// 分组在启动时就校验存在性：比等到加号成功才在登记回调里失败好——
	// 那时号码已消耗，登记失败只能事后手动补。
	if h.cfg.Groups != nil {
		known := map[string]bool{}
		for _, g := range h.cfg.Groups.List() {
			known[g] = true
		}
		for _, g := range req.Groups {
			if g = strings.TrimSpace(g); g != "" && !known[g] {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "分组 " + g + " 不存在"})
				return
			}
		}
	}
	if req.Workers < 0 || req.Workers > 8 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "workers 需在 1-8 之间（不填默认 3）"})
		return
	}
	// 0 = 不指定（用默认）。越界直接报错而不是静默夹取：用户以为调到了 100 次，
	// 实际只有 36 次，会一直困惑为什么还是收不到码。
	if req.PollCount < 0 || req.PollCount > maxPollCount {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("poll_count 需在 1-%d 之间（不填默认 %d 次，约 %d 秒）",
				maxPollCount, defaultPollCount, defaultPollCount*5),
		})
		return
	}
	if req.MaxAttempts < 0 || req.MaxAttempts > maxAttemptsLimit {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("max_attempts 需在 1-%d 之间（不填按目标数推导）", maxAttemptsLimit),
		})
		return
	}
	err := h.cfg.AutoEnroll.AutoRunWith(AutoRunOptions{
		Want:        req.Count,
		Workers:     req.Workers,
		PollCount:   req.PollCount,
		MaxAttempts: req.MaxAttempts,
		Groups:      req.Groups,
	})
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	// 回显**生效值**：前端据此显示"这次真的按几次轮询跑"。
	st := h.cfg.AutoEnroll.Status()
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok":           true,
		"count":        req.Count,
		"workers":      st.Workers,
		"poll_count":   st.PollCount,
		"max_attempts": st.MaxAttempts,
		"note":         "任务已在后台运行，用 GET /admin/account/sms/auto-enroll 查看进度",
	})
}

// accountSMSAutoEnrollStatus 查询自动加号进度。
func (h *Handler) accountSMSAutoEnrollStatus(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AutoEnroll == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "自动加号未启用"})
		return
	}
	st := h.cfg.AutoEnroll.Status()
	body := map[string]any{
		"running": st.Running, "attempts": st.Attempts,
		"ok": st.OK, "fail": st.Fail, "logs": st.Logs,
		"consumed": st.Consumed,
		// held > 0 表示还有号占着豪猪额度（会拖累后续取号），控制台要能看见。
		"held": st.Held, "released": st.Released,
	}
	if st.Workers > 0 {
		body["workers"] = st.Workers
	}
	// 本次生效的轮询次数 / 尝试上限：控制台据此显示，也让"填了但被夹住"可见。
	if st.PollCount > 0 {
		body["poll_count"] = st.PollCount
	}
	if st.MaxAttempts > 0 {
		body["max_attempts"] = st.MaxAttempts
	}
	// 终止原因（余额不足/熔断/无号可取）：控制台据此提示用户，别只显示 0 成功。
	if st.StopReason != "" {
		body["stop_reason"] = st.StopReason
	}
	// 余额：任务开始前能看出钱还够不够，失败也不用翻后台。
	if bal, err := h.cfg.AutoEnroll.Balance(r.Context()); err == nil && bal >= 0 {
		body["balance"] = bal
	}
	writeJSON(w, http.StatusOK, body)
}

// accountSMSReleaseAll 一键释放豪猪名下所有占用中的号码（cancelAllRecv）。
//
// 使用场景：额度被历史遗留号占满（「您的余额不足,请释放拉黑后再取号」）
// 但逐号补释放一直失败，或账本外的号（手动测试等）也占着额度。语义与
// 豪猪后台的"释放全部"按钮相同。任务运行中返回 409（在途号码会被一并
// 放掉，所有 worker 白等）。
func (h *Handler) accountSMSReleaseAll(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AutoEnroll == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "自动加号未启用"})
		return
	}
	done, heldBefore, err := h.cfg.AutoEnroll.ReleaseAllHeld(r.Context())
	if errors.Is(err, ErrBusy) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "豪猪释放失败: " + err.Error()})
		return
	}
	h.cfg.AutoEnroll.AppendLog("一键释放完成：豪猪名下占用号码已全部归还（含账本外 %d 个遗留号）", heldBefore)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "released": done, "ledger_cleared": heldBefore,
	})
}

// smsLoginStatus 把登录失败映射到 HTTP 状态码：会话失效用 410，其余上游/参数
// 问题用 502 或 400，便于控制台区分"重发验证码"与"直接报错"。
func smsLoginStatus(err error) int {
	switch {
	case errors.Is(err, smslogin.ErrSessionNotFound):
		return http.StatusGone
	case errors.Is(err, smslogin.ErrSessionBusy):
		return http.StatusConflict
	case errors.Is(err, smslogin.ErrGlobalUnsupported):
		return http.StatusBadRequest
	}
	var ue *smslogin.Error
	if errors.As(err, &ue) {
		switch ue.Step {
		case "发送验证码", "校验验证码", "读取账号", "人机校验":
			return http.StatusBadRequest
		}
		return http.StatusBadGateway
	}
	return http.StatusBadRequest
}

// smsLoginErrorBody 组装错误响应，并带上 retryable 让前端决定是保留当前会话
// （验证码填错，直接重填即可）还是清空状态要求重新发码。
func smsLoginErrorBody(err error) map[string]any {
	body := map[string]any{"error": err.Error(), "retryable": false}
	var ue *smslogin.Error
	if errors.As(err, &ue) && ue.Retryable {
		body["retryable"] = true
	}
	switch {
	case errors.Is(err, smslogin.ErrCodeWrong):
		body["reason"] = "code_wrong"
	case errors.Is(err, smslogin.ErrTooFrequent):
		body["reason"] = "too_frequent"
	case errors.Is(err, smslogin.ErrSessionNotFound):
		body["reason"] = "session_expired"
	case errors.Is(err, smslogin.ErrSessionBusy):
		body["reason"] = "busy"
	case errors.Is(err, smslogin.ErrCaptchaPending):
		body["reason"] = "captcha_pending"
	case errors.Is(err, smslogin.ErrCaptchaIncomplete):
		body["reason"] = "captcha_incomplete"
	}
	if ue != nil && ue.Step == "取回凭证" && ue.Retryable {
		body["reason"] = "ticket_pending"
	}
	return body
}

// promoteMixedRegionIfNeeded keeps both CN and global credentials available
// after a user adds an account from the other region. It persists region=all
// for the next restart and updates the running handler after a successful write.
func (h *Handler) promoteMixedRegionIfNeeded(loginRegion string) error {
	h.configMu.Lock()
	configured := strings.ToLower(strings.TrimSpace(h.cfg.Region))
	h.configMu.Unlock()
	if configured == "all" || configured == loginRegion {
		return nil
	}
	return h.promoteMixedRegion()
}

func (h *Handler) promoteMixedRegion() error {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	if h.cfg.ConfigPath == "" {
		h.cfg.Region = "all"
		return nil
	}
	raw, err := os.ReadFile(h.cfg.ConfigPath)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	doc["region"] = "all"
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(h.cfg.ConfigPath, append(out, '\n'), 0600); err != nil {
		// Keep the in-memory region unchanged when persistence fails. The caller
		// already added the account to the live pool, and a later account add can
		// retry this write instead of incorrectly considering it complete.
		return err
	}
	h.cfg.Region = "all"
	return nil
}

func (h *Handler) enableAccount(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "账号 UID 不能为空"})
		return
	}
	if h.cfg.Pool == nil || !h.cfg.Pool.Enable(uid) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号不存在"})
		return
	}
	h.cfg.Pool.Flush()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "message": "账号已启用"})
}

func (h *Handler) disableAccount(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "账号 UID 不能为空"})
		return
	}
	if h.cfg.Pool == nil || h.cfg.Pool.AuthByUID(uid) == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号不存在"})
		return
	}
	h.cfg.Pool.Disable(uid, "manual disabled")
	h.cfg.Pool.Flush()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "message": "账号已禁用"})
}

func (h *Handler) deleteAccount(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "账号 UID 不能为空"})
		return
	}
	if h.cfg.Pool == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号不存在"})
		return
	}
	account := h.cfg.Pool.AuthByUID(uid)
	if account == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号不存在"})
		return
	}
	if removed, busy := h.cfg.Pool.Remove(uid); !removed {
		if busy {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "账号仍有请求处理中，请稍后再删除"})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号不存在"})
		return
	}
	if err := h.removeAccountFile(uid, account); err != nil {
		// 文件删除失败时恢复内存中的账号，避免控制台显示删除成功但账号仍会在下次同步出现。
		h.cfg.Pool.Add(account)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("删除账号文件失败: %v", err)})
		return
	}
	h.cfg.Pool.Flush()
	// 分组归属一并清理（分组存储启用时）；失败不阻塞删除——残留的
	// 归属记录只影响分组计数展示，下次该 uid 重加时会被覆盖。
	if h.cfg.Groups != nil {
		h.cfg.Groups.RemoveAccount(uid)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "message": "账号已删除"})
}

func (h *Handler) clearCooldownAccount(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "账号 UID 不能为空"})
		return
	}
	if h.cfg.Pool == nil || !h.cfg.Pool.ClearCooldown(uid) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号不存在"})
		return
	}
	h.cfg.Pool.Flush()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "message": "冷却与熔断已清除"})
}

func (h *Handler) checkinAccount(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "账号 UID 不能为空"})
		return
	}
	if h.cfg.CheckinAccount == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "签到服务不可用"})
		return
	}
	res := h.cfg.CheckinAccount(uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": res.OK, "uid": res.UID, "detail": res.Detail})
}

func (h *Handler) keepaliveAccount(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "账号 UID 不能为空"})
		return
	}
	if h.cfg.KeepaliveAccount == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "保活服务不可用"})
		return
	}
	res := h.cfg.KeepaliveAccount(uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": res.OK, "uid": res.UID, "detail": res.Detail})
}

func (h *Handler) removeAccountFile(uid string, account *auth.Auth) error {
	snapshot := account.Snapshot()
	path := snapshot.FilePath
	if path == "" {
		if h.cfg.AuthDir == "" || filepath.Base(uid) != uid {
			return nil
		}
		path = filepath.Join(h.cfg.AuthDir, "workbuddy-"+uid+".json")
	}
	if h.cfg.AuthDir == "" {
		return fmt.Errorf("auth_dir 未配置")
	}
	root, err := filepath.Abs(h.cfg.AuthDir)
	if err != nil {
		return err
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("账号文件不在 auth_dir 内")
	}
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"healthy": healthy, "total": total})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	// 分组归属快照（分组存储启用时）：账号列表页一次拉齐，省得逐账号 GET。
	var accountGroups map[string][]string
	if h.cfg.Groups != nil {
		accountGroups = h.cfg.Groups.SnapshotAccountGroups()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":        h.cfg.Pool.List(),
		"account_groups":  accountGroups,
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"in_flight_full":  inFlightFull,
		"concurrency":     h.cfg.Pool.CapacityForModel(upstream.NormalizeModelID(r.URL.Query().Get("model"))),
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
		"metrics":         h.metricsSnapshot(),
	})
}

func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.metricsSnapshot())
}

func (h *Handler) requests(w http.ResponseWriter, r *http.Request) {
	// 在边界处夹紧 limit。存储层各自还有一道防御性夹紧，但依赖后端实现意味着
	// 换个后端就换一套行为；"limit=100000000" 这种客户端输入不该被放行。
	const defaultRequestsLimit, maxRequestsLimit = 50, 500
	limit := defaultRequestsLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil && value > 0 {
			limit = value
			if limit > maxRequestsLimit {
				limit = maxRequestsLimit
			}
		}
	}
	if h.cfg.RequestLogStore == nil {
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []RequestLog{}})
		return
	}
	// 控制台的请求日志页：带任何筛选参数时走数据库下推查询，支持
	// 时间窗/模型/状态/账号等多维过滤；老客户端（只传 limit）行为不变——
	// 无筛选时的 QueryRequests 与 RecentRequests 等价。
	if queryStore, ok := h.cfg.RequestLogStore.(RequestLogQueryStore); ok {
		filter, _ := parseRequestLogFilter(r, limit)
		filter.Limit = limit
		records, err := queryStore.QueryRequests(filter)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"code": "request_log_unavailable", "message": err.Error()}})
			return
		}
		if records == nil {
			records = []RequestLog{}
		}
		body := map[string]any{"object": "list", "data": records}
		// include=summary 时附带全量聚合（不受 limit 截断），仪表盘的
		// 时间窗统计靠它显示精确总数。
		if strings.Contains(r.URL.Query().Get("include"), "summary") {
			if summary, err := queryStore.SummarizeRequests(filter); err == nil {
				body["summary"] = summary
			}
		}
		writeJSON(w, http.StatusOK, body)
		return
	}
	records, err := h.cfg.RequestLogStore.RecentRequests(limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"code": "request_log_unavailable", "message": err.Error()}})
		return
	}
	if records == nil {
		records = []RequestLog{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": records})
}

// parseRequestLogFilter decodes console filter query parameters. ok=false
// means no filter parameter was supplied at all, letting the endpoint keep
// its legacy behaviour.
func parseRequestLogFilter(r *http.Request, limit int) (RequestLogFilter, bool) {
	query := r.URL.Query()
	filter := RequestLogFilter{Limit: limit}
	seen := false
	if raw := strings.TrimSpace(query.Get("since")); raw != "" {
		if value, err := strconv.ParseInt(raw, 10, 64); err == nil {
			filter.SinceUnix = value
			seen = true
		}
	}
	if raw := strings.TrimSpace(query.Get("until")); raw != "" {
		if value, err := strconv.ParseInt(raw, 10, 64); err == nil {
			filter.UntilUnix = value
			seen = true
		}
	}
	for param, target := range map[string]*string{
		"model":   &filter.Model,
		"route":   &filter.Route,
		"account": &filter.AccountUID,
		"region":  &filter.Region,
		"code":    &filter.ErrorCode,
		"error":   &filter.ErrorCode,
		"id":      &filter.ID,
	} {
		if raw := strings.TrimSpace(query.Get(param)); raw != "" {
			*target = raw
			seen = true
		}
	}
	if raw := strings.TrimSpace(query.Get("status")); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil {
			filter.Status = value
			seen = true
		}
	}
	if raw := strings.TrimSpace(query.Get("success")); raw != "" {
		value := false
		if raw == "1" || strings.EqualFold(raw, "true") {
			value = true
		} else if raw != "0" && !strings.EqualFold(raw, "false") {
			return filter, seen
		}
		filter.Success = &value
		seen = true
	}
	if raw := strings.TrimSpace(query.Get("ttfb_min")); raw != "" {
		if value, err := strconv.ParseInt(raw, 10, 64); err == nil && value > 0 {
			filter.TTFBMinMillis = value
			seen = true
		}
	}
	if raw := strings.TrimSpace(query.Get("ttfb_max")); raw != "" {
		if value, err := strconv.ParseInt(raw, 10, 64); err == nil && value > 0 {
			filter.TTFBMaxMillis = value
			seen = true
		}
	}
	return filter, seen
}

func (h *Handler) metricsSnapshot() map[string]any {
	if h.cfg.MetricsStore != nil {
		return h.cfg.MetricsStore.SnapshotMetrics()
	}
	return metricsSnapshot()
}

// 静态 WorkBuddy 模型表（动态接口失败时的回退；两种区域共用兼容别名）。
var staticModels = []map[string]any{
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5.1", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5v-turbo", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "kimi-k3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "kimi-k2.7-code", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "minimax-m3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3-preview", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3-preview-agent", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "deepseek-v4-pro", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "deepseek-v4-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
}

// dynamicModelsCache 动态模型缓存。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

func invalidateDynamicModelsCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
}

// models 返回模型列表：优先动态（缓存 1h），失败回退静态表。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// modelList 动态获取模型列表并包装成 OpenAI 格式（含 context_length）。
func (h *Handler) modelList() []map[string]any {
	if infos := h.fetchDynamicModels(); len(infos) > 0 {
		out := make([]map[string]any, 0, len(infos))
		for _, mi := range infos {
			entry := map[string]any{
				"id":                mi.ID,
				"object":            "model",
				"created":           1753600000,
				"owned_by":          "workbuddy",
				"context_length":    mi.ContextWindow,
				"max_output_tokens": mi.MaxTokens,
			}
			if mi.ContextWindow == 0 {
				entry["context_length"] = 131072 // 兜底
			}
			out = append(out, entry)
		}
		return out
	}
	return staticModels
}

// fetchDynamicModels 从池中任一健康账号拉模型列表（含 contextWindow/maxTokens），缓存 1h。
// 拉取失败记录时间戳进入 5min 负缓存，冷却期内直接用静态表，避免反复打上游。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	acct := h.cfg.Pool.Pick()
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// 拉取失败惩罚该账号，避免下次 Pick 又选中同一个反复失败；lastFail 保持全局负缓存。
		h.cfg.Pool.NoteError(acct.UID)
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos
}

// responses adapts the OpenAI Responses API to the existing Chat Completions
// execution path, preserving account rotation, retries, sticky sessions and
// upstream error handling in one place.
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	// responsesAPIEnabled：关闭时 Responses 端点整体 404（含会话历史
	// 存取路径）。WebUI 特性开关可运行时切换。
	if !h.responsesAPIEnabled() {
		writeOpenAIError(w, http.StatusNotFound, "endpoint_disabled", "Responses API is disabled on this deployment (features.responses_api=false)")
		return
	}
	body, tooLarge, err := readRequestBody(r)
	if tooLarge {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 8 MiB limit")
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	chatBody, stream, err := responsesToChat(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var requestDoc map[string]any
	var chatDoc map[string]any
	// nil 检查与 chat 路径同理：`null` 会解码成功但留下 nil map，
	// 后续 map 写入会 panic（"assignment to entry in nil map"）。
	if json.Unmarshal(body, &requestDoc) != nil || requestDoc == nil ||
		json.Unmarshal(chatBody, &chatDoc) != nil || chatDoc == nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	currentMessages, _ := chatDoc["messages"].([]any)
	turnMessages := responseMessages(currentMessages)
	messages := turnMessages
	routeKey := session.ExtractKey(body)
	previousID, _ := requestDoc["previous_response_id"].(string)
	if previousID != "" {
		previousMessages, previousRouteKey, ok := h.loadResponseHistory(previousID)
		if !ok {
			writeOpenAIError(w, http.StatusBadRequest, "previous_response_not_found", "previous_response_id is unknown or expired")
			return
		}
		// Top-level Responses instructions apply to the current request. Keep
		// them before the restored transcript and never insert them between a
		// prior assistant tool call and its function_call_output.
		messages = append(responseInstructionMessages(messages), stripInstructionMessages(previousMessages)...)
		messages = append(messages, responseInputMessages(turnMessages)...)
		routeKey = previousRouteKey
	}
	fallbackResponseID := newResponseID()
	if routeKey == "" {
		routeKey = "responses:" + fallbackResponseID
	}
	chatDoc["messages"] = messages
	meta, _ := chatDoc["metadata"].(map[string]any)
	if meta == nil {
		meta = make(map[string]any)
		chatDoc["metadata"] = meta
	}
	meta["conversation_id"] = routeKey
	chatBody, err = json.Marshal(chatDoc)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	chatReq := r.Clone(r.Context())
	chatReq.Body = io.NopCloser(bytes.NewReader(chatBody))
	if !stream {
		rec := httptest.NewRecorder()
		h.chatCompletions(rec, chatReq)
		if rec.Code < 200 || rec.Code >= 300 {
			copyResponse(w, rec)
			return
		}
		var chat map[string]any
		if json.Unmarshal(rec.Body.Bytes(), &chat) != nil {
			copyResponse(w, rec)
			return
		}
		responseID := upstream.ResponseID(chat)
		if responseID == "" {
			responseID = fallbackResponseID
		}
		upstream.CopyResponseIDHeaders(w.Header(), rec.Header())
		if w.Header().Get("X-Request-Id") == "" {
			w.Header().Set("X-Request-Id", responseID)
		}
		response := chatToResponse(chat, responseID)
		if assistant := chatAssistantMessage(chat, responseID); assistant != nil {
			h.storeResponse(responseID, append(responseInputMessages(turnMessages), assistant), routeKey, previousID)
		}
		writeJSON(w, http.StatusOK, response)
		return
	}
	sw := &responsesStreamWriter{header: make(http.Header), dst: w, fallbackID: fallbackResponseID}
	sw.onComplete = func(responseID string, assistant map[string]any) {
		h.storeResponse(responseID, append(responseInputMessages(turnMessages), assistant), routeKey, previousID)
	}
	h.chatCompletions(sw, chatReq)
}

func responseInstructionMessages(messages []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, 1)
	for _, message := range messages {
		if role, _ := message["role"].(string); role == "system" || role == "developer" {
			out = append(out, message)
		}
	}
	return out
}

func responseInputMessages(messages []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		if role, _ := message["role"].(string); role != "system" && role != "developer" {
			out = append(out, message)
		}
	}
	return out
}

func stripInstructionMessages(messages []map[string]any) []map[string]any {
	return responseInputMessages(messages)
}

func responseMessages(raw []any) []map[string]any {
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if message, ok := item.(map[string]any); ok {
			out = append(out, message)
		}
	}
	return out
}

func newResponseID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err == nil {
		return "resp_" + hex.EncodeToString(b)
	}
	return fmt.Sprintf("resp_%d", time.Now().UnixNano())
}

func (h *Handler) loadResponseHistory(id string) ([]map[string]any, string, bool) {
	seen := make(map[string]struct{})
	var routeKey string
	var turns [][]map[string]any
	for id != "" {
		if _, duplicate := seen[id]; duplicate {
			return nil, "", false
		}
		seen[id] = struct{}{}
		record, ok := h.loadResponseRecord(id)
		if !ok {
			return nil, "", false
		}
		if routeKey == "" {
			routeKey = record.routeKey
		}
		turns = append(turns, record.messages)
		id = record.parentID
	}
	history := make([]map[string]any, 0)
	for i := len(turns) - 1; i >= 0; i-- {
		history = append(history, turns[i]...)
	}
	return history, routeKey, true
}

func (h *Handler) loadResponseRecord(id string) (storedResponse, bool) {
	now := time.Now()
	h.responsesMu.Lock()
	record, ok := h.responseHistory[id]
	if ok && now.After(record.expiresAt) {
		h.deleteResponseLocked(id)
		ok = false
	}
	h.responsesMu.Unlock()
	if ok {
		return record, true
	}
	if h.cfg.ResponseStore == nil {
		return storedResponse{}, false
	}
	raw, ok := h.cfg.ResponseStore.LoadResponse(id)
	if !ok {
		return storedResponse{}, false
	}
	var persisted storedResponseWire
	if json.Unmarshal(raw, &persisted) != nil || len(persisted.Messages) == 0 || persisted.RouteKey == "" {
		return storedResponse{}, false
	}
	record = storedResponse{
		messages: persisted.Messages, parentID: persisted.ParentID, routeKey: persisted.RouteKey,
		expiresAt: now.Add(responseHistoryTTL), size: len(raw),
	}
	h.cacheResponse(id, record)
	return record, true
}

func (h *Handler) storeResponse(id string, messages []map[string]any, routeKey, parentID string) {
	now := time.Now()
	raw, _ := json.Marshal(messages)
	h.cacheResponse(id, storedResponse{
		messages: append([]map[string]any{}, messages...), parentID: parentID, routeKey: routeKey,
		expiresAt: now.Add(responseHistoryTTL), size: len(raw),
	})
	if h.cfg.ResponseStore != nil {
		persisted, err := json.Marshal(storedResponseWire{Messages: messages, ParentID: parentID, RouteKey: routeKey})
		if err == nil {
			h.cfg.ResponseStore.SaveResponse(id, persisted, responseHistoryTTL)
		}
	}
}

func (h *Handler) cacheResponse(id string, record storedResponse) {
	now := time.Now()
	h.responsesMu.Lock()
	defer h.responsesMu.Unlock()
	if _, exists := h.responseHistory[id]; exists {
		h.deleteResponseLocked(id)
	}
	for key, record := range h.responseHistory {
		if now.After(record.expiresAt) {
			h.deleteResponseLocked(key)
		}
	}
	for len(h.responseHistory) >= maxResponseHistory || h.responseBytes+record.size > maxResponseHistoryBytes {
		var oldestID string
		var oldest time.Time
		for key, record := range h.responseHistory {
			if oldestID == "" || record.expiresAt.Before(oldest) {
				oldestID, oldest = key, record.expiresAt
			}
		}
		if oldestID == "" {
			break
		}
		h.deleteResponseLocked(oldestID)
	}
	if record.size > maxResponseHistoryBytes {
		return
	}
	h.responseHistory[id] = record
	h.responseBytes += record.size
}

func (h *Handler) deleteResponseLocked(id string) {
	if record, ok := h.responseHistory[id]; ok {
		h.responseBytes -= record.size
		delete(h.responseHistory, id)
	}
}

func responsesToChat(raw []byte) ([]byte, bool, error) {
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, false, err
	}
	chat := make(map[string]any, len(in))
	for k, v := range in {
		switch k {
		case "input", "instructions", "stream", "max_output_tokens", "conversation", "previous_response_id":
			continue
		case "include", "store":
			// Responses 专属协议字段：include（reasoning.encrypted_content 等）
			// 与 store 只对原生 Responses 后端有意义，透传进 chat body 属于
			// 非预期负载。prompt_cache_key/conversation_id 保留——它们驱动本地
			// 粘性路由（session.ExtractKey 在进入上游前已完成提取，上游 payload
			// 层负责剥离）。
			continue
		}
		chat[k] = v
	}
	delete(chat, "reasoning")
	delete(chat, "text")
	if reasoning, ok := in["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && strings.TrimSpace(effort) != "" {
			chat["reasoning_effort"] = effort
		}
	}
	if text, ok := in["text"].(map[string]any); ok {
		if format, ok := text["format"].(map[string]any); ok {
			if responseFormat := responsesFormatToChat(format); responseFormat != nil {
				chat["response_format"] = responseFormat
			}
		}
	}
	// Responses tools are flat ({type,name,parameters}); Chat Completions
	// expects function metadata nested under `function`.
	if rawTools, ok := in["tools"].([]any); ok {
		tools := make([]any, 0, len(rawTools))
		for _, raw := range rawTools {
			tool, ok := raw.(map[string]any)
			if !ok || tool["type"] != "function" {
				tools = append(tools, raw)
				continue
			}
			if _, nested := tool["function"].(map[string]any); nested {
				tools = append(tools, tool)
				continue
			}
			fn := make(map[string]any, len(tool))
			for k, v := range tool {
				if k != "type" {
					fn[k] = v
				}
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		chat["tools"] = tools
	}
	if model, ok := in["model"].(string); !ok || strings.TrimSpace(model) == "" {
		return nil, false, fmt.Errorf("model is required")
	}
	msgs := make([]map[string]any, 0, 4)
	if s, ok := in["instructions"].(string); ok && strings.TrimSpace(s) != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": s})
	}
	if input, ok := in["input"]; ok {
		appendResponseInput(&msgs, input)
	}
	if len(msgs) == 0 {
		return nil, false, fmt.Errorf("input is required")
	}
	chat["messages"] = msgs
	// Carry a stable Responses conversation/cache key into the existing chat
	// routing path without sending Responses-only conversation fields upstream.
	if key := session.ExtractKey(raw); key != "" {
		meta, _ := chat["metadata"].(map[string]any)
		if meta == nil {
			meta = make(map[string]any)
			chat["metadata"] = meta
		}
		if _, exists := meta["conversation_id"]; !exists {
			meta["conversation_id"] = key
		}
	}
	stream, _ := in["stream"].(bool)
	chat["stream"] = stream
	if n, ok := in["max_output_tokens"]; ok {
		chat["max_tokens"] = n
	}
	out, err := json.Marshal(chat)
	return out, stream, err
}

func appendResponseInput(msgs *[]map[string]any, input any) {
	appendOne := func(role string, content any) {
		if content != nil && content != "" {
			*msgs = append(*msgs, map[string]any{"role": role, "content": content})
		}
	}
	switch v := input.(type) {
	case string:
		appendOne("user", v)
	case []any:
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				if s, ok := item.(string); ok {
					appendOne("user", s)
				}
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "function_call":
				callID, _ := m["call_id"].(string)
				name, _ := m["name"].(string)
				args, _ := m["arguments"].(string)
				if callID != "" && name != "" {
					call := map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": args}}
					if n := len(*msgs); n > 0 && (*msgs)[n-1]["role"] == "assistant" {
						last := (*msgs)[n-1]
						calls, _ := last["tool_calls"].([]any)
						last["tool_calls"] = append(calls, call)
					} else {
						*msgs = append(*msgs, map[string]any{"role": "assistant", "content": "", "tool_calls": []any{call}})
					}
				}
				continue
			case "function_call_output":
				callID, _ := m["call_id"].(string)
				var output any = m["output"]
				if parts, ok := output.([]any); ok {
					output = responseContentToChat(parts)
				}
				if output == nil {
					output = ""
				}
				if callID != "" {
					*msgs = append(*msgs, map[string]any{"role": "tool", "tool_call_id": callID, "content": output})
				} else if output != "" {
					// 无 call_id 的输出无法配对到任何 tool_call；静默丢弃会让前一条
					// assistant 的 tool_calls 变成孤立序列，触发上游 11148。降级为
					// user 消息（对齐参考实现），信息不丢、序列完整。
					*msgs = append(*msgs, map[string]any{"role": "user", "content": output})
				}
				continue
			}
			role, _ := m["role"].(string)
			if role == "" {
				role = "user"
			}
			if content, ok := m["content"].(string); ok {
				appendOne(role, content)
				continue
			}
			if content, ok := m["content"].([]any); ok {
				appendOne(role, responseContentToChat(content))
				continue
			}
			if text, ok := m["text"].(string); ok {
				appendOne(role, text)
			}
		}
	case map[string]any:
		role, _ := v["role"].(string)
		if role == "" {
			role = "user"
		}
		text, _ := v["content"].(string)
		appendOne(role, text)
	}
}

func responseContentToChat(content []any) []any {
	out := make([]any, 0, len(content))
	for _, raw := range content {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := part["type"].(string)
		switch typ {
		case "input_text", "output_text", "text":
			if text, _ := part["text"].(string); text != "" {
				out = append(out, map[string]any{"type": "text", "text": text})
			}
		case "input_image", "image_url":
			if imageURL, ok := part["image_url"].(string); ok && imageURL != "" {
				image := map[string]any{"url": imageURL}
				if detail, _ := part["detail"].(string); detail != "" {
					image["detail"] = detail
				}
				out = append(out, map[string]any{"type": "image_url", "image_url": image})
			} else if imageURL, ok := part["image_url"].(map[string]any); ok {
				out = append(out, map[string]any{"type": "image_url", "image_url": imageURL})
			} else if fileID, _ := part["file_id"].(string); fileID != "" {
				// Some compatible providers accept uploaded image IDs in the
				// image_url object even though the field name is historical.
				out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"file_id": fileID}})
			} else {
				// Preserve malformed image parts so the chat-path validator can
				// return a deterministic invalid_image error instead of silently
				// dropping the content.
				out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{}})
			}
		case "input_audio", "audio":
			audio, _ := part["input_audio"].(map[string]any)
			if audio == nil {
				audio, _ = part["audio"].(map[string]any)
			}
			if audio == nil {
				audio = selectFields(part, "data", "format")
			}
			if len(audio) > 0 {
				out = append(out, map[string]any{"type": "input_audio", "input_audio": audio})
			}
		case "input_file", "file":
			file, _ := part["file"].(map[string]any)
			if file == nil {
				file = selectFields(part, "file_id", "file_data", "filename")
			}
			if len(file) > 0 {
				out = append(out, map[string]any{"type": "file", "file": file})
			}
		}
	}
	return out
}

func selectFields(source map[string]any, keys ...string) map[string]any {
	out := make(map[string]any, len(keys))
	for _, key := range keys {
		if value, ok := source[key]; ok && value != nil && value != "" {
			out[key] = value
		}
	}
	return out
}

func responsesFormatToChat(format map[string]any) map[string]any {
	typ, _ := format["type"].(string)
	switch typ {
	case "text":
		return map[string]any{"type": "text"}
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		name, _ := format["name"].(string)
		schema := format["schema"]
		if name == "" || schema == nil {
			return nil
		}
		jsonSchema := map[string]any{"name": name, "schema": schema}
		if strict, ok := format["strict"].(bool); ok {
			jsonSchema["strict"] = strict
		}
		return map[string]any{"type": "json_schema", "json_schema": jsonSchema}
	default:
		return nil
	}
}

func chatToResponse(chat map[string]any, id string) map[string]any {
	if id == "" {
		id = newResponseID()
	}
	model, _ := chat["model"].(string)
	text := ""
	finishReason := ""
	var message map[string]any
	if choices, ok := chat["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			message, _ = choice["message"].(map[string]any)
			text, _ = message["content"].(string)
			finishReason, _ = choice["finish_reason"].(string)
		}
	}

	calls := normalizedAssistantToolCalls(message, id)
	output := make([]any, 0, len(calls)+1)
	if text != "" || len(calls) == 0 {
		output = append(output, map[string]any{
			"id": id + "-item", "type": "message", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
		})
	}
	for i, raw := range calls {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := call["function"].(map[string]any)
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		callID, _ := call["id"].(string)
		if callID == "" {
			callID = fmt.Sprintf("call-%s-%d", id, i)
		}
		output = append(output, map[string]any{
			"id": callID, "type": "function_call", "status": "completed", "call_id": callID,
			"name": name, "arguments": args,
		})
	}

	status := "completed"
	out := map[string]any{
		"id": id, "object": "response", "created_at": time.Now().Unix(), "status": status,
		"model": model, "output": output, "output_text": text,
	}
	if finishReason == "length" {
		out["status"] = "incomplete"
		out["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if usage, ok := chat["usage"]; ok {
		out["usage"] = responseUsage(usage)
	}
	return out
}

func chatAssistantMessage(chat map[string]any, responseID string) map[string]any {
	choices, ok := chat["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return nil
	}
	message, ok := choice["message"].(map[string]any)
	if !ok {
		return nil
	}
	assistant := map[string]any{"role": "assistant"}
	if content, exists := message["content"]; exists {
		assistant["content"] = content
	}
	if calls := normalizedAssistantToolCalls(message, responseID); len(calls) > 0 {
		assistant["tool_calls"] = calls
	}
	if _, hasContent := assistant["content"]; !hasContent {
		if _, hasCalls := assistant["tool_calls"]; !hasCalls {
			return nil
		}
	}
	return assistant
}

func normalizedAssistantToolCalls(message map[string]any, responseID string) []any {
	raw, _ := message["tool_calls"].([]any)
	out := make([]any, 0, len(raw))
	for i, value := range raw {
		call, ok := value.(map[string]any)
		if !ok {
			continue
		}
		copy := make(map[string]any, len(call)+1)
		for k, v := range call {
			copy[k] = v
		}
		if id, _ := copy["id"].(string); id == "" {
			copy["id"] = fmt.Sprintf("call-%s-%d", responseID, i)
		}
		out = append(out, copy)
	}
	return out
}

// responseUsage normalizes the upstream Chat Completions usage shape into the
// field names used by the Responses API while retaining provider-specific data.
func responseUsage(raw any) any {
	u, ok := raw.(map[string]any)
	if !ok {
		return raw
	}
	out := make(map[string]any, len(u)+2)
	for k, v := range u {
		out[k] = v
	}
	copyNumber := func(dst string, keys ...string) {
		if _, exists := out[dst]; exists {
			return
		}
		for _, key := range keys {
			if v, exists := u[key]; exists {
				out[dst] = v
				return
			}
		}
	}
	copyNumber("input_tokens", "prompt_tokens")
	copyNumber("output_tokens", "completion_tokens")
	if _, exists := out["input_tokens_details"]; !exists {
		cached := 0
		for _, key := range []string{"prompt_cache_hit_tokens", "cache_read_input_tokens"} {
			if n, ok := numberInt(u[key]); ok {
				cached = maxInts(cached, n)
			}
		}
		if details, ok := u["prompt_tokens_details"].(map[string]any); ok {
			if n, ok := numberInt(details["cached_tokens"]); ok {
				cached = maxInts(cached, n)
			}
		}
		if cached > 0 {
			out["input_tokens_details"] = map[string]any{"cached_tokens": cached}
		}
	}
	return out
}

func numberInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case float32:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	default:
		return 0, false
	}
}

type responseCapture interface {
	Header() http.Header
	WriteHeader(int)
	Write([]byte) (int, error)
}

func copyResponse(dst http.ResponseWriter, src *httptest.ResponseRecorder) {
	for k, vv := range src.Header() {
		for _, v := range vv {
			dst.Header().Add(k, v)
		}
	}
	dst.WriteHeader(src.Code)
	_, _ = dst.Write(src.Body.Bytes())
}

type responsesStreamWriter struct {
	header      http.Header
	dst         http.ResponseWriter
	buf         bytes.Buffer
	started     bool
	passthrough bool
	completed   bool
	id          string
	fallbackID  string
	model       string
	finish      string
	outputText  strings.Builder
	textStarted bool
	textIndex   int
	nextIndex   int
	usage       map[string]any
	calls       map[int]*responseStreamCall
	onComplete  func(string, map[string]any)
}

type responseStreamCall struct {
	index       int
	id          string
	name        string
	args        strings.Builder
	added       bool
	emittedArgs int
}

func (w *responsesStreamWriter) Header() http.Header { return w.header }
func (w *responsesStreamWriter) WriteHeader(code int) {
	if code >= 400 {
		for k, values := range w.header {
			for _, value := range values {
				w.dst.Header().Add(k, value)
			}
		}
		w.passthrough = true
		w.dst.WriteHeader(code)
	}
}
func (w *responsesStreamWriter) Write(p []byte) (int, error) {
	if w.passthrough {
		return w.dst.Write(p)
	}
	if w.completed {
		return len(p), nil
	}
	w.buf.Write(p)
	for {
		data := w.buf.Bytes()
		i := bytes.Index(data, []byte("\n\n"))
		if i < 0 {
			break
		}
		frame := string(data[:i])
		w.buf.Next(i + 2)
		for _, line := range strings.Split(frame, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			if payload == "[DONE]" {
				if err := w.complete(); err != nil {
					return 0, err
				}
				continue
			}
			var chunk map[string]any
			if json.Unmarshal([]byte(payload), &chunk) != nil {
				continue
			}
			if rawErr, ok := chunk["error"]; ok {
				w.ensureID(chunk)
				w.completed = true
				if err := w.emit("response.failed", map[string]any{"type": "response.failed", "response": map[string]any{"id": w.id, "object": "response", "status": "failed", "error": rawErr}}); err != nil {
					return 0, err
				}
				if _, err := io.WriteString(w.dst, "data: [DONE]\n\n"); err != nil {
					return 0, err
				}
				if f, ok := w.dst.(http.Flusher); ok {
					f.Flush()
				}
				return len(p), nil
			}
			if err := w.ensureCreated(chunk); err != nil {
				return 0, err
			}
			if usage, ok := chunk["usage"].(map[string]any); ok {
				w.usage = usage
			}
			if choices, ok := chunk["choices"].([]any); ok && len(choices) > 0 {
				if c, ok := choices[0].(map[string]any); ok {
					if finish, ok := c["finish_reason"].(string); ok && finish != "" {
						w.finish = finish
					}
					if d, ok := c["delta"].(map[string]any); ok {
						if text, ok := d["content"].(string); ok && text != "" {
							if err := w.startTextOutput(); err != nil {
								return 0, err
							}
							w.outputText.WriteString(text)
							if err := w.emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "response_id": w.id, "item_id": w.id + "-item", "output_index": w.textIndex, "content_index": 0, "delta": text}); err != nil {
								return 0, err
							}
						}
						if calls, ok := d["tool_calls"].([]any); ok {
							for _, raw := range calls {
								if call, ok := raw.(map[string]any); ok {
									idx := 0
									if n, ok := numberInt(call["index"]); ok {
										idx = n
									}
									if w.calls == nil {
										w.calls = make(map[int]*responseStreamCall)
									}
									state := w.calls[idx]
									if state == nil {
										state = &responseStreamCall{index: w.nextIndex}
										w.nextIndex++
										w.calls[idx] = state
									}
									fn, _ := call["function"].(map[string]any)
									if callID, _ := call["id"].(string); callID != "" {
										state.id = callID
									}
									name, _ := fn["name"].(string)
									if name != "" {
										state.name = name
									}
									args, _ := fn["arguments"].(string)
									if state.id == "" {
										state.id = fmt.Sprintf("call-%s-%d", w.id, idx)
									}
									if state.name != "" && !state.added {
										state.added = true
										if err := w.emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "response_id": w.id, "output_index": state.index, "item": map[string]any{"id": state.id, "type": "function_call", "status": "in_progress", "call_id": state.id, "name": state.name, "arguments": ""}}); err != nil {
											return 0, err
										}
									}
									if args != "" {
										state.args.WriteString(args)
									}
									if state.added && state.emittedArgs < state.args.Len() {
										allArgs := state.args.String()
										pending := allArgs[state.emittedArgs:]
										state.emittedArgs = len(allArgs)
										if err := w.emit("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "response_id": w.id, "output_index": state.index, "item_id": state.id, "call_id": state.id, "delta": pending}); err != nil {
											return 0, err
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	return len(p), nil
}

func (w *responsesStreamWriter) ensureID(chunk map[string]any) {
	if w.id == "" {
		w.id = upstream.ResponseID(chunk)
	}
	if w.id == "" {
		w.id = upstream.ResponseIDFromHeader(w.header)
	}
	if w.id == "" {
		w.id = w.fallbackID
	}
	if w.id == "" {
		w.id = newResponseID()
	}
	if w.dst.Header().Get("X-Request-Id") == "" {
		w.dst.Header().Set("X-Request-Id", w.id)
	}
	if w.model == "" && chunk != nil {
		w.model, _ = chunk["model"].(string)
	}
}

func (w *responsesStreamWriter) ensureCreated(chunk map[string]any) error {
	w.ensureID(chunk)
	if w.started {
		return nil
	}
	return w.emit("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": w.id, "object": "response", "status": "in_progress", "model": w.model}})
}

func (w *responsesStreamWriter) complete() error {
	if w.completed {
		return nil
	}
	w.completed = true
	w.ensureID(nil)
	text := w.outputText.String()
	output := make([]any, w.nextIndex)
	if w.textStarted {
		output[w.textIndex] = map[string]any{"id": w.id + "-item", "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
	}
	indexes := make([]int, 0, len(w.calls))
	for index := range w.calls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		call := w.calls[index]
		item := map[string]any{"id": call.id, "type": "function_call", "status": "completed", "call_id": call.id, "name": call.name, "arguments": call.args.String()}
		output[call.index] = item
		if err := w.emit("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "response_id": w.id, "item_id": call.id, "output_index": call.index, "arguments": call.args.String()}); err != nil {
			return err
		}
		if err := w.emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "response_id": w.id, "output_index": call.index, "item": item}); err != nil {
			return err
		}
	}
	status := "completed"
	response := map[string]any{"id": w.id, "object": "response", "status": status, "model": w.model, "output": output, "output_text": text}
	if w.finish == "length" {
		status = "incomplete"
		response["status"] = status
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if w.usage != nil {
		response["usage"] = responseUsage(w.usage)
	}
	if w.onComplete != nil {
		assistant := map[string]any{"role": "assistant", "content": text}
		if len(indexes) > 0 {
			toolCalls := make([]any, 0, len(indexes))
			for _, index := range indexes {
				call := w.calls[index]
				toolCalls = append(toolCalls, map[string]any{"id": call.id, "type": "function", "function": map[string]any{"name": call.name, "arguments": call.args.String()}})
			}
			assistant["tool_calls"] = toolCalls
		}
		w.onComplete(w.id, assistant)
	}
	if w.textStarted {
		if err := w.emit("response.output_text.done", map[string]any{"type": "response.output_text.done", "response_id": w.id, "item_id": w.id + "-item", "output_index": w.textIndex, "content_index": 0, "text": text}); err != nil {
			return err
		}
		if err := w.emit("response.content_part.done", map[string]any{"type": "response.content_part.done", "response_id": w.id, "item_id": w.id + "-item", "output_index": w.textIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}); err != nil {
			return err
		}
		if err := w.emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "response_id": w.id, "output_index": w.textIndex, "item": map[string]any{"id": w.id + "-item", "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}}); err != nil {
			return err
		}
	}
	event := "response.completed"
	if status == "incomplete" {
		event = "response.incomplete"
	}
	if err := w.emit(event, map[string]any{"type": event, "response": response}); err != nil {
		return err
	}
	// A number of OpenAI-compatible clients, including DSH, require the
	// terminal Chat Completions sentinel even when the payload uses Responses
	// lifecycle events. Keep both protocols well formed.
	if _, err := io.WriteString(w.dst, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if f, ok := w.dst.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func (w *responsesStreamWriter) startTextOutput() error {
	if w.textStarted {
		return nil
	}
	w.textStarted = true
	w.textIndex = w.nextIndex
	w.nextIndex++
	itemID := w.id + "-item"
	if err := w.emit("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "response_id": w.id, "output_index": w.textIndex,
		"item": map[string]any{"id": itemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}},
	}); err != nil {
		return err
	}
	return w.emit("response.content_part.added", map[string]any{
		"type": "response.content_part.added", "response_id": w.id, "item_id": itemID,
		"output_index": w.textIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

func (w *responsesStreamWriter) emit(event string, v map[string]any) error {
	raw, _ := json.Marshal(v)
	if !w.started {
		upstream.CopyResponseIDHeaders(w.dst.Header(), w.header)
		w.dst.Header().Set("Content-Type", "text/event-stream")
		w.dst.Header().Set("Cache-Control", "no-cache")
		w.dst.Header().Set("X-Accel-Buffering", "no")
		w.dst.WriteHeader(http.StatusOK)
		w.started = true
	}
	if _, err := fmt.Fprintf(w.dst, "event: %s\ndata: %s\n\n", event, raw); err != nil {
		return err
	}
	if f, ok := w.dst.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, tooLarge, err := readRequestBody(r)
	if tooLarge {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 8 MiB limit")
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if err := validateImageParts(body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_image", err.Error())
		return
	}
	// 请求体必须能解码成 JSON 对象。上游 client 在 prepareBody 里会返回
	// upstream.ErrUnprocessableBody（fail-closed，避免脱敏被畸形 body 绕过），
	// 但那个错误发生在选号之后的循环里，会被当成传输层错误逐号重试。
	// 在这里提前拒掉，既给出正确的 400，也不浪费号池与熔断计数。
	// 用 map 而不是 json.Valid：后者会放行 [1,2,3] 这类合法但非对象的 JSON，
	// 而那正是 prepareBody 会拒绝的输入。
	// 还必须显式检查 nil map：json.Unmarshal([]byte("null"), &doc) 返回
	// err == nil 且 doc == nil，只看 err 会漏掉 `null` 这个 4 字节的请求体，
	// 让它一路走到 prepareBody 才失败（并被误判成 503 no_healthy_account）。
	var bodyDoc map[string]any
	if err := json.Unmarshal(body, &bodyDoc); err != nil || bodyDoc == nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "request body must be a JSON object")
		return
	}
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &peek)

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	passthrough := h.requestPassthrough(r)
	st := newChatStatWithOptions(time.Now(), body, peek.Stream, h.cfg.MetricsStore, h.cfg.RequestLogStore, h.currentCreditPolicy(), passthrough, r.URL.Path)
	st.completionStore = h.cfg.CompletionStore
	// 影子扣减：成功请求的真实费用（upstream usage.credit 或费率估算）
	// 实时回写账号池，weightOf 的有效余额因子据此均衡组内消耗。
	if h.cfg.Pool != nil {
		st.spendHook = h.cfg.Pool.SpendCredits
	}
	defer st.done()
	// The upstream client canonicalizes public aliases before sending the body.
	// Use the same canonical ID for model-level routing/cooldowns so a limit
	// learned from kimi-k3-1 also applies to the upstream kimi-k3 request.
	routeModel := upstream.NormalizeModelID(st.model)

	tried := map[string]bool{}
	var lastErr error
	var lastKind upstream.ErrKind

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	sessKey := ""
	stickyUID := ""
	if h.cfg.Session != nil {
		sessKey = session.ExtractKey(body)
		if sessKey != "" {
			if uid, ok := h.cfg.Session.Resolve(sessKey); ok {
				stickyUID = uid
			}
		}
	}

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			h.cfg.Session.Unbind(sessKey)
			stickyUID = ""
		}
	}

	// 分组作用域：分组密钥把选号范围收窄到绑定分组的账号；管理员/会话
	// 请求为 nil（全池）。粘性号不属于当前分组时按"粘性号不可用"处理
	// （解绑回落），否则分组密钥会借粘性会话越权用别的分组的账号。
	scopeAllow := h.allowSet(scopeFromRequest(r))

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 选号：粘性号优先（同时校验账号级状态和当前模型限流），否则按当前模型轮换。
		var acct *auth.Auth
		if stickyUID != "" && (scopeAllow == nil || scopeAllow[stickyUID]) {
			acct = h.cfg.Pool.PickAndAcquireByUIDForModel(stickyUID, routeModel)
			if acct == nil {
				// 粘性号当前不可用（冷却/占满）→ 解绑，本次回落普通轮换。
				h.cfg.Session.Unbind(sessKey)
				stickyUID = ""
			}
		} else if stickyUID != "" {
			// 粘性号不在当前密钥的分组里 → 解绑（不是失败，是权限边界）。
			h.cfg.Session.Unbind(sessKey)
			stickyUID = ""
		}
		if acct == nil {
			acct = h.cfg.Pool.PickAndAcquireForModelAllow(routeModel, tried, scopeAllow)
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			break
		}
		st.uid = acct.UID
		st.region = acct.Region()
		tried[acct.UID] = true

		// Selection and reservation are atomic; retries are reserved for actual
		// upstream failures rather than races over the last account slot.
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("chat refresh uid=%s: save auth failed: %v", acct.UID, err)
			}
		}

		// 时长策略按请求类型分流：流式走 Stream（默认不限总时长 + 守空闲），
		// 非流式仍用整个请求的上限。二者语义不同，不能共用一个超时。
		policy := h.cfg.Upstream.RequestPolicy(peek.Stream)
		rc, status, respBody, upstreamHeaders, terr := h.cfg.Upstream.ChatStreamWithPolicy(r.Context(), acct, body, policy)
		upstream.CopyResponseIDHeaders(w.Header(), upstreamHeaders)
		upstreamHeaderID := upstream.ResponseIDFromHeader(upstreamHeaders)
		if upstreamHeaderID != "" {
			st.id = upstreamHeaderID
		}
		if terr != nil {
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 上游 client 已打 transport error 日志。
			st.status = http.StatusServiceUnavailable
			setRequestError(st, "transport_error", terr.Error())
			lastErr = terr
			fail(acct.UID)
			continue
		}
		if status >= 400 {
			st.status = status
			bodyText := string(respBody)
			kind := upstream.Classify(status, bodyText)
			lastKind = kind
			setRequestError(st, kind.String(), bodyText)
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: bodyText}
			h.applyErrorPolicy(acct.UID, routeModel, kind, bodyText)
			if kind == upstream.ErrClient {
				// 确定性 body 级 4xx（11128 首条须 system、11101 tool_choice 形态、
				// 11148 工具序列断裂等）：同一 body 换号重发必然复现同样错误，
				// 还会在上游按会话累积状态的场景下进一步污染会话。直接透传，
				// 让客户端看到真实原因。账号侧不罚（applyErrorPolicy 对 ErrClient
				// 本就只换号不喂熔断）。
				break
			}
			fail(acct.UID)
			continue
		}
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			streamErr := upstream.StreamWithOptionsAndID(w, stats, passthrough, upstreamHeaderID)
			st.ttfb = stats.TTFB()
			st.toks, _ = stats.Tokens()
			st.inputTokens, st.toks, st.totalTokens, st.cacheRead, st.cacheWrite, st.toolCalls = stats.UsageStats()
			if _, ok := stats.Tokens(); !ok {
				st.toks = -1
			}
			if credits, ok := stats.CreditUsage(); ok {
				st.creditsConsumed = credits
				st.creditSource = "upstream"
			}
			if responseID := stats.ResponseID(); responseID != "" {
				st.id = responseID
			}
			rc.Close()
			if streamErr != nil {
				st.status = http.StatusBadGateway
				setRequestError(st, "upstream_stream_error", streamErr.Error())
				log.Printf("chat_stream model=%s: %v", st.model, streamErr)
				return
			}
			h.cfg.Pool.NoteSuccess(acct.UID)
			if sessKey != "" && h.cfg.Session != nil {
				h.cfg.Session.Bind(sessKey, acct.UID)
			}
			st.status = http.StatusOK
			st.errorCode = ""
			st.errorMessage = ""
			return
		}
		resp, err := upstream.AggregateWithID(rc, upstreamHeaderID)
		rc.Close()
		if err != nil {
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			setRequestError(st, "upstream_parse", err.Error())
			return
		}
		usageStats(resp, st)
		if responseID := upstream.ResponseID(resp); responseID != "" {
			st.id = responseID
			if w.Header().Get("X-Request-Id") == "" {
				w.Header().Set("X-Request-Id", responseID)
			}
		}
		writeJSON(w, http.StatusOK, resp)
		h.cfg.Pool.NoteSuccess(acct.UID)
		if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}
		st.status = http.StatusOK
		st.errorCode = ""
		st.errorMessage = ""
		st.toks = completionTokens(resp)
		return
	}
	// A deterministic upstream 4xx (for example code=11128, which means the
	// first message must be system) is a request error, not an account-pool
	// outage. Preserve that status after rotation so clients can act on the
	// actual cause instead of receiving a misleading 503/no_healthy_account.
	if lastErr != nil && lastKind == upstream.ErrClient {
		if ue, ok := lastErr.(*upstream.Error); ok && ue.Status >= 400 && ue.Status < 500 {
			writeOpenAIError(w, ue.Status, "upstream_client_error", ue.Msg)
			st.status = ue.Status
			setRequestError(st, "upstream_client_error", ue.Msg)
			return
		}
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
	st.status = http.StatusServiceUnavailable
	setRequestError(st, "no_healthy_account", msg)
}

// validateImageParts catches malformed multimodal parts before they reach the
// upstream. It intentionally leaves unknown content-part types untouched for
// provider compatibility.
func validateImageParts(body []byte) error {
	var doc struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil // preserve the existing upstream handling for generic JSON errors
	}
	for mi, message := range doc.Messages {
		parts, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for pi, raw := range parts {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := part["type"].(string)
			if typ != "image_url" && typ != "input_image" {
				continue
			}
			if url, ok := part["image_url"].(string); ok && strings.TrimSpace(url) != "" {
				continue
			}
			if image, ok := part["image_url"].(map[string]any); ok {
				if url, _ := image["url"].(string); strings.TrimSpace(url) != "" {
					continue
				}
				if fileID, _ := image["file_id"].(string); strings.TrimSpace(fileID) != "" {
					continue
				}
			}
			if fileID, _ := part["file_id"].(string); strings.TrimSpace(fileID) != "" {
				continue
			}
			return fmt.Errorf("messages[%d].content[%d] image part requires image_url.url or file_id", mi, pi)
		}
	}
	return nil
}

func setRequestError(st *chatStat, code, message string) {
	st.errorCode = code
	st.errorMessage = truncateRequestError(message)
}

func truncateRequestError(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 1024 {
		return message[:1024]
	}
	return message
}

func (h *Handler) requestPassthrough(r *http.Request) bool {
	if !h.currentPassthrough() {
		return false
	}
	value := strings.TrimSpace(strings.ToLower(r.Header.Get("X-WorkBuddy-Passthrough")))
	if value == "" {
		return true
	}
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

// currentPassthrough 读运行时透传开关（WebUI 保存后即时变更，不再读启动配置）。
func (h *Handler) currentPassthrough() bool {
	h.featuresMu.RLock()
	defer h.featuresMu.RUnlock()
	return h.passthrough
}

// responsesAPIEnabled 读运行时 Responses 端点开关。
func (h *Handler) responsesAPIEnabled() bool {
	h.featuresMu.RLock()
	defer h.featuresMu.RUnlock()
	return !h.responsesOff
}

// currentCreditPolicy 读运行时费率（billing.* 由 WebUI 修改即时生效）。
func (h *Handler) currentCreditPolicy() CreditPolicy {
	h.featuresMu.RLock()
	defer h.featuresMu.RUnlock()
	return h.creditPolicy
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify），此处不再按原始 status 二次判断。
// 仅在 chatCompletions 轮转循环内调用：调用方已准备好 lastErr 并打算 continue 换号。
//
// 五条路径，各司其职：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate：有模型上下文时只冷却该模型；无模型时退回账号级 CoolSoft/CoolRateLimit。
//     上游 code=6004 或带 reset 时间的限流 → 精确冷却到 reset，期间不对该模型兜底重试。
//   - ErrNotFound → Cooldown(CoolSoft)：即时账号级软冷却（404）。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断。
//
// 恢复出口：账号级/模型级 CoolSoft、CoolRateLimit、CoolHard 各自到期自动恢复；熔断按其指数退避截止到期；
// 成功（NoteSuccess）清 fails/熔断；签到解冻（ReenableIfCredits→reviveCoolingLocked）只清冷却，不动熔断。
func (h *Handler) applyErrorPolicy(uid, model string, kind upstream.ErrKind, body string) {
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSoftRate:
		now := time.Now()
		model = strings.TrimSpace(model)
		if model == "-" {
			model = ""
		}
		if isExplicitRateLimit(body) {
			if resetAt, ok := rateLimitResetAt(body, now); ok {
				reason := rateLimitReason(body, resetAt)
				if model != "" {
					h.cfg.Pool.CooldownModelUntil(uid, model, resetAt, reasonWithModel(reason, model))
				} else {
					h.cfg.Pool.CooldownUntil(uid, pool.CoolRateLimit, resetAt, reason)
				}
			} else {
				// code=6004 without a parseable timestamp remains strict: do not
				// keep hammering the account while the upstream window is unknown.
				reason := rateLimitFallbackReason(body)
				if model != "" {
					h.cfg.Pool.CooldownModel(uid, model, h.cfg.SoftCooldown, reasonWithModel(reason, model))
				} else {
					h.cfg.Pool.Cooldown(uid, pool.CoolRateLimit, h.cfg.SoftCooldown, reason)
				}
			}
		} else {
			reason := "429 rate limit"
			if model != "" {
				h.cfg.Pool.CooldownModel(uid, model, h.cfg.SoftCooldown, reasonWithModel(reason, model))
			} else {
				h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, reason)
			}
		}
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(uid, "12153 session dead")
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")
	case upstream.ErrServer:
		// 5xx 上游故障：Classify 已把 ≥500 判为 ErrServer，在此喂熔断计数（不再手写 status>=500）。
		h.cfg.Pool.NoteError(uid)
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚（防雪崩），不喂熔断。
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func readRequestBody(r *http.Request) ([]byte, bool, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	if len(raw) > maxRequestBodyBytes {
		return nil, true, nil
	}
	return raw, false, err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}
