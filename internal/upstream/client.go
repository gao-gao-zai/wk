// Package upstream 封装对 CodeBuddy 上游（chat / billing / auth）的全部 HTTP 调用，
// 以及错误分类（驱动 pool 冷却状态机）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone        ErrKind = iota // 成功
	ErrHardCredit                 // 余额不足（402 或 body 关键词）→ 长冷却
	ErrSoftRate                   // 429 软限流 → 短冷却
	ErrSessionDead                // 401 + 12153 offline session 失效 → 禁用
	ErrNotFound                   // 404 上游偶发 → 短冷却，不累计错误计数（防雪崩）
	ErrServer                     // 5xx 上游故障
	// ErrRiskFlag 上游 403 + code=11140（"request illegal"，displayMsg 为
	// "内容未通过安全审核"）：账号级风控标记，对该账号的所有请求一律失败，
	// 且不会自愈（余额/token 都正常，唯独 chat 通道被拒）。与 ErrClient 的
	// 关键差异：ErrClient 是"请求体形态错误"（换号必复现、账号无辜），
	// 11140 则高度指向账号本身被标记。由于健康号偶发的内容审核触发也会
	// 返回 11140，责任归属由 pool 侧的连续计数（NoteRiskStrike）判定，
	// 达阈值才 Disable；成功一次即清零。
	ErrRiskFlag
	ErrClient // 其他 4xx / 业务错误
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrRiskFlag:
		return "risk_flag"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// hardMarkers 余额不足关键词（小写比较 + 中文原文比较双通道）。
//
// 上游 14018 的实际措辞是"额度已用尽"（多一个"已"字）——它不是"额度用尽"
// 的子串，历史上因此漏判成 soft_rate，额度耗尽的号只冷却 60s 就被重新捞出，
// 反复撞 429。这里同时收录两种措辞与数字码 14018 本身（信封 code 通道）。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "额度已用尽", "没有积分",
}

var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// Classify 按 HTTP 状态码 + body 判定错误类别。
func Classify(status int, body string) ErrKind {
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	// 14018 是上游"额度已用尽（需购买加量包）"的确定性数字码：关键词措辞
	// 可能再变，数字码稳定，放在关键词匹配之前做精确判定。
	if isHardCredit1418(body) {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return ErrSessionDead
		}
	}
	// code=6004 is the upstream's deterministic rate-limit response. Some
	// gateway paths wrap the original 429 as HTTP 503, so inspect the body too.
	if status == http.StatusTooManyRequests || isRateLimit6004(body) {
		return ErrSoftRate
	}
	// 11140 是账号级风控标记（403 "request illegal"），必须在通用 4xx 兜底
	// 之前判定，否则会落进 ErrClient 的"只换号不罚"——被标记的号因此永远
	// 显示健康并持续吃流量（线上实测：15 个号反复 403，err_total 恒 0）。
	if status == http.StatusForbidden && isRiskFlag11140(body) {
		return ErrRiskFlag
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		return ErrClient
	}
	// HTTP 200 但业务 code 非 0 且含余额关键词的情况已被上面 hardMarkers 捕获。
	return ErrNone
}

// isHardCredit1418 识别上游信封里的 code=14018（额度已用尽，购买加量包）。
// 与 isRateLimit6004 同构：正文可能被外层信封包裹（如最终 503 携带原始 JSON），
// 因此除标准解析外再做一次 "code":14018 的宽松子串匹配。
func isHardCredit1418(body string) bool {
	var envelope struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err == nil && envelope.Code == 14018 {
		return true
	}
	return strings.Contains(body, `"code":14018`) || strings.Contains(body, `"code":"14018"`)
}

func isRateLimit6004(body string) bool {
	var envelope struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err == nil && envelope.Code == 6004 {
		return true
	}
	// Preserve classification when an outer error message prefixes the
	// original JSON, or when the upstream returns a non-JSON diagnostic.
	lower := strings.ToLower(body)
	return strings.Contains(body, "6004") &&
		(strings.Contains(body, "使用量已超出频率限制") ||
			strings.Contains(lower, "rate limit") || strings.Contains(lower, "soft_rate"))
}

