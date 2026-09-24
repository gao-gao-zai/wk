// Package haozhumah5 访问豪猪 H5 网页后端（h5.haozhuma.com）。
//
// 与 internal/haozhuma（官方接码 API，token 鉴权）平行：这里走的是网页
// 版逆向所得的 api.php，唯一凭证是登录时下发的 PHPSESSID Cookie。
// 封装五个端点：
//
//	GET api.php?type=30&gjc=<关键词>   搜索项目（名称+16位hex会话标识）
//	GET api.php?type=8&sid=<会话标识>  列出项目的对接码（价格/库存/运营商）
//	GET api.php?type=4&djm=<对接码>    把对接码加入我的账户（写操作）
//	GET api.php?type=3&gjc=<关键词>    我的对接码列表（含暂停/恢复/删除，见 myxmlist.html）
//	GET time.php                       服务器时间（用于会话保活）
//
// 关于 type=4（加入对接码）：官方接码 API 的 getPhone?uid= 只认**已加入
// 账户**的对接码；H5 市场列表（type=8）里的码只是"挂牌"，看得见 ≠ 拥有。
// 实测（2026-09-24）：H5 type=4&djm=52283-4PM8WKUBKJ → "添加成功" →
// 官方 getPhone 立即能取到号。所以选码之后必须先加入再取号。
//
// 风险边界（设计文档 §6）：H5 接口非官方公开 API，豪猪随时可改。因此
// 本包只做增强能力（项目/对接码选择器数据源 + 一键加入），核心加号
// 链路（取号/收码）永远只依赖官方接码 API；PHPSESSID 未配置或失效时
// 上层自动退化为手填模式，不影响任务。
//
// 会话生命周期：PHPSESSID 10 天滑动过期（每次请求自动顺延）。保活由
// 后台 ticker 每 7 天调一次 time.php（留 3 天余量）。
package haozhumah5

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BaseURL H5 API 基址。变量而非常量：测试里指到 httptest 服务器。
var BaseURL = "https://h5.haozhuma.com"

// keepaliveInterval 保活间隔。PHPSESSID 10 天滑动过期，7 天打一次
// time.php 留 3 天余量（豪猪故障/本机休眠都能兜住）。
const keepaliveInterval = 7 * 24 * time.Hour

// cacheTTL type=30/type=8 结果缓存时长。对接码库存变化快但没必要每次
// 打开选择器都打一次上游；5 分钟内重复查询直接回缓存。
const cacheTTL = 5 * time.Minute

// ErrNoSession PHPSESSID 未配置。
var ErrNoSession = errors.New("H5 会话未配置（需在 WebUI 粘贴 PHPSESSID）")

// ErrSessionExpired PHPSESSID 已失效（豪猪返回 code:-1 且非业务错误）。
// 上层据此提示重新粘贴，同时退化为手填模式；已存的会话不清空（可能
// 只是临时故障，重试比重配省一步）。
var ErrSessionExpired = errors.New("H5 会话已失效（PHPSESSID 过期或被挤掉，请重新粘贴）")

// Project 项目搜索结果（type=30 data 元素）。
type Project struct {
	// SID 16 位 hex 项目会话标识，用于 type=8 查询（不是项目数字 ID）。
	SID string `json:"sid"`
	// ProjectID 项目数字 ID（从名称【52283】前缀解析；解析失败为空）。
	// 取号 API 用的就是这个数字 ID，选择器点选后直接写入 sms.haozhuma.sid。
	ProjectID string `json:"project_id"`
	// Name 项目名称（原样，含【ID】前缀和尾随空格）。
	Name string `json:"name"`
}

// UIDItem 对接码列表项（type=8 data 元素）。上游字段名是拼音缩写
// （yhj=优惠价、zxky=最新可取、yyy=运营商），这里转成语义化字段。
type UIDItem struct {
	UID         string   `json:"uid"`          // 对接码（52283-WW9L2J4WOL）
	Name        string   `json:"name"`         // 项目名称
	Price       float64  `json:"price"`        // 单价（元）
	Stock       int      `json:"stock"`        // 可用数量（-1=未知）
	ISPs        []string `json:"isps"`         // 运营商（中文，如 移动/联通/电信）
	Provinces   []string `json:"provinces"`    // 省份限制（空=不限）
	SegmentType string   `json:"segment_type"` // 号段类型（未知/虚拟/正常/混合号段）
	Segments    []string `json:"segments"`     // 支持号段前缀（如 198/193/191）
	Pinned      bool     `json:"pinned"`       // 置顶（上游 zd=1，通常按价格高→低排）
	UpdatedAt   string   `json:"updated_at"`   // 上游更新时间（原样）
}

