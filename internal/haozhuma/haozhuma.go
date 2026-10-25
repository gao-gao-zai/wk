package haozhuma

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client 调用豪猪接码平台 API（api.haozhuma.com/sms/?api=...）。
//
// 接口全部是 GET + query。code 字段："0"/"200" 成功；"-1" 且 msg 含
// "等待" 表示短信还没到（轮询用）；其它为失败。
//
// Provider "ecode"（2026-10 新增）：E码云/D1系（eomsg.com / uomsg.com，
// 同一模板）。API 形态完全不同——纯文本响应、keyWord 取号、无 token
// 换取接口（token 在网页版个人中心手动创建）。差异要点：
//   - 基址 https://api.eomsg.com/zc/data.php，参数 code=<动作>&token=...
//   - getPhone: keyWord（对应豪猪 sid 的语义位置）+ 可选 phone/province/cardType
//   - getMsg:   phone + keyWord；"尚未收到" 表示等待（对应豪猪 code=-1 等待）
//   - 失败统一 "ERROR:xxx" 纯文本前缀
//   - release/block 平台已自动化（指定号除外），调用视为 no-op
//   - token 由网页版创建后长期有效，Relogin 无操作（保留账密仅为兼容）
type Client struct {
	Base    string
	Token   string
	Timeout time.Duration
	HTTP    *http.Client

	// Provider 接码平台选择。空 / "haozhuma" = 豪猪 JSON API；
	// "ecode" = E码云/D1 系纯文本 API（Base 应指向各自 data.php）。
	Provider string

	// CardType E码云取号卡类型：实卡/虚卡/全部（空 = 全部）。仅 ecode。
	CardType string

	// Province E码云取号归属地（空 = 不限）。仅 ecode。
	Province string

	// Fallback 兜底接码客户端（2026-10 双平台共存）：主客户端（豪猪）
	// GetPhone 失败时自动切到这里（E码云）；GetMessage 按号码来源路由。
	// 取号成功后号码记入 phoneSource；收码时先查来源，路由到对应平台。
	// Release/Blacklist：豪猪侧照常，ecode 侧 no-op（平台自动）。
	// 配置：sms.haozhuma.fallback_*（见 config.go）。
	Fallback *Client

	// Mode 平台选择模式（前端「自动加号」页热切换/任务级覆盖）：
	//   ""/"auto"      = 豪猪优先，失败自动切 Fallback（默认，兼容旧行为）
	//   "haozhuma"     = 仅豪猪（摘掉 Fallback）
	//   "ecode"        = 仅 Fallback 平台
	//   "both"         = 双平台并行：交替首选，失败换边再试（两池同耗）
	// 仅当 Fallback != nil 时非 auto 模式才有意义；Fallback 为 nil 时
	// 一切行为退回主客户端单平台。
	Mode string

	// altPrefersBoth both 模式轮替游标：true = 本次先试 Fallback 侧。
	altPreferAlt bool

	// phoneSource 号码来源记账（fallback 路由用）：
	// phone -> "ecode"（取号走的是兜底平台）。
	phoneSource map[string]string

	// user/pass 留着，token 失效时 Relogin 用。
	mu   sync.Mutex
	user string
	pass string

	// Author 「[限对接]」项目的对接方标识。空字符串表示不下发。
	Author string

	// uid 是**可选钉死**的对接码（豪猪后台的"专属码"）。
	// 上游会增删对接码，钉死的那个可能整批失效；GetPhone 遇到该情况会
	// 自动丢弃它并退回平台自动分配，见 GetPhone 的注释。
	// 并发下由 mu 保护（多个 worker 可能同时取号）。
	uid string
	// lastUnknownUID 记录最近一次因失效被丢弃的对接码，供上层提示用户。
	lastUnknownUID string

	// uidPool 多对接码轮换池（2026-09 新增，豪猪侧「加入对接码」逆向
	// 完成后的自动管理）。非空时 GetPhone 每次取下一个码（round-robin）；
	// 某个码报「专属码不存在/没号」时自动出池，出池的码记进 drained
	// 供上层通过 H5 从账户移除。池空后退回单 uid / 平台自动分配。
	uidPool []string
	// uidIdx round-robin 游标。
	uidIdx int
	// drained 已出池的对接码（等上层消费走 H5 type=41 移出账户）。
	drained []string

	// ISP 运营商优先级列表（取号参数 isp）。实测取值：1=移动 2=联通 3=电信，
	// 逗号分隔表示依次降级，最后自动退回"不限"。
	// 空字符串表示直接不限。腾讯短信通道对广电号支持不稳定。
	ISP string
}