// riskFlagMarkers 11140 风控的文本副信道。code=11140 是主信道（数字码稳定），
// 文本匹配是冗余兜底：上游措辞历史上有过漂移（14018 "额度用尽"→"额度已用尽"
// 就漏判过），displayMsg 的 zh/en 两条都收录，与 hardMarkers 的双通道思路一致。
var riskFlagMarkers = []string{
	"request illegal",
	"内容未通过安全审核",
	"did not pass the safety review",
}

// isRiskFlag11140 识别上游信封里的 code=11140（账号级风控标记）。
// 与 isHardCredit1418 同构：标准解析 + 宽松子串双通道，外层信封包裹时也能命中。
func isRiskFlag11140(body string) bool {
	var envelope struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err == nil && envelope.Code == 11140 {
		return true
	}
	if strings.Contains(body, `"code":11140`) || strings.Contains(body, `"code":"11140"`) {
		return true
	}
	for _, m := range riskFlagMarkers {
		if strings.Contains(body, m) {
			return true
		}
	}
	return false
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 上游 HTTP 客户端。Base 字段可覆盖便于测试。
type Client struct {
	HTTP *http.Client

	// Stream 是流式 chat 请求的时长策略。零值 = 只受调用方 ctx 约束（即不限总时长、
	// 不检查空闲），对长回答最友好；main 会按配置注入非零值。
	// 注意它**不**影响 HTTP.Timeout 所覆盖的非流式/控制面请求。
	// 运行时经 SetStreamPolicy 热改（WebUI 上游超时设置）；请求路径在
	// RequestPolicy/ChatStreamContextWithHeaders 里读快照。
	Stream StreamPolicy

	// effortsMu/efforts 缓存各模型 supportedEfforts（FetchModels 刷新），供请求体 effort 降级。
	effortsMu sync.RWMutex
	efforts   map[string][]string

	// SanitizeFingerprints 出站请求体黑名单指纹脱敏开关（默认 true；false 完全还原）。
	// 运行时经 SetSanitizeFingerprints 修改（WebUI 特性开关）；并发请求路径
	// 在 prepareBody 里读，写侧持 flagsMu 保证不撕裂。
	SanitizeFingerprints bool

	flagsMu sync.Mutex

	ChatBaseCN      string
	BillingBaseCN   string
	ChatBaseGlobal  string
	BillingBaseGlob string
	// WebBaseCN Web 域（任务领奖）。零值回落默认常量（webBase()），
	// 字段化便于测试注入。
	WebBaseCN string

	// DialProxy reqproxy 接入钩子（账号级统一路由，见 reqproxy_hook.go）。
	// nil = 模块关闭，全部直连，行为与旧版逐字节一致。
	DialProxy DialProxyFunc
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	// Keep the standard dial/TLS timeouts and HTTP/2 negotiation. A larger idle
	// pool avoids repeating handshakes after concurrent streaming bursts.
	// PerHost must cover the deployment's full serving capacity (accounts ×
	// max-in-flight): the upstream is a single host, and every connection
	// beyond the idle cap is torn down after one use and pays a fresh public-
	// internet TLS handshake (~1-2 RTT) on the next request. 512 leaves room
	// for the load-test default of 100×3 plus admin/control-plane traffic.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = 1024
	tr.MaxIdleConnsPerHost = 512
	return &Client{
		HTTP:                 &http.Client{Timeout: 120 * time.Second, Transport: tr},
		SanitizeFingerprints: true,
		// 生产默认：流式不限总时长（长回答不被掐断），但守 120s 空闲。
		// 显式给出，避免零值悄悄退化成"既不限总时长也不查空闲"。
		Stream:          StreamPolicy{Total: 0, Idle: 120 * time.Second},
		ChatBaseCN:      "https://copilot.tencent.com",
		BillingBaseCN:   "https://www.codebuddy.cn",
		ChatBaseGlobal:  "https://www.workbuddy.ai",
		BillingBaseGlob: "https://www.workbuddy.ai",
	}
}

func (c *Client) chatBase(a *auth.Auth) string {
	if a != nil && a.Region() == "global" {
		return c.ChatBaseGlobal
	}
	return c.ChatBaseCN
}

// prepareBody 组装出站请求体（脱敏开关由 Client.SanitizeFingerprints 控制）。
// 返回错误表示请求体无法解码——此时必须拒绝请求而不是原样转发，
// 否则内容脱敏会被一个畸形 body 绕过（见 ErrUnprocessableBody）。
func (c *Client) prepareBody(body []byte) ([]byte, error) {
	return PrepareBodyOptWithEfforts(body, c.sanitizeEnabled(), c.effortsSnapshot())
}

// prepareBodyFromObj 是 prepareBody 的免解码入口：server 层在请求入口已经
// 做过一次全量 map 解码（校验/统计/路由共用），传进来可省掉对大请求体的
// 第二次 map[string]any 解码。fail-closed 语义与 prepareBody 完全一致。
func (c *Client) prepareBodyFromObj(obj map[string]any) ([]byte, error) {
	return PrepareBodyFromMap(obj, c.sanitizeEnabled(), c.effortsSnapshot())
}

// sanitizeEnabled 读取脱敏开关的并发安全快照。
func (c *Client) sanitizeEnabled() bool {
	c.flagsMu.Lock()
	defer c.flagsMu.Unlock()
	return c.SanitizeFingerprints
}

// SetSanitizeFingerprints 运行时切换指纹脱敏开关（WebUI 特性开关用）。
func (c *Client) SetSanitizeFingerprints(enabled bool) {
	c.flagsMu.Lock()
	defer c.flagsMu.Unlock()
	c.SanitizeFingerprints = enabled
}

// SetStreamPolicy 运行时更新流式时长策略（WebUI 上游超时设置用）。
// Total/Idle/HeaderWait 同步替换；已在途的流不受影响（它们的看门狗
// 建立时已取好策略副本）。
func (c *Client) SetStreamPolicy(p StreamPolicy) {
	c.flagsMu.Lock()
	defer c.flagsMu.Unlock()
	c.Stream = p
}

// SetRequestTimeout 运行时更新非流式整请求上限（WebUI 上游超时设置用）。
// http.Client.Timeout 文档未承诺并发读写安全，这里经 flagsMu 串行化写；
// 读侧（RequestPolicy）拿的是瞬时值，撕裂窗口内最多旧值多生效一个请求。
func (c *Client) SetRequestTimeout(d time.Duration) {
	c.flagsMu.Lock()
	defer c.flagsMu.Unlock()
	c.HTTP.Timeout = d
}

// streamPolicySnapshot 返回当前流式策略的并发安全快照。
func (c *Client) streamPolicySnapshot() StreamPolicy {
	c.flagsMu.Lock()
	defer c.flagsMu.Unlock()
	return c.Stream
}

// effortsSnapshot 返回 effort 能力缓存副本；nil 表示未知（透传不降级）。
func (c *Client) effortsSnapshot() map[string][]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	if len(c.efforts) == 0 {
		return nil
	}
	cp := make(map[string][]string, len(c.efforts))
	for k, v := range c.efforts {
		cp[k] = v
	}
	return cp
}