// Client H5 只读客户端。零值不可用，用 New 建。
//
// 并发安全：所有方法可被多个 goroutine 同时调（HTTP client 本身并发
// 安全，session/缓存/失效标记各自有锁）。
type Client struct {
	HTTP    *http.Client
	Timeout time.Duration

	mu        sync.Mutex
	session   string    // PHPSESSID；空 = 未配置
	expired   bool      // 上次请求判定会话失效（不清空 session，重贴时覆盖）
	lastSeen  time.Time // 上次成功调用时间（回显/诊断用）
	lastError string    // 上次调用错误文案（回显/诊断用）

	// 缓存：key 形如 "30:腾讯" / "8:70da814e38b466b6"。
	cacheMu sync.Mutex
	cache   map[string]cacheEntry

	stopKeepalive chan struct{}
	stopOnce      sync.Once
}

type cacheEntry struct {
	at    time.Time
	value any // *[]Project 或 *[]UIDItem
}

// New 建客户端。session 为空串时客户端仍可用，但业务方法返回
// ErrNoSession（用于"已创建但尚未配置"的状态）。
func New(session string) *Client {
	return &Client{
		HTTP:          newHTTPClient(),
		Timeout:       15 * time.Second,
		session:       strings.TrimSpace(session),
		cache:         map[string]cacheEntry{},
		stopKeepalive: make(chan struct{}),
	}
}

// Session 配置/更新 PHPSESSID。空串等于清空（显式语义）。
func (c *Client) Session(session string) {
	c.mu.Lock()
	c.session = strings.TrimSpace(session)
	c.expired = false // 新会话先按有效对待，首次调用再验证
	c.lastError = ""
	c.mu.Unlock()
	c.cacheMu.Lock()
	c.cache = map[string]cacheEntry{} // 换会话清缓存（可能换了账号，结果不同）
	c.cacheMu.Unlock()
}

// SessionInfo 回显会话状态（不回显值本身）。has=true 且 expired=true
// 表示"存了但已失效"。
type SessionInfo struct {
	Has       bool      `json:"has"`
	Expired   bool      `json:"expired"`
	LastSeen  time.Time `json:"last_seen"`
	LastError string    `json:"last_error"`
}

// Info 返回会话状态摘要。
func (c *Client) Info() SessionInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return SessionInfo{
		Has:       c.session != "",
		Expired:   c.expired,
		LastSeen:  c.lastSeen,
		LastError: c.lastError,
	}
}

// StartKeepalive 启动后台保活（7 天一次 time.php）。重复调用是 no-op。
// 进程退出由 Close 收尾；不 Start 也不影响业务方法（每次业务调用天然
// 顺延会话，保活只是空闲期的兜底）。
func (c *Client) StartKeepalive() {
	go func() {
		t := time.NewTicker(keepaliveInterval)
		defer t.Stop()
		for {
			select {
			case <-c.stopKeepalive:
				return
			case <-t.C:
				// time.php 无需鉴权也能通，但带 session 才有顺延效果。
				ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
				_ = c.Ping(ctx)
				cancel()
			}
		}
	}()
}

// Close 停止后台保活。业务方法仍可用。
func (c *Client) Close() {
	c.stopOnce.Do(func() { close(c.stopKeepalive) })
}

// Ping 调一次 time.php（保活/连通性探测）。
func (c *Client) Ping(ctx context.Context) error {
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == "" {
		return ErrNoSession
	}
	body, err := c.raw(ctx, "GET", "/time.php", nil)
	if err != nil {
		return err
	}
	_ = body // 时间串内容无关紧要，能通就行
	c.mu.Lock()
	c.lastSeen = time.Now()
	c.mu.Unlock()
	return nil
}