// newHTTPClient 打码/接码平台的共享 client。DefaultTransport 的
// MaxIdleConnsPerHost=2 会让轮询（GetPhone/轮询收码）频繁重建连接；
// 这里克隆 DefaultTransport 并放宽到 8——单账号场景足够，取号轮询的
// 并发度不会更高。
func newHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = 8
	tr.MaxIdleConnsPerHost = 8
	return &http.Client{Transport: tr}
}

// New(token) 建客户端。
func New(token string) *Client {
	return &Client{
		Base:    "https://api.haozhuma.com/sms/",
		Token:   token,
		Timeout: 15 * time.Second,
		HTTP:    newHTTPClient(),
	}
}

// SetUID 钉死一个对接码（专属码）。空字符串 = 不钉死，由平台自动分配。
// 注意：配置了轮换池（SetUIDs）时池优先，单 uid 被忽略。
func (c *Client) SetUID(uid string) {
	c.mu.Lock()
	c.uid = strings.TrimSpace(uid)
	c.mu.Unlock()
}

// UID 返回当前钉死的对接码（可能因为失效被自动清空）。
func (c *Client) UID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.uidPool) > 0 {
		return c.uidPool[c.uidIdx%len(c.uidPool)]
	}
	return c.uid
}

// SetUIDs 配置多对接码轮换池。列表为空 = 清空池（退回单 uid 语义）。
// 重复项去重；顺序保留（round-robin 按配置顺序轮）。
func (c *Client) SetUIDs(uids []string) {
	clean := make([]string, 0, len(uids))
	seen := map[string]bool{}
	for _, u := range uids {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		clean = append(clean, u)
	}
	c.mu.Lock()
	c.uidPool = clean
	c.uidIdx = 0
	c.mu.Unlock()
}

// UIDs 返回当前轮换池快照（空 = 未配置池）。
func (c *Client) UIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.uidPool))
	copy(out, c.uidPool)
	return out
}

// TakeDrainedUIDs 返回并清空"已出池待移除"的对接码列表。
// 上层消费后应通过 H5 type=41 从豪猪账户移除（自动生命周期管理的
// "用完移出账户"一步），并从配置里删掉。
func (c *Client) TakeDrainedUIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.drained
	c.drained = nil
	return out
}

// nextPoolUID 取轮换池的下一个码并推进游标。空池返回空串。
func (c *Client) nextPoolUID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.uidPool) == 0 {
		return ""
	}
	u := c.uidPool[c.uidIdx%len(c.uidPool)]
	c.uidIdx++
	return u
}

// dropPoolUID 把码从轮换池移除（「专属码不存在」或连番「没有号」时），
// 记入 drained。返回池是否已空。
func (c *Client) dropPoolUID(uid string) (empty bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.uidPool) > 0 && c.uidIdx >= len(c.uidPool) {
		c.uidIdx = 0 // 防御：删过元素后游标越界
	}
	out := c.uidPool[:0]
	for _, u := range c.uidPool {
		if u != uid {
			out = append(out, u)
		}
	}
	if len(out) != len(c.uidPool) {
		c.drained = append(c.drained, uid)
		c.uidPool = out
		if c.uidIdx >= len(c.uidPool) {
			c.uidIdx = 0
		}
	}
	return len(c.uidPool) == 0
}

// TakeUnknownUID 返回并清空"最近一次因失效被丢弃的对接码"。
// 上层用它提示用户去改配置（配置里的旧值已经没用了）。
func (c *Client) TakeUnknownUID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.lastUnknownUID
	c.lastUnknownUID = ""
	return v
}

// NewWithCredentials 用已有 token 建客户端，同时记住账号密码，
// token 失效时可自动 Relogin。
func NewWithCredentials(token, user, password string) *Client {
	c := New(token)
	c.user = user
	c.pass = password
	return c
}

// NewEcode 建 E码云系客户端（eomsg.com / uomsg.com 同模板）。
// token 在网页版 个人中心 → API对接中心 创建；E码云没有 API 登录接口，
// user/pass 仅为将来兼容保留（Relogin 直接成功——token 本就长期有效）。
func NewEcode(base, token string) *Client {
	if base == "" {
		base = "https://api.eomsg.com/zc/data.php"
	}
	return &Client{
		Base:     base,
		Provider: "ecode",
		Token:    token,
		Timeout:  15 * time.Second,
		HTTP:     newHTTPClient(),
	}
}