func (c *Client) billingBase(a *auth.Auth) string {
	if a != nil && a.Region() == "global" {
		return c.BillingBaseGlob
	}
	return c.BillingBaseCN
}

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
// 账号级统一路由（reqproxy）：a 非 nil 且 DialProxy 已配置时经该账号槽位出站。
func (c *Client) doJSON(req *http.Request, a *auth.Auth) (json.RawMessage, error) {
	cli, err := c.proxyClientFor(a)
	if err != nil {
		return nil, err // D4：无可用节点，不回退直连
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。全程持 a 锁，防止并发 SaveAtomic 读半更新 token。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	credentials := a.Snapshot()
	if strings.TrimSpace(credentials.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	url := c.chatBase(a) + "/v2/plugin/auth/token/refresh"
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	RefreshHeaders(req, a)
	data, err := c.doJSON(req, a)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
	}
	// preserveExpiry：响应缺 expiresIn 时保留旧过期时间，避免刷新风暴。
	expiresAt := int64(0)
	if tok.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	a.ApplyRefresh(tok.AccessToken, tok.RefreshToken, tok.Domain, expiresAt)
	return nil
}

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、body 为上游响应体（供调用方 Classify(status, string(body))）、err 为 nil；
// 只有传输层失败才返回 err。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	return c.ChatStreamContext(context.Background(), a, body)
}