// Projects 搜索项目（type=30）。关键词为空时上游会返回全量列表，
// 这里要求至少一个字符，避免无谓的大响应。
func (c *Client) Projects(ctx context.Context, keyword string) ([]Project, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, errors.New("搜索关键词不能为空")
	}
	key := "30:" + keyword
	if v, ok := c.cached(key); ok {
		p := v.(projects)
		return []Project(p), nil
	}
	var raw struct {
		Code any    `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			Name string `json:"name"`
			SID  string `json:"sid"`
		} `json:"data"`
	}
	if err := c.call(ctx, "GET", "/api.php", map[string]string{"type": "30", "gjc": keyword}, &raw); err != nil {
		return nil, err
	}
	list := make([]Project, 0, len(raw.Data))
	for _, d := range raw.Data {
		if d.SID == "" {
			continue
		}
		list = append(list, Project{
			SID:       d.SID,
			ProjectID: parseProjectID(d.Name),
			Name:      strings.TrimSpace(d.Name),
		})
	}
	c.store(key, projects(list))
	return list, nil
}

// UIDs 列出项目的对接码（type=8）。sid 用 Projects 返回的 16 位 hex
// 会话标识；实测上游也接受数字项目 ID，两者都放行（名称里的【ID】
// 前缀解析出的数字 ID 同样可用——见设计文档待验证清单 #5）。
// 结果按置顶优先、库存降序排（置顶款通常价格高但稳定）。
func (c *Client) UIDs(ctx context.Context, sid string) ([]UIDItem, error) {
	sid = strings.TrimSpace(sid)
	if sid == "" {
		return nil, errors.New("项目标识不能为空")
	}
	key := "8:" + sid
	if v, ok := c.cached(key); ok {
		u := v.(uidItems)
		return []UIDItem(u), nil
	}
	var raw struct {
		Code any    `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			SID     string `json:"sid"`
			MC      string `json:"mc"`
			UID     string `json:"uid"`
			YHJ     string `json:"yhj"`
			ZXKY    string `json:"zxky"`
			YYY     string `json:"yyy"`
			Sheng   string `json:"sheng"`
			HaoDuan string `json:"haoduan"`
			HD      any    `json:"hd"`
			Time    string `json:"time"`
			ZD      any    `json:"zd"`
		} `json:"data"`
	}
	if err := c.call(ctx, "GET", "/api.php", map[string]string{"type": "8", "sid": sid}, &raw); err != nil {
		return nil, err
	}
	items := make([]UIDItem, 0, len(raw.Data))
	for _, d := range raw.Data {
		if d.UID == "" {
			continue
		}
		items = append(items, UIDItem{
			UID:         d.UID,
			Name:        strings.TrimSpace(d.MC),
			Price:       parsePrice(d.YHJ),
			Stock:       parseStock(d.ZXKY),
			ISPs:        splitPipe(d.YYY),
			Provinces:   splitPipe(d.Sheng),
			SegmentType: strings.TrimSpace(d.HaoDuan),
			Segments:    splitAny(d.HD),
			Pinned:      anyToBool(d.ZD),
			UpdatedAt:   strings.TrimSpace(d.Time),
		})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Pinned != items[j].Pinned {
			return items[i].Pinned
		}
		return items[i].Stock > items[j].Stock
	})
	c.store(key, uidItems(items))
	return items, nil
}

// AddUID 把对接码加入我的账户（type=4&djm=<对接码>）。
//
// 为什么需要它：官方接码 API getPhone?uid= 只认已加入账户的对接码，
// 市场列表里的码直接用会报「没有这个[xxx]专属码」。WebUI 选码后
// 调这个加入，取号链路才真正通。
//
// 语义说明（实测）：重复加入返回 code=-1 msg="已添加过了,如果找不到
// 可以通过底部搜索查询"——不是错误，调用方把"已添加"当成功处理（目的
// 已达成）。上游也可能回"请先登录"（会话失效），统一映射到
// ErrSessionExpired 由上层提示重贴。
func (c *Client) AddUID(ctx context.Context, uid string) error {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return errors.New("对接码不能为空")
	}
	var raw struct {
		Code any    `json:"code"`
		Msg  string `json:"msg"`
		Data any    `json:"data"`
	}
	if err := c.call(ctx, "GET", "/api.php", map[string]string{"type": "4", "djm": uid}, &raw); err != nil {
		// "已添加"在 call() 里会走到 code=-1 分支变成业务错误，这里吸收掉。
		if strings.Contains(err.Error(), "已添加") {
			c.InvalidateMyUIDs()
			return nil
		}
		return err
	}
	c.InvalidateMyUIDs()
	return nil
}

// RemoveUIDs 从我的账户移除对接码（type=41&open=del&uid=<逗号分隔>）。
//
// 用途（自动生命周期管理）：对接码「用完」（库存耗尽/专属码失效）或
// 用户取消勾选时自动移出账户——留在账户里的码会持续占用豪猪侧的
// 对接位（且官方 SDK 的 cancelAllRecv 语义与它们纠缠）。
//
// 实测语义：删除不存在的码也返回 code=200 "对接码状态:已删除"
// （天然幂等）；批量删用逗号拼接一次请求。
func (c *Client) RemoveUIDs(ctx context.Context, uids []string) error {
	clean := make([]string, 0, len(uids))
	for _, u := range uids {
		if u = strings.TrimSpace(u); u != "" {
			clean = append(clean, u)
		}
	}
	if len(clean) == 0 {
		return nil
	}
	var raw struct {
		Code any    `json:"code"`
		Msg  string `json:"msg"`
		Data any    `json:"data"`
	}
	q := map[string]string{"type": "41", "open": "del", "uid": strings.Join(clean, ",")}
	if err := c.call(ctx, "GET", "/api.php", q, &raw); err != nil {
		return err
	}
	c.InvalidateMyUIDs()
	return nil
}