// ecodeGet 发一次 E码云请求（纯文本响应）。
// 返回 body 原文；HTTP 非 200 或 "ERROR:" 前缀时包成 APIError。
func (c *Client) ecodeGet(ctx context.Context, params url.Values) (string, error) {
	u, err := url.Parse(c.Base)
	if err != nil {
		return "", err
	}
	q := u.Query()
	for k, vs := range params {
		for _, v := range vs {
			if strings.TrimSpace(v) == "" {
				continue
			}
			q.Add(k, v)
		}
	}
	u.RawQuery = q.Encode()

	ctx2, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx2, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/plain, */*")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// 同豪猪 call：不能让 *url.Error 把含 token 的完整 URL 带进日志。
		return "", fmt.Errorf("网络错误: api=%s: %w", params.Get("code"), redactNetErr(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(body))
	// E码云 token 失效实测同样回 HTTP 403/401（无 JSON）。
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return "", &APIError{API: params.Get("code"), Code: "403",
			Msg: fmt.Sprintf("HTTP %d（token 失效或账号被拒）", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		return "", &APIError{API: params.Get("code"), Code: fmt.Sprintf("%d", resp.StatusCode),
			Msg: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(text, 120))}
	}
	if strings.HasPrefix(text, "ERROR:") {
		msg := strings.TrimSpace(strings.TrimPrefix(text, "ERROR:"))
		return text, &APIError{API: params.Get("code"), Code: "-1", Msg: msg}
	}
	return text, nil
}

// ecodeWaiting E码云的"短信未到"判定：响应含 "[尚未收到]"。
// 对应豪猪 code=-1 msg=等待短信 的轮询正常态。
func ecodeWaiting(text string) bool {
	return strings.Contains(text, "尚未收到")
}

// Login 用账号密码换取 token。
func Login(user, password string) (*Client, error) {
	c := &Client{
		Base:    "https://api.haozhuma.com/sms/",
		Timeout: 15 * time.Second,
		HTTP:    newHTTPClient(),
	}
	raw, err := c.call(context.Background(), "login", url.Values{
		"user": {user}, "pass": {password},
	})
	if err != nil {
		return nil, err
	}
	token, _ := raw["token"].(string)
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("登录成功但未返回 token: %v", c.lastMsg(raw))
	}
	c.Token = token
	c.user = user
	c.pass = password
	return c, nil
}

// Relogin 用保存的账号密码重换 token（token 失效时调用）。
// 没存过凭据（token 直建模式）时返回错误。
// E码云没有 API 登录接口（token 网页版手动创建、长期有效），直接
// 视为成功——真正失效时 Summary 会报 403，由用户去网页版重建。
func (c *Client) Relogin() error {
	if c.Provider == "ecode" {
		return nil
	}
	c.mu.Lock()
	user, pass := c.user, c.pass
	c.mu.Unlock()
	if user == "" || pass == "" {
		return errors.New("token 失效且未保存账号密码，无法重登")
	}
	raw, err := c.call(context.Background(), "login", url.Values{
		"user": {user}, "pass": {pass},
	})
	if err != nil {
		return err
	}
	token, _ := raw["token"].(string)
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("重登成功但未返回 token")
	}
	c.mu.Lock()
	c.Token = token
	c.mu.Unlock()
	return nil
}

type Response map[string]any

func (r Response) Code() string {
	return anyCode(r)
}

func (r Response) Msg() string {
	v, _ := r["msg"].(string)
	return strings.TrimSpace(v)
}

func (r Response) Phone() string {
	v, _ := r["phone"].(string)
	return strings.TrimSpace(v)
}

func (r Response) SMS() string {
	v, _ := r["sms"].(string)
	return v
}

// code 字段有时是数字。统一取出来。
func anyCode(raw map[string]any) string {
	switch v := raw["code"].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return fmt.Sprintf("%g", v)
	default:
		return ""
	}
}

func (c *Client) lastMsg(raw map[string]any) string {
	v, _ := raw["msg"].(string)
	return v
}

// call 发一次请求并校验业务码。
func (c *Client) call(ctx context.Context, api string, params url.Values) (map[string]any, error) {
	u, err := url.Parse(c.Base)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("api", api)
	for k, vs := range params {
		for _, v := range vs {
			if strings.TrimSpace(v) == "" {
				continue
			}
			q.Add(k, v)
		}
	}
	u.RawQuery = q.Encode()

	ctx2, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx2, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// 绝不要把 err 原样往上报：*url.Error 会把完整 URL（含 token）
		// 拼进 Error()，一路带到日志里。
		return nil, fmt.Errorf("网络错误: api=%s: %w", api, redactNetErr(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	// token 失效时豪猪直接回 HTTP 403 且 body 不是 JSON（实测），
	// 这里必须当成"需要重登"而不是解析错误，否则任务会误判成普通失败。
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return nil, &APIError{API: api, Code: "403",
			Msg: fmt.Sprintf("HTTP %d（token 失效或账号被拒）", resp.StatusCode)}
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("响应不是 JSON（HTTP %d）: %s", resp.StatusCode, truncate(string(body), 200))
	}
	code := anyCode(raw)
	if code == "0" || code == "200" {
		return raw, nil
	}
	if code == "-1" && strings.Contains(c.lastMsg(raw), "等待") {
		return raw, nil
	}
	return raw, &APIError{API: api, Code: code, Msg: c.lastMsg(raw), Raw: raw}
}

type APIError struct {
	API  string
	Code string
	Msg  string
	Raw  map[string]any
}

func (e *APIError) Error() string {
	return fmt.Sprintf("豪猪[%s] code=%s msg=%s", e.API, e.Code, e.Msg)
}

// Waiting 报告是否处于"等待短信"状态（轮询场景的正常返回）。
func (e *APIError) Waiting() bool {
	return e.Code == "-1" && strings.Contains(e.Msg, "等待")
}

// Fatal 报告是否为继续重试也无意义的错误：账户余额不足、无号可取、项目
// 不存在、账号被禁。关键词取自实测响应（如错 sid 返回"没有找到项目ID"）。
//
// 注意区分两种"余额不足"：
//   - "您的余额不足,请释放拉黑后再取号" = 手里占用的号到了并发上限，
//     释放后就能继续取 —— 可恢复，不算致命（见 QuotaExhausted）。
//   - "余额不足，请充值" = 账户真没钱了 —— 致命。
func (e *APIError) Fatal() bool {
	if e == nil {
		return false
	}
	if e.QuotaExhausted() {
		return false
	}
	m := e.Msg
	for _, kw := range []string{
		"余额不足", "充值", "欠费",
		"无号", "没有可用", "暂无号码", "号码不足", "库存不足", "已售完",
		"项目不存在", "没有找到项目", "项目已", "项目被",
		"已被禁用", "封禁", "账号异常",
	} {
		if strings.Contains(m, kw) {
			return true
		}
	}
	switch e.Code {
	case "201", "202", "203", "204", "205", "301", "302", "303":
		// 常见豪猪错误段：201x 余额/套餐、30x 项目/账号被禁。
		return true
	}
	return false
}

// QuotaExhausted 报告是否为"占用号数到上限，先释放/拉黑再取"这类可恢复状态。
// 豪猪用「您的余额不足,请释放拉黑后再取号」表达这个意思——措辞有误导性，
// 实际与账户余额无关，是同时占用的号达到了上限。调用方应稍后重试。
func (e *APIError) QuotaExhausted() bool {
	if e == nil {
		return false
	}
	return strings.Contains(e.Msg, "释放") &&
		(strings.Contains(e.Msg, "余额不足") || strings.Contains(e.Msg, "再取号"))
}

// UnknownUID 报告是否为"钉死的对接码不存在"。
//
// 实测文案：「没有这个[52283-XXXX]专属码」。上游增删对接码是常态，
// 配置里钉死的那个随时可能整批消失——调用方应丢掉它并退回自动分配，
// 而不是把这个错误当成致命错误终止任务。
func (e *APIError) UnknownUID() bool {
	if e == nil {
		return false
	}
	return strings.Contains(e.Msg, "专属码") ||
		strings.Contains(e.Msg, "没有这个") ||
		strings.Contains(e.Msg, "对接码不存在")
}

// TokenInvalid 报告 token 是否失效（需要重新 login）。
// 实测 token 失效时豪猪返回 HTTP 403（无 JSON body），此处对应 Code=403。
func (e *APIError) TokenInvalid() bool {
	if e == nil {
		return false
	}
	if e.Code == "403" || e.Code == "401" {
		return true
	}
	if e.Code == "101" {
		return true
	}
	return strings.Contains(e.Msg, "token") || strings.Contains(e.Msg, "登录")
}

// Summary 账户概览：余额 + 平台侧当前占用号码数（getSummary）。
//
// Occupied 来自响应的 num 字段。语义按"当前占用号数"处理（豪猪后台首页
// 的"已占用"同源），-1 表示平台未返回/无法解析——前端按"未知"展示，
// 不要把 -1 当成"0 个占用"。
type Summary struct {
	Balance  float64
	Occupied int
}

// Balance 返回 getSummary 里的余额（元）。解析失败返回 -1。
// 内部走 Summary 后取 Balance 字段，保证与 Summary 的解析逻辑不会漂移。
func (c *Client) Balance(ctx context.Context) (float64, error) {
	s, err := c.Summary(ctx)
	return s.Balance, err
}

func (c *Client) Summary(ctx context.Context) (Summary, error) {
	// E码云：leftAmount 纯文本返回余额数字；占用数平台不提供（自动释放，
	// 无并发额度概念），保持 -1 = 未知。
	if c.Provider == "ecode" {
		text, err := c.ecodeGet(ctx, url.Values{
			"code": {"leftAmount"}, "token": {c.Token},
		})
		if err != nil {
			return Summary{Occupied: -1}, err
		}
		f, perr := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if perr != nil {
			return Summary{Occupied: -1}, fmt.Errorf("余额字段无法解析: %v", truncate(text, 60))
		}
		return Summary{Balance: f, Occupied: -1}, nil
	}
	// 双平台共存：豪猪挂了（维护/网络）但兜底可用时，返回兜底平台余额，
	// 错误信息附带说明——WebUI 概览不至于全红，取号链路也由兜底撑着。
	fb := c.fallbackClient()
	if fb != nil && c.mode() == "ecode" {
		// 仅 E码云模式：只查兜底侧，不碰豪猪（摘掉的意图要尊重）。
		return fb.Summary(ctx)
	}
	s, hzErr := c.summaryHaozhuma(ctx)
	if hzErr != nil {
		if fb != nil && c.mode() != "haozhuma" {
			if fbS, fbErr := fb.Summary(ctx); fbErr == nil {
				return Summary{
					Balance:  fbS.Balance,
					Occupied: -1,
				}, fmt.Errorf("豪猪不可用（%v），当前由 E码云兜底", hzErr)
			}
		}
		return s, hzErr
	}
	// 两边都活着且非"仅豪猪"模式：余额合并展示（豪猪 + E码云），占用取豪猪侧。
	if fb != nil && c.mode() != "haozhuma" {
		if fbS, fbErr := fb.Summary(ctx); fbErr == nil {
			s.Balance += fbS.Balance
		}
	}
	return s, nil
}

// summaryHaozhuma 豪猪侧原版 getSummary（Summary 的主路径）。
func (c *Client) summaryHaozhuma(ctx context.Context) (Summary, error) {
	raw, err := c.call(ctx, "getSummary", url.Values{"token": {c.Token}})
	if err != nil {
		return Summary{}, err
	}
	s := Summary{Occupied: -1}
	switch v := raw["money"].(type) {
	case string:
		f, perr := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if perr != nil {
			return Summary{Occupied: -1}, fmt.Errorf("余额字段无法解析: %v", raw["money"])
		}
		s.Balance = f
	case float64:
		s.Balance = v
	default:
		return Summary{Occupied: -1}, fmt.Errorf("余额字段无法解析: %v", raw["money"])
	}
	// num 可能是数字或数字字符串，两态都接；失败保持 -1（未知）。
	switch v := raw["num"].(type) {
	case float64:
		if v >= 0 {
			s.Occupied = int(v)
		}
	case string:
		if n, perr := strconv.Atoi(strings.TrimSpace(v)); perr == nil && n >= 0 {
			s.Occupied = n
		}
	}
	return s, nil
}

func (r Response) IsWaiting() bool { return false }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// GetPhone 在项目 sid 下取一个号。
//
// 「[限对接]」类项目按对接方放号，官方 SDK 会带 author（默认 adminzfz）；
// 缺了会拿不到号（豪猪提示"建议学会加对接码后再取号"）。
//
// ISP 是运营商**优先级列表**（1=移动 2=联通 3=电信）：逐档尝试，前一档
// 没号就退到下一档，最后自动退回"不限"。实测同一项目不同对接码放的号段
// 不一样（广电/联通/虚拟运营商），挑到能收码的运营商比死等一档更快。
//
// uid 钉死是可选的：对接码会被上游增删，钉死的那个可能整批失效。一旦
// 报"专属码不存在"，就丢掉它退回平台自动分配，而不是让整个任务卡死。
//
// 多码轮换（2026-09）：配置了 SetUIDs 池时，每次调用从池里 round-robin
// 取一个码用；某码报「专属码不存在」自动出池（记入 drained，上层负责
// 从豪猪账户移除），换下一个码继续；池耗尽后退回平台自动分配。
func (c *Client) GetPhone(ctx context.Context, sid string) (string, error) {
	// E码云直连客户端：sid 语义位放 keyWord（短信关键词，如"腾讯科技"）；
	// 无对接码/author/isp 概念，uid 池与 ISP 参数全部忽略。无号时 ERROR:xxx。
	if c.Provider == "ecode" {
		keyWord := strings.TrimSpace(sid)
		params := url.Values{
			"code": {"getPhone"}, "token": {c.Token},
			"keyWord": {keyWord},
			"cardType": {c.cardTypeOr()},
			"province": {strings.TrimSpace(c.Province)},
		}
		phone, err := c.ecodeGet(ctx, params)
		if err != nil {
			return "", err
		}
		phone = strings.TrimSpace(phone)
		if phone == "" {
			return "", errors.New("取号成功但未返回手机号")
		}
		return phone, nil
	}
	// —— 双平台模式分发（主客户端 = 豪猪，Fallback = E码云系）——
	if fb := c.fallbackClient(); fb != nil {
		switch c.mode() {
		case "haozhuma":
			// 仅豪猪：走主链（下方原逻辑），失败不上兜底。
		case "ecode":
			// 仅 E码云：直接走兜底客户端，不碰豪猪。
			phone, err := fb.GetPhone(ctx, sid)
			if err != nil {
				return "", err
			}
			c.rememberSource(phone, "ecode")
			return phone, nil
		case "both":
			// 双平台并行：交替首选，首选失败换边再试一次。
			// 两池同耗，取号吞吐翻倍；失败面（一边挂了）自动单边续跑。
			if c.takePreferAlt() {
				if phone, err := fb.GetPhone(ctx, sid); err == nil {
					c.rememberSource(phone, "ecode")
					return phone, nil
				}
				if phone, err := c.getPhoneHaozhuma(ctx, sid); err == nil {
					return phone, nil
				} else {
					return "", fmt.Errorf("双平台取号均失败（E码云先试）: %w", err)
				}
			}
			if phone, err := c.getPhoneHaozhuma(ctx, sid); err == nil {
				return phone, nil
			}
			if phone, err := fb.GetPhone(ctx, sid); err == nil {
				c.rememberSource(phone, "ecode")
				return phone, nil
			} else {
				return "", fmt.Errorf("双平台取号均失败（豪猪先试）: %w", err)
			}
		default:
			// auto（默认）：豪猪优先，失败切 E码云——原兜底语义。
			if phone, err := c.getPhoneHaozhuma(ctx, sid); err == nil {
				return phone, nil
			} else if fbPhone, fbErr := fb.GetPhone(ctx, sid); fbErr == nil {
				c.rememberSource(fbPhone, "ecode")
				return fbPhone, nil
			} else {
				return "", fmt.Errorf("豪猪取号失败: %v；兜底(E码云)取号也失败: %v", err, fbErr)
			}
		}
	}
	return c.getPhoneHaozhuma(ctx, sid)
}

// mode 当前平台选择模式（并发安全快照）。
func (c *Client) mode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m := strings.TrimSpace(c.Mode); m != "" {
		return m
	}
	return "auto"
}

// takePreferAlt both 轮替：读并翻转"下次先试哪边"游标。
func (c *Client) takePreferAlt() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.altPreferAlt
	c.altPreferAlt = !v
	return v
}

// getPhoneHaozhuma 豪猪原版取号（uid 池/ISP 降级/钉死码回退全逻辑）。
func (c *Client) getPhoneHaozhuma(ctx context.Context, sid string) (string, error) {
	// 轮换池模式：逐码尝试（每码内部仍按 ISP 降级）。
	if len(c.uidsSnapshot()) > 0 {
		var lastErr error
		var poolErr error // 池模式最终失败（触发兜底判定）
		for range c.uidsSnapshot() { // 最多试池大小的次数
			uid := c.nextPoolUID()
			if uid == "" {
				break
			}
			phone, err := c.getPhoneRound(ctx, sid, uid)
			if err == nil {
				return phone, nil
			}
			lastErr = err
			var ae *APIError
			if errors.As(err, &ae) && ae.UnknownUID() {
				c.dropPoolUID(uid)
				continue // 这个码没了，换下一个
			}
			// 其它错误（没号/网络）不逐码轮——池里的码共享项目侧库存，
			// 单码没号不代表其它码没号，但为了不放大请求量，先直接返回。
			poolErr = err
			break
		}
		if poolErr == nil && lastErr != nil {
			// 池试空（全部专属码失效）：退回平台自动分配。
			phone, err := c.getPhoneRound(ctx, sid, "")
			if err == nil {
				return phone, nil
			}
			return "", fmt.Errorf("%w（轮换池 %d 个码已试完，已退回平台自动分配）", err, len(c.uidsSnapshot()))
		}
		if poolErr != nil {
			return "", poolErr
		}
		// 池试空且无错误（nextPoolUID 空）：退回平台自动分配，走下方单码模式。
	}

	// 单码/自动分配模式。
	phone, err := c.getPhoneRound(ctx, sid, c.UID())

	// 钉死的对接码失效：丢掉它，退回平台自动分配再试一轮。
	var ae *APIError
	if err != nil && errors.As(err, &ae) && ae.UnknownUID() && c.UID() != "" {
		bad := c.UID()
		c.SetUID("")
		c.mu.Lock()
		c.lastUnknownUID = bad
		c.mu.Unlock()
		if phone, retryErr := c.getPhoneRound(ctx, sid, ""); retryErr == nil {
			return phone, nil
		} else {
			return "", retryErr
		}
	}
	return phone, err
}

// fallbackClient 返回兜底客户端（无配置返回 nil）。并发安全。
func (c *Client) fallbackClient() *Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Fallback
}

// rememberSource 记下号码来源平台（fallback 收码路由用）。
// 记账上限：超过 256 条（长跑任务累积）直接清空——在途号码必然
// 是最近取的，老的早已收尾，清掉不影响路由。
func (c *Client) rememberSource(phone, provider string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.phoneSource == nil {
		c.phoneSource = map[string]string{}
	}
	if len(c.phoneSource) >= 256 {
		c.phoneSource = map[string]string{}
	}
	c.phoneSource[phone] = provider
}

// sourceOf 查号码来源；未记账返回空串（= 主平台语义）。
func (c *Client) sourceOf(phone string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.phoneSource[phone]
}

// forgetSource 号码收尾后清掉记账。
func (c *Client) forgetSource(phone string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.phoneSource, phone)
}

// uidsSnapshot 池快照（只读）。
func (c *Client) uidsSnapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.uidPool))
	copy(out, c.uidPool)
	return out
}

// cardTypeOr 返回 E码云 cardType 参数；未配置时给"全部"。
func (c *Client) cardTypeOr() string {
	if t := strings.TrimSpace(c.CardType); t != "" {
		return t
	}
	return "全部"
}

// getPhoneRound 跑一轮取号（按 ISP 优先级逐档降级）。uid 为本次用的
// 对接码（空 = 平台自动分配）。
func (c *Client) getPhoneRound(ctx context.Context, sid, uid string) (string, error) {
	base := url.Values{"token": {c.Token}, "sid": {sid}}
	if c.Author != "" {
		base.Set("author", c.Author)
	}
	if uid != "" {
		base.Set("uid", uid)
	}

	var lastErr error
	for _, isp := range c.ispCandidates() {
		params := cloneValues(base)
		if isp != "" {
			params.Set("isp", isp)
		}
		raw, err := c.call(ctx, "getPhone", params)
		if err != nil {
			lastErr = err
			// 无号 = 这一档没货，继续试下一档；其余（余额/项目）直接抛。
			var ae *APIError
			if errors.As(err, &ae) && !ae.Fatal() {
				continue
			}
			return "", err
		}
		phone, _ := raw["phone"].(string)
		phone = strings.TrimSpace(phone)
		if phone == "" {
			lastErr = fmt.Errorf("取号成功但未返回手机号: %v", c.lastMsg(raw))
			continue
		}
		// 平台自动分配时记下实际用的对接码，便于日志/诊断。
		if u, _ := raw["uid"].(string); strings.TrimSpace(u) != "" && uid == "" {
			c.mu.Lock()
			if len(c.uidPool) == 0 {
				c.uid = strings.TrimSpace(u)
			}
			c.mu.Unlock()
		}
		return phone, nil
	}
	if lastErr == nil {
		lastErr = errors.New("取号失败：没有可用号码")
	}
	return "", lastErr
}

// ispCandidates 展开 ISP 配置为优先级列表。空配置返回 [""]（不限运营商）。
func (c *Client) ispCandidates() []string {
	if strings.TrimSpace(c.ISP) == "" {
		return []string{""}
	}
	var out []string
	for _, part := range strings.Split(c.ISP, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{""}
	}
	// 末尾补一个"不限"，保证前几档没号时仍能取到号（总比空手强）。
	return append(out, "")
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

// redactNetErr 压掉网络错误里的完整 URL。*url.Error 的 Error() 形如
//
//	Get "https://...?token=<secret>": context canceled
//
// token 会随日志泄露，所以这里只保留方法、错误原因和主机名。
func redactNetErr(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	host := ""
	if u, perr := url.Parse(ue.URL); perr == nil {
		host = u.Host
	}
	inner := ue.Err
	if inner == nil {
		inner = errors.New("请求失败")
	}
	return fmt.Errorf("%s %s: %w", ue.Op, host, inner)
}

// GetMessage 查一次短信。返回 sms 原文与 code 状态。
// 官方 SDK 还会看 yzm 字段（有的项目把码单独放那里），这里两个都取。
func (c *Client) GetMessage(ctx context.Context, sid, phone string) (string, error) {
	// 双平台共存：号码来自兜底平台时，收码路由到兜底客户端（keyWord
	// 语义），不查豪猪。sid 直接透传——兜底端把它当 keyWord 用。
	if c.sourceOf(phone) == "ecode" {
		if fb := c.fallbackClient(); fb != nil {
			return fb.GetMessage(ctx, sid, phone)
		}
	}
	// E码云：phone + keyWord 查短信；"[尚未收到]" 是正常等待态（上层
	// pollCode 靠 APIError.Waiting() 识别，这里返回原文、空 sms 表示等待）。
	if c.Provider == "ecode" {
		text, err := c.ecodeGet(ctx, url.Values{
			"code": {"getMsg"}, "token": {c.Token},
			"phone": {phone}, "keyWord": {strings.TrimSpace(sid)},
		})
		if err != nil {
			// 等待态在 ecodeGet 里表现为含 [尚未收到] 的非 ERROR 文本；
			// 为与豪猪语义对齐，等待时返回空 sms（不报错）。
			if ecodeWaiting(text) {
				return "", nil
			}
			return "", err
		}
		if ecodeWaiting(text) {
			return "", nil
		}
		return text, nil
	}
	raw, err := c.call(ctx, "getMessage", url.Values{
		"token": {c.Token}, "sid": {sid}, "phone": {phone},
	})
	if err != nil {
		return "", err
	}
	sms, _ := raw["sms"].(string)
	if strings.TrimSpace(sms) == "" {
		// 兜底：yzm 字段（SDK 优先用它）。
		if yzm, _ := raw["yzm"].(string); strings.TrimSpace(yzm) != "" {
			return yzm, nil
		}
	}
	return sms, nil
}

// Release 释放号码（cancelRecv）。E码云为自动释放，no-op。
// 双平台共存：兜底来源的号码（E码云侧自动归还）不碰豪猪。记账保留
// （拉黑收尾时才清）——tryOne 的 finish 是 Blacklist→Release 顺序，
// Release 先来时若无记账，后续 Blacklist 就路由错了。
func (c *Client) Release(ctx context.Context, sid, phone string) error {
	if c.Provider == "ecode" {
		return nil
	}
	if c.sourceOf(phone) == "ecode" {
		return nil
	}
	_, err := c.call(ctx, "cancelRecv", url.Values{
		"token": {c.Token}, "sid": {sid}, "phone": {phone},
	})
	return err
}

// ReleaseAll 一键释放账户名下所有占用中的号码（cancelAllRecv）。
// E码云为自动释放，no-op。
// 只需 token，不需要 sid/phone——用于"额度被旧号占满"时批量归还，
// 或界面上手动兜底（豪猪后台也有同款按钮）。注意它会释放**所有**
// 占用号，不区分是谁取的；任务运行中调用会把在途号码一起放掉，
// 调用方需自行保证没有并发取号。
func (c *Client) ReleaseAll(ctx context.Context) error {
	if c.Provider == "ecode" {
		return nil
	}
	_, err := c.call(ctx, "cancelAllRecv", url.Values{
		"token": {c.Token},
	})
	return err
}

// Blacklist 拉黑号码（收不到码的号）。E码云自动拉黑随机号（指定号不拉），
// no-op——上层 tryOne 的拉黑+释放收尾在 ecode 下天然幂等。
// 双平台共存：兜底来源的号码 E码云侧超时自动拉黑，这里只清记账。
func (c *Client) Blacklist(ctx context.Context, sid, phone string) error {
	if c.Provider == "ecode" {
		return nil
	}
	if c.sourceOf(phone) == "ecode" {
		c.forgetSource(phone)
		return nil
	}
	_, err := c.call(ctx, "addBlacklist", url.Values{
		"token": {c.Token}, "sid": {sid}, "phone": {phone},
	})
	return err
}

var codePatternAlt = regexp.MustCompile(`(\d{4,8})`)

// ExtractCode 从短信文本里抠验证码。腾讯注册短信通常是"验证码xxxxx"。
// 返回第一个 4-8 位数字串（优先"验证码"后面的）。
func ExtractCode(sms string) string {
	sms = strings.TrimSpace(sms)
	if sms == "" {
		return ""
	}
	// 优先 "验证码" 紧跟的数字。
	after := regexp.MustCompile(`验证码[^0-9]{0,6}([0-9]{4,8})`).FindStringSubmatch(sms)
	if len(after) >= 2 {
		return after[1]
	}
	// 兜底：任意独立 4-8 位数字。
	m := codePatternAlt.FindStringSubmatch(sms)
	if len(m) >= 2 {
		return m[1]
	}
	return ""
}