// ChatStreamContext attaches the caller context so a disconnected client can
// cancel the upstream request and release the account lease promptly.
func (c *Client) ChatStreamContext(ctx context.Context, a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	rc, status, respBody, _, err = c.ChatStreamContextWithHeaders(ctx, a, body)
	return rc, status, respBody, err
}

// ChatStreamContextWithHeaders is ChatStreamContext plus the original
// WorkBuddy response headers. Callers use them to pass request/record IDs to
// downstream clients without rewriting their values.
func (c *Client) ChatStreamContextWithHeaders(ctx context.Context, a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, headers http.Header, err error) {
	return c.ChatStreamWithPolicy(ctx, a, body, c.streamPolicySnapshot())
}

// RequestPolicy 按请求类型选择时长策略。
//
// 非流式沿用 HTTP.Timeout（整请求上限，对"一次性拿完整响应"是正确语义）；
// 流式用 Stream 策略（默认不限总时长 + 守空闲）。两者必须分开，否则要么长回答被
// 掐断，要么非流式失去兜底而可能永远挂住。
func (c *Client) RequestPolicy(stream bool) StreamPolicy {
	if stream {
		return c.streamPolicySnapshot()
	}
	// 非流式：整个请求（含读完响应体）的上限；等响应头同受其约束。
	// 经锁读 HTTP.Timeout：SetRequestTimeout 的并发写才有确定的 happens-before。
	c.flagsMu.Lock()
	defer c.flagsMu.Unlock()
	return StreamPolicy{Total: c.HTTP.Timeout}
}

// StreamPolicy 单个 chat 请求的时长策略。零值 = 不限总时长、不检查空闲。
//
// 之所以要区分 Total 与 Idle：http.Client.Timeout 是"整个请求（含读完响应体）"的
// 墙钟上限，对非流式是恰当的，用在流式上却等于给回答长度设了死限。流式真正需要的是
// Idle（上游卡住才失败，持续产出就一直流），Total 只作为防止流跑飞的兜底。
type StreamPolicy struct {
	// Total 整个请求（含读完响应体）的上限。0 = 不限，仅靠 Idle 与调用方 ctx 约束。
	Total time.Duration
	// Idle 两次成功读取之间的最大间隔。0 = 不检查。
	// 上游持续发数据时永不影响；上游卡住时尽快失败。
	Idle time.Duration
	// HeaderWait 等待响应头的上限。0 = 沿用 Idle（见 headerWait）。
	//
	// 单独留一个窗口，是因为 Total=0（默认）时"等响应头"这一阶段否则完全不受约束：
	// 那时还没拿到 body，看门狗无从守起，若上游接了连接却不回响应头，请求会一直挂住
	// 并占着在途租约。它只约束**开始**，不影响已经开始产出的流。
	HeaderWait time.Duration
}

// defaultHeaderWait 既没配 HeaderWait 也没配 Idle 时的兜底。
// 取 120s：远小于"长回答"的时长，又足以容纳上游排队/建连较慢的情况。
const defaultHeaderWait = 120 * time.Second

// headerWait 解析等响应头的窗口。
//
// 默认沿用 Idle：语义上"还没收到任何数据"与"两次数据之间"是同一件事，用同一个窗口
// 更符合直觉，也省掉一个需要单独调参的常量——把 idle 调大时，等响应头的容忍度随之
// 变宽，反之亦然。仅在两者都没配时才用 defaultHeaderWait。
func (p StreamPolicy) headerWait() time.Duration {
	switch {
	case p.HeaderWait > 0:
		return p.HeaderWait
	case p.Idle > 0:
		return p.Idle
	default:
		return defaultHeaderWait
	}
}

// ChatStreamWithPolicy 是 ChatStreamContextWithHeaders 加上显式时长策略的版本。
//
// 实现要点：不修改 c.HTTP.Timeout（那是控制面 JSON 请求的整请求超时），而是在本次
// 请求上复制一份 client 并清零 Timeout，改由 ctx 承担 Total、由 idleBody 承担 Idle。
// 复制是安全的——http.Client 不含任何锁；Transport 仍是同一个指针，因此测试里
// 替换 up.HTTP.Transport 的做法照旧生效。
func (c *Client) ChatStreamWithPolicy(ctx context.Context, a *auth.Auth, body []byte, policy StreamPolicy) (rc io.ReadCloser, status int, respBody []byte, headers http.Header, err error) {
	outBody, err := c.prepareBody(body)
	if err != nil {
		return nil, 0, nil, nil, err
	}
	return c.chatStreamPrepared(ctx, a, outBody, policy)
}