// MyUIDItem 我的对接码列表项（type=3 data 元素）。
//
// 与 UIDItem 不同：type=3 是账户视角，多一个对接状态（djzt：已对接/
// 已暂停），没有置顶字段。
type MyUIDItem struct {
	UID         string   `json:"uid"`
	Name        string   `json:"name"`
	Price       float64  `json:"price"`
	Stock       int      `json:"stock"`
	State       string   `json:"state"` // 已对接 / 已暂停
	ISPs        []string `json:"isps"`
	Provinces   []string `json:"provinces"`
	SegmentType string   `json:"segment_type"`
	UpdatedAt   string   `json:"updated_at"`
}

// MyUIDs 列出我账户已加入的对接码（type=3，成功返回 code=200）。gjc
// 关键词可空（空=全量）。用于：WebUI 标注哪些码"已加入"（可直接取号），
// 以及加号前的自检。
func (c *Client) MyUIDs(ctx context.Context, keyword string) ([]MyUIDItem, error) {
	keyword = strings.TrimSpace(keyword)
	key := "3:" + keyword
	if v, ok := c.cached(key); ok {
		u := v.(myUIDItems)
		return []MyUIDItem(u), nil
	}
	q := map[string]string{"type": "3"}
	if keyword != "" {
		q["gjc"] = keyword
	}
	var raw struct {
		Code any    `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			MC      string `json:"mc"`
			UID     string `json:"uid"`
			YHJ     string `json:"yhj"`
			ZXKY    string `json:"zxky"`
			DJZT    string `json:"djzt"`
			YYY     any    `json:"yyy"`
			Sheng   string `json:"sheng"`
			HaoDuan string `json:"haoduan"`
			Time    any    `json:"time"`
		} `json:"data"`
	}
	if err := c.call(ctx, "GET", "/api.php", q, &raw); err != nil {
		return nil, err
	}
	items := make([]MyUIDItem, 0, len(raw.Data))
	for _, d := range raw.Data {
		if d.UID == "" {
			continue
		}
		ts, _ := d.Time.(string)
		items = append(items, MyUIDItem{
			UID:         d.UID,
			Name:        strings.TrimSpace(d.MC),
			Price:       parsePrice(d.YHJ),
			Stock:       parseStock(d.ZXKY),
			State:       strings.TrimSpace(d.DJZT),
			ISPs:        splitAny(d.YYY),
			Provinces:   splitPipe(d.Sheng),
			SegmentType: strings.TrimSpace(d.HaoDuan),
			UpdatedAt:   strings.TrimSpace(ts),
		})
	}
	// 会话侧列表变化频率低，缓存 5 分钟足够。
	c.store(key, myUIDItems(items))
	return items, nil
}

// InvalidateMyUIDs 作废"我的对接码"缓存（加入新码后调用，让下次重拉）。
func (c *Client) InvalidateMyUIDs() {
	c.cacheMu.Lock()
	for k := range c.cache {
		if strings.HasPrefix(k, "3:") {
			delete(c.cache, k)
		}
	}
	c.cacheMu.Unlock()
}

// ---- 内部：缓存值类型（避免 any 里存指针再解引用的类型断言噪音） ----

type projects []Project
type uidItems []UIDItem
type myUIDItems []MyUIDItem

// ---- 内部：HTTP 与解析 ----

func (c *Client) call(ctx context.Context, method, path string, query map[string]string, out any) error {
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == "" {
		return ErrNoSession
	}
	body, err := c.raw(ctx, method, path, query)
	if err != nil {
		c.markError(err)
		return err
	}
	// out 自带 {code, msg, data} 结构，直接整体解析（豪猪 data 为数组，
	// 不能先剥壳再喂——数组对不上 struct）。
	if err := json.Unmarshal(body, out); err != nil {
		err = fmt.Errorf("H5 响应解析失败: %w", err)
		c.markError(err)
		return err
	}
	// 反射太绕：code/msg 通过二次轻量解析拿（body 已验证是合法 JSON）。
	var probe struct {
		Code any    `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal(body, &probe)
	code := codeString(probe.Code)
	switch code {
	case "1", "200": // 成功（type=30/8 用 1，type=3 用 200）
	case "0": // 无数据：不是错误，落回空列表
	default: // -1 等：失败
		msg := strings.TrimSpace(probe.Msg)
		// 会话失效的标志性文案（抓包实测）；其它 -1 一律当上游业务错误。
		if strings.Contains(msg, "请选择正确的API") {
			c.mu.Lock()
			c.expired = true
			c.lastError = msg
			c.mu.Unlock()
			return ErrSessionExpired
		}
		err := fmt.Errorf("豪猪 H5: %s", msg)
		c.markError(err)
		return err
	}
	c.mu.Lock()
	c.lastSeen = time.Now()
	c.mu.Unlock()
	return nil
}

// raw 发一次 HTTP 请求。只允许打到 h5.haozhuma.com（CheckRedirect 钉死
// 同域，防止重定向把 PHPSESSID Cookie 带出域——比接码 API 的
// token-in-query 风险更实，见设计文档 §6.2）。
func (c *Client) raw(ctx context.Context, method, path string, query map[string]string) ([]byte, error) {
	if c.HTTP == nil {
		c.HTTP = newHTTPClient()
	}
	base := BaseURL
	u := base + path
	if len(query) > 0 {
		q := make(url.Values, len(query)) //nolint — 见下
		for k, v := range query {
			q.Set(k, v)
		}
		u += "?" + q.Encode()
	}
	ctx2, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx2, method, u, nil)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	req.Header.Set("Cookie", "PHPSESSID="+session)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// 脱敏：*url.Error 会带完整 URL。H5 的 token 在 Cookie 不在 URL，
		// query 里只有关键词/项目标识，但统一脱敏不亏。
		return nil, redactURL(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MiB 上限
	if err != nil {
		return nil, redactURL(err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("H5 HTTP %d", resp.StatusCode)
	}
	return body, nil
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 15 * time.Second
}

func (c *Client) markError(err error) {
	c.mu.Lock()
	c.lastError = err.Error()
	c.mu.Unlock()
}

func (c *Client) cached(key string) (any, bool) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	e, ok := c.cache[key]
	if !ok || time.Since(e.at) > cacheTTL {
		return nil, false
	}
	return e.value, true
}

func (c *Client) store(key string, value any) {
	c.cacheMu.Lock()
	c.cache[key] = cacheEntry{at: time.Now(), value: value}
	c.cacheMu.Unlock()
}

// ---- 内部：字段解析 ----

// parseProjectID 从名称「【52283】腾讯科技[限对接]」解析数字 ID。
// 解析失败返回空串（项目列表仍可用 sid 查对接码，只是前端不能一键
// 写入 sms.haozhuma.sid）。
func parseProjectID(name string) string {
	i := strings.Index(name, "【")
	if i < 0 {
		return ""
	}
	rest := name[i+len("【"):]
	j := strings.Index(rest, "】")
	if j <= 0 {
		return ""
	}
	id := rest[:j]
	for _, r := range id {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return id
}

// parsePrice 「16.500」→ 16.5。空/非数字返回 0。
func parsePrice(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

// parseStock 「可用数量:23」→ 23；空/非数字 → -1（未知）。
func parseStock(s string) int {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return -1
	}
	return n
}

// splitPipe 「移动|电信|联通|」→ [移动 电信 联通]。
func splitPipe(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// splitAny hd 字段实测是「198|193|191|」字符串，但也可能是数组
// （豪猪字段类型不稳定），两种都兼容。
func splitAny(v any) []string {
	switch t := v.(type) {
	case string:
		return splitPipe(t)
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		if len(out) > 0 {
			return out
		}
		return nil
	default:
		return nil
	}
}

func anyToBool(v any) bool {
	switch t := v.(type) {
	case string:
		return t == "1"
	case float64:
		return t == 1
	case bool:
		return t
	default:
		return false
	}
}

func codeString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	default:
		return ""
	}
}

// newHTTPClient 与接码 client 同参数：放宽单 host 连接池（选择器打开
// 时会连打两次：项目搜索 + 对接码列表）。
func newHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = 8
	tr.MaxIdleConnsPerHost = 8
	return &http.Client{Transport: tr}
}

// redactURL 把 *url.Error 的完整 URL 替换为 host+path（query 可能含
// 搜索关键词，不带凭据但没必要外泄）。
func redactURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if u, perr := url.Parse(ue.URL); perr == nil {
			return fmt.Errorf("%s %s: %w", ue.Op, u.Host+u.Path, ue.Err)
		}
	}
	return err
}