// ChatStreamWithPolicyObj 是 ChatStreamWithPolicy 的免解码入口：调用方把
// 请求入口已解码好的 body map 传入，省掉第二次 map[string]any 全量解码。
// 其余语义（时长策略、看门狗、错误分类）与 ChatStreamWithPolicy 完全一致。
func (c *Client) ChatStreamWithPolicyObj(ctx context.Context, a *auth.Auth, obj map[string]any, policy StreamPolicy) (rc io.ReadCloser, status int, respBody []byte, headers http.Header, err error) {
	outBody, err := c.prepareBodyFromObj(obj)
	if err != nil {
		return nil, 0, nil, nil, err
	}
	return c.chatStreamPrepared(ctx, a, outBody, policy)
}

// chatStreamPrepared 发送已改写完毕的出站请求体并处理响应。
func (c *Client) chatStreamPrepared(ctx context.Context, a *auth.Auth, outBody []byte, policy StreamPolicy) (rc io.ReadCloser, status int, respBody []byte, headers http.Header, err error) {
	url := c.chatBase(a) + "/v2/chat/completions"
	// Total（可为 0=不限）覆盖全程：等响应头 + 读响应体。
	reqCtx, cancel := context.WithCancel(ctx)
	if policy.Total > 0 {
		reqCtx, cancel = context.WithTimeout(ctx, policy.Total)
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(outBody))
	if err != nil {
		cancel()
		return nil, 0, nil, nil, err
	}
	ChatHeaders(req, a)

	// 等响应头单独设一个可解除的看门狗。不能直接用 context.WithTimeout 当这个窗口：
	// 响应体的读取同样绑定在 reqCtx 上，那样等于又给整个流加了总时限，回到要修的问题。
	// 所以只在"响应头还没到"这段时间计时，响应头一到就解除。
	//
	// 为什么需要它：默认 Total=0 时，这一阶段否则完全不受约束——那时还没有 body，
	// timeoutBody 的看门狗无从守起；上游接了连接却不回响应头就会一直挂住并占着租约。
	//
	// 用 CAS 而不是"Stop + 事后读标志"：Stop 无法撤销一个已经开始执行的回调，若定时器
	// 恰好在响应头到达的同时触发，就会出现"标志还没置上、ctx 已被取消"的窗口，把一个
	// 健康的流打断。让回调和主流程争抢同一个原子量，只有抢到的一方能取消 ctx。
	var headerArrived atomic.Bool
	var headerTimedOut atomic.Bool
	headerWait := policy.headerWait()
	if policy.Total > 0 && policy.Total < headerWait {
		headerWait = policy.Total
	}
	headerTimer := time.AfterFunc(headerWait, func() {
		if headerArrived.CompareAndSwap(false, true) {
			headerTimedOut.Store(true)
			cancel()
		}
	})

	// 复制 client 并清零 Timeout：流式的时长已完全交给 policy，若保留整请求超时
	// 会把长流式回答再次掐断，那就白改了。
	// reqproxy（账号级统一路由）：先经钩子取该账号的 client（代理 Transport 或直连原样），
	// 再清零 Timeout。D4：钩子报错（无可用节点）直接失败，不回退直连。
	baseCli, err := c.proxyClientFor(a)
	if err != nil {
		cancel()
		return nil, 0, nil, nil, err
	}
	cli := *baseCli
	cli.Timeout = 0
	resp, err := cli.Do(req)
	// 声明"响应头已到"：若回调抢先抢到 CAS，这里会失败且 headerTimedOut 已置位。
	alreadyArrived := headerArrived.CompareAndSwap(false, true)
	headerTimer.Stop()
	if err != nil {
		cancel()
		if headerTimedOut.Load() {
			// 换成可识别的哨兵，别让上游看到一句无来由的 "context canceled"。
			err = fmt.Errorf("%w (waited %s for response headers)", ErrStreamHeaderTimeout, headerWait)
		}
		log.Printf("chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, nil, err
	}
	if !alreadyArrived || headerTimedOut.Load() {
		// 响应头刚到、定时器同时触发并取消了 reqCtx。此时 body 已随 ctx 失效，与其返回
		// 一个注定读失败的流（下游只会看到一句语焉不详的 "context canceled"），不如在这里
		// 就按"等响应头超时"如实报错。
		resp.Body.Close()
		cancel()
		err := fmt.Errorf("%w (waited %s for response headers)", ErrStreamHeaderTimeout, headerWait)
		log.Printf("chat_stream uid=%s: %v", a.UID, err)
		return nil, 0, nil, nil, err
	}
	headers = resp.Header.Clone()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("chat_stream uid=%s: upstream %d %s body=%s",
			a.UID, resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, headers, nil
	}
	// 交还 body 时把 cancel 挂上：调用方 Close 即释放 ctx（含 Total 定时器）。
	return newTimeoutBody(resp.Body, policy.Total, policy.Idle, cancel), resp.StatusCode, nil, headers, nil
}

// ModelInfo 动态模型信息（含 maxInputTokens/maxOutputTokens）。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64    // = maxInputTokens
	MaxTokens     int64    // = maxOutputTokens
	Efforts       []string // reasoning.supportedEfforts（空=未知/固定档）
}

// FetchModels 调上游动态模型接口。
// 字段名与上游实际返回对齐：maxInputTokens（非 contextWindow）、maxOutputTokens（非 maxTokens）。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	url := c.chatBase(a) + "/console/enterprises/personal/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.Snapshot().AccessToken)
	req.Header.Set("Accept", "application/json")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
	// 账号级统一路由（reqproxy）：经钩子取该账号 client（代理 Transport 或直连原样）。
	cli, err := c.proxyClientFor(a)
	if err != nil {
		return nil, err // D4：无可用节点，不回退直连
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID              string `json:"id"`
				Name            string `json:"name"`
				MaxInputTokens  int64  `json:"maxInputTokens"`
				MaxOutputTokens int64  `json:"maxOutputTokens"`
				Disabled        bool   `json:"disabled"`
				Reasoning       struct {
					Effort           string   `json:"effort"`
					SupportedEfforts []string `json:"supportedEfforts"`
				} `json:"reasoning"`
			} `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}
	dynMap := make(map[string]struct {
		ID              string
		Name            string
		MaxInputTokens  int64
		MaxOutputTokens int64
		Disabled        bool
		Efforts         []string
	}, len(env.Data.Models))
	for _, m := range env.Data.Models {
		info := struct {
			ID              string
			Name            string
			MaxInputTokens  int64
			MaxOutputTokens int64
			Disabled        bool
			Efforts         []string
		}{m.ID, m.Name, m.MaxInputTokens, m.MaxOutputTokens, m.Disabled, m.Reasoning.SupportedEfforts}
		dynMap[m.ID] = info
		canonical := NormalizeModelID(m.ID)
		if _, exists := dynMap[canonical]; !exists {
			dynMap[canonical] = info
		}
	}
	out := make([]ModelInfo, 0, len(cliIDs))
	seen := make(map[string]struct{}, len(cliIDs))
	for _, id := range cliIDs {
		canonical := NormalizeModelID(id)
		m, ok := dynMap[id]
		if !ok {
			m, ok = dynMap[canonical]
		}
		if !ok || m.Disabled {
			continue
		}
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		out = append(out, ModelInfo{
			ID:            canonical,
			Name:          m.Name,
			ContextWindow: m.MaxInputTokens,
			MaxTokens:     m.MaxOutputTokens,
			Efforts:       m.Efforts,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	// 刷新 effort 能力缓存（供请求体降级；无 supportedEfforts 的模型不入缓存）。
	cache := make(map[string][]string, len(out))
	for _, mi := range out {
		if len(mi.Efforts) > 0 {
			cache[mi.ID] = mi.Efforts
		}
	}
	c.effortsMu.Lock()
	c.efforts = cache
	c.effortsMu.Unlock()
	return out, nil
}

// ResourceUsage contains the aggregate upstream credit counters for one user.
// Remaining follows the same cycle-first rule used by the account pool.
type ResourceUsage struct {
	Remaining           int64
	CapacitySize        int64
	CapacityRemain      int64
	CapacityUsed        int64
	CycleCapacitySize   int64
	CycleCapacityRemain int64
	CycleCapacityUsed   int64
}

// RequestUsage is the authoritative per-request credit record returned by the
// billing meter. Input text is intentionally not modeled or persisted.
type RequestUsage struct {
	RequestID   string  `json:"requestId"`
	Credit      float64 `json:"credit"`
	Model       string  `json:"model"`
	RequestTime string  `json:"requestTime"`
}

// UserRequestUsage queries the web billing meter for recent request charges.
func (c *Client) UserRequestUsage(a *auth.Auth, start, end time.Time, page, pageSize int) ([]RequestUsage, int, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 200
	}
	body := map[string]any{"startTime": start.Format("2006-01-02 15:04:05"), "endTime": end.Format("2006-01-02 15:04:05"), "pageNum": page, "pageSize": pageSize}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	usagePath := "/billing/meter/get-user-request-usage"
	req, err := http.NewRequest(http.MethodPost, c.billingBase(a)+usagePath, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, err
	}
	BillingHeaders(req, a)
	data, err := c.doJSON(req, a)
	if err != nil {
		return nil, 0, err
	}
	var resp struct {
		Data  []RequestUsage `json:"data"`
		Total int            `json:"total"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, 0, fmt.Errorf("request usage parse: %w", err)
	}
	return resp.Data, resp.Total, nil
}

// UserResourceDetails queries the upstream credit counters for one account.
func (c *Client) UserResourceDetails(a *auth.Auth) (ResourceUsage, error) {
	url := c.billingBase(a) + "/v2/billing/meter/get-user-resource"
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return ResourceUsage{}, err
	}
	BillingHeaders(req, a)
	data, err := c.doJSON(req, a)
	if err != nil {
		return ResourceUsage{}, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return ResourceUsage{}, fmt.Errorf("resource parse: %w", err)
	}
	usage := ResourceUsage{}
	for _, acct := range resp.Response.Data.Accounts {
		var r int64
		switch {
		case acct.CycleCapacitySize > 0:
			r = acct.CycleCapacityRemain
		case acct.CycleCapacityRemain > 0 || acct.CycleCapacityUsed > 0:
			r = acct.CycleCapacityRemain
		default:
			r = acct.CapacityRemain
		}
		if r < 0 {
			r = 0
		}
		usage.CapacitySize += maxInt64(acct.CapacitySize, 0)
		usage.CapacityRemain += maxInt64(acct.CapacityRemain, 0)
		usage.CapacityUsed += maxInt64(acct.CapacityUsed, 0)
		usage.CycleCapacitySize += maxInt64(acct.CycleCapacitySize, 0)
		usage.CycleCapacityRemain += maxInt64(acct.CycleCapacityRemain, 0)
		usage.CycleCapacityUsed += maxInt64(acct.CycleCapacityUsed, 0)
		usage.Remaining += r
	}
	return usage, nil
}

// UserResource keeps the legacy balance-only API used by older callers.
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	usage, err := c.UserResourceDetails(a)
	if err != nil {
		return 0, err
	}
	return usage.Remaining, nil
}

func maxInt64(value, floor int64) int64 {
	if value < floor {
		return floor
	}
	return value
}

// DailyCheckin 执行每日签到。已签到（业务 code 非 0）也返回错误，调用方按 msg 区分。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	url := c.billingBase(a) + "/v2/billing/meter/daily-checkin"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	BillingHeaders(req, a)
	_, err = c.doJSON(req, a)
	return err
}

// alreadyCheckinMarkers "今天已签到"关键词（上游对重复签到返回 code!=0，
// 如 code=10001/14001）。幂等成功而非失败，调度日志不应刷 error 行。
var alreadyCheckinMarkers = []string{"已签到", "already"}

// IsAlreadyCheckin 报告 err 是否表示"今天已签到"（上游幂等拒绝重复签到）。
// 只认带分类的 *Error（业务 code 或 HTTP 错误）：网络层/解析层错误不得当作幂等成功，
// 否则签到遇抖动会误记为 already，账号当天实际未签到却被判定正常。
func IsAlreadyCheckin(err error) bool {
	var ue *Error
	if !errors.As(err, &ue) {
		return false
	}
	for _, m := range alreadyCheckinMarkers {
		if strings.Contains(ue.Msg, m) || strings.Contains(strings.ToLower(ue.Msg), strings.ToLower(m)) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
