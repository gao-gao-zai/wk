// Package haozhumah5 adapts the Haozhuma Web v1 API to the enrollment UI.
package haozhumah5

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BaseURL H5 API 基址。变量而非常量：测试里指到 httptest 服务器。
var BaseURL = "https://api.veeto.cn/v1"

// cacheTTL type=30/type=8 结果缓存时长。对接码库存变化快但没必要每次
// 打开选择器都打一次上游；5 分钟内重复查询直接回缓存。
const cacheTTL = 5 * time.Minute

// ErrNoSession Web token 未配置。
var ErrNoSession = errors.New("Web 登录令牌未配置（请在增强设置粘贴 qs_web_token）")

// ErrSessionExpired Web token 已失效（豪猪返回 code:-1 且非业务错误）。
// 上层据此提示重新粘贴，同时退化为手填模式；已存的会话不清空（可能
// 只是临时故障，重试比重配省一步）。
var ErrSessionExpired = errors.New("Web 登录令牌已失效，请重新登录豪猪并粘贴令牌")

// Project is the UI project DTO, populated from explicit Web id/sid/name fields.
type Project struct {
	// SID 16 位 hex 项目会话标识，用于 type=8 查询（不是项目数字 ID）。
	SID string `json:"sid"`
	// ProjectID is the explicit numeric project id returned by the Web API.
	// 取号 API 用的就是这个数字 ID，选择器点选后直接写入 sms.haozhuma.sid。
	ProjectID string `json:"project_id"`
	// Name 项目名称（原样，含【ID】前缀和尾随空格）。
	Name string `json:"name"`
}

// UIDItem is the stable UI model, independent of Web API field names.
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
	LastUsedAt  string   `json:"last_used_at"`
	UpdatedAt   string   `json:"updated_at"` // 上游更新时间（原样）
}

// Client supports Web reads and explicitly requested binding mutations.
//
// 并发安全：所有方法可被多个 goroutine 同时调（HTTP client 本身并发
// 安全，session/缓存/失效标记各自有锁）。
type Client struct {
	HTTP    *http.Client
	Timeout time.Duration

	mu           sync.Mutex
	session      string    // Web token；空 = 未配置
	expired      bool      // 上次请求判定会话失效（不清空 session，重贴时覆盖）
	lastSeen     time.Time // 上次成功调用时间（回显/诊断用）
	lastError    string    // 上次调用错误文案（回显/诊断用）
	persisted    bool
	storageError string
	// The store is fixed at construction; saves are serialized separately from reads.
	tokenStore *tokenStore
	saveMu     sync.Mutex

	// 缓存：key 形如 "30:腾讯" / "8:70da814e38b466b6"。
	cacheMu sync.Mutex
	cache   map[string]cacheEntry
}

type cacheEntry struct {
	at    time.Time
	value any // *[]Project 或 *[]UIDItem
}

// New 建客户端。session 为空串时客户端仍可用，但业务方法返回
// ErrNoSession（用于"已创建但尚未配置"的状态）。
func New(session string) *Client {
	return &Client{
		HTTP:    newHTTPClient(),
		Timeout: 15 * time.Second,
		session: strings.TrimSpace(session),
		cache:   map[string]cacheEntry{},
	}
}

// Session 配置/更新 Web token。空串等于清空（显式语义）。
func (c *Client) Session(session string) {
	c.mu.Lock()
	c.session = strings.TrimSpace(session)
	c.persisted = false
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
	Has          bool      `json:"has"`
	Expired      bool      `json:"expired"`
	LastSeen     time.Time `json:"last_seen"`
	LastError    string    `json:"last_error"`
	Storage      string    `json:"storage"`
	Persisted    bool      `json:"persisted"`
	StorageError string    `json:"storage_error,omitempty"`
}

// Info 返回会话状态摘要。
func (c *Client) Info() SessionInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	storage := "memory"
	if c.tokenStore != nil {
		storage = "encrypted_file"
	}
	return SessionInfo{
		Has:          c.session != "",
		Expired:      c.expired,
		LastSeen:     c.lastSeen,
		LastError:    c.lastError,
		Storage:      storage,
		Persisted:    c.persisted,
		StorageError: c.storageError,
	}
}

// StartKeepalive is retained for callers. No verified token renewal API exists.
func (c *Client) StartKeepalive() {}
func (c *Client) Close()          {}

// Ping validates authentication through a private read-only endpoint, not /ping.
func (c *Client) Ping(ctx context.Context) error {
	var out struct {
		List []json.RawMessage `json:"list"`
	}
	return c.call(ctx, "GET", "/bindings", map[string]string{"page": "1", "size": "1"}, nil, &out)
}

// Projects uses the explicit numeric id, rather than parsing the project name.
func (c *Client) Projects(ctx context.Context, keyword string) ([]Project, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, errors.New("搜索关键词不能为空")
	}
	var data struct {
		List []struct {
			ID   scalar `json:"id"`
			SID  string `json:"sid"`
			Name string `json:"name"`
		} `json:"list"`
	}
	if err := c.call(ctx, "GET", "/projects/search", map[string]string{"keyword": keyword, "size": "50"}, nil, &data); err != nil {
		return nil, err
	}
	out := make([]Project, 0, len(data.List))
	for _, p := range data.List {
		if p.SID != "" {
			out = append(out, Project{SID: p.SID, ProjectID: string(p.ID), Name: p.Name})
		}
	}
	c.store("30:"+keyword, projects(out))
	return out, nil
}

// scalar accepts numeric strings and JSON numbers without silently accepting objects.
type scalar string

func (s *scalar) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	var str string
	if len(b) > 0 && b[0] == '"' {
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
	} else {
		var n json.Number
		if err := json.Unmarshal(b, &n); err != nil {
			return err
		}
		str = n.String()
	}
	*s = scalar(str)
	return nil
}

type webCode struct {
	Code        string   `json:"code"`
	ProjectName string   `json:"projectName"`
	Price       scalar   `json:"price"`
	Available   scalar   `json:"available"`
	Operators   []string `json:"operators"`
	Provinces   []string `json:"provinces"`
	Prefixes    []string `json:"prefixes"`
	SegmentType string   `json:"segmentType"`
	LastUsedAt  string   `json:"lastUsedAt"`
	Status      string   `json:"status"`
}

func (v webCode) item() (UIDItem, error) {
	price, err := strconv.ParseFloat(string(v.Price), 64)
	if err != nil || math.IsNaN(price) || math.IsInf(price, 0) || price < 0 {
		return UIDItem{}, errors.New("豪猪返回的价格格式无效")
	}
	stock := -1
	if v.Available != "" {
		stock, err = strconv.Atoi(string(v.Available))
		if err != nil || stock < 0 {
			return UIDItem{}, errors.New("豪猪返回的库存格式无效")
		}
	}
	return UIDItem{UID: v.Code, Name: v.ProjectName, Price: price, Stock: stock, ISPs: v.Operators, Provinces: v.Provinces, Segments: v.Prefixes, SegmentType: v.SegmentType, LastUsedAt: v.LastUsedAt}, nil
}
func (c *Client) UIDs(ctx context.Context, sid string) ([]UIDItem, error) {
	sid = strings.TrimSpace(sid)
	if sid == "" {
		return nil, errors.New("项目标识不能为空")
	}
	if v, ok := c.cached("8:" + sid); ok {
		return v.(uidItems), nil
	}
	var raw []webCode
	if err := c.call(ctx, "GET", "/projects/"+url.PathEscape(sid)+"/public-codes", nil, nil, &raw); err != nil {
		return nil, err
	}
	// Merge owned codes: public listings may exclude codes already joined.
	mine, err := c.MyUIDs(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make([]UIDItem, 0, len(raw))
	seen := map[string]bool{}
	paused := map[string]bool{}
	for _, v := range mine {
		if v.State == "已暂停" {
			paused[v.UID] = true
		}
	}
	for _, v := range raw {
		if v.Code == "" || paused[v.Code] {
			continue
		}
		it, err := v.item()
		if err != nil {
			return nil, err
		}
		out = append(out, it)
		seen[it.UID] = true
	}
	// Resolve the numeric id so an opaque Web sid never gets compared with code prefixes.
	var projectID string
	if len(raw) > 0 {
		projectID = strings.SplitN(raw[0].Code, "-", 2)[0]
	}
	if projectID == "" {
		for _, p := range c.projectLookup() {
			if p.SID == sid {
				projectID = p.ProjectID
				break
			}
		}
	}
	if projectID == "" {
		if _, err := strconv.Atoi(sid); err == nil {
			projectID = sid
		}
	}
	for _, v := range mine {
		if projectID != "" && strings.HasPrefix(v.UID, projectID+"-") && !seen[v.UID] && v.State != "已暂停" {
			out = append(out, UIDItem{UID: v.UID, Name: v.Name, Price: v.Price, Stock: v.Stock, ISPs: v.ISPs, Provinces: v.Provinces, Segments: v.Segments, SegmentType: v.SegmentType, LastUsedAt: v.LastUsedAt})
			seen[v.UID] = true
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Stock > out[j].Stock })
	c.store("8:"+sid, uidItems(out))
	return out, nil
}
func (c *Client) projectLookup() []Project {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	var out []Project
	for _, v := range c.cache {
		if ps, ok := v.value.(projects); ok {
			out = append(out, ps...)
		}
	}
	return out
}

// RememberProject preserves a configured id/sid pair for owned-only markets.
func (c *Client) RememberProject(sid, id string) {
	c.store("30:configured:"+sid, projects{{SID: sid, ProjectID: id}})
}
func (c *Client) AddUID(ctx context.Context, uid string) error {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return errors.New("对接码不能为空")
	}
	var out struct {
		Results []struct {
			Code string `json:"code"`
			OK   bool   `json:"ok"`
		} `json:"results"`
	}
	if err := c.call(ctx, "POST", "/bindings", nil, map[string]any{"codes": []string{uid}}, &out); err != nil {
		return err
	}
	c.InvalidateMyUIDs()
	c.InvalidateUIDLists()
	for _, r := range out.Results {
		if r.Code == uid && r.OK {
			return nil
		}
	}
	// A duplicate join may be an item failure; verify ownership and pause state.
	mine, err := c.MyUIDs(ctx, "")
	if err == nil {
		for _, v := range mine {
			if v.UID == uid && v.State != "已暂停" {
				return nil
			}
		}
	}
	return errors.New("加入对接码未成功（请检查是否已暂停或达到加入上限）")
}
func (c *Client) RemoveUIDs(ctx context.Context, uids []string) error {
	clean := []string{}
	seen := map[string]bool{}
	for _, u := range uids {
		u = strings.TrimSpace(u)
		if u != "" && !seen[u] {
			clean = append(clean, u)
			seen[u] = true
		}
	}
	if len(clean) == 0 {
		return nil
	}
	var result struct {
		Affected int `json:"affected"`
	}
	if err := c.call(ctx, "POST", "/bindings/batch", nil, map[string]any{"action": "delete", "codes": clean}, &result); err != nil {
		return err
	}
	c.InvalidateMyUIDs()
	c.InvalidateUIDLists()
	mine, err := c.MyUIDs(ctx, "")
	if err != nil {
		return err
	}
	for _, u := range mine {
		if seen[u.UID] {
			return errors.New("删除后仍存在部分对接码")
		}
	}
	return nil
}

type MyUIDItem struct {
	Segments    []string `json:"segments"`
	UID         string   `json:"uid"`
	Name        string   `json:"name"`
	Price       float64  `json:"price"`
	Stock       int      `json:"stock"`
	State       string   `json:"state"`
	ISPs        []string `json:"isps"`
	Provinces   []string `json:"provinces"`
	SegmentType string   `json:"segment_type"`
	LastUsedAt  string   `json:"last_used_at"`
	UpdatedAt   string   `json:"updated_at"`
}

func (c *Client) MyUIDs(ctx context.Context, keyword string) ([]MyUIDItem, error) {
	keyword = strings.TrimSpace(keyword)
	key := "3:" + keyword
	if v, ok := c.cached(key); ok {
		return v.(myUIDItems), nil
	}
	out := make([]MyUIDItem, 0)
	seen := map[string]bool{}
	for page := 1; page <= 1000; page++ {
		var raw struct {
			List  []webCode `json:"list"`
			Total *int      `json:"total"`
		}
		q := map[string]string{"page": strconv.Itoa(page), "size": "20"}
		if keyword != "" {
			q["keyword"] = keyword
		}
		if err := c.call(ctx, "GET", "/bindings", q, nil, &raw); err != nil {
			return nil, err
		}
		if raw.Total == nil || *raw.Total < 0 {
			return nil, errors.New("豪猪列表缺少有效总数")
		}
		added := 0
		for _, v := range raw.List {
			if v.Code == "" || seen[v.Code] {
				continue
			}
			it, err := v.item()
			if err != nil {
				return nil, err
			}
			state := "已对接"
			if v.Status == "paused" {
				state = "已暂停"
			}
			out = append(out, MyUIDItem{UID: it.UID, Name: it.Name, Price: it.Price, Stock: it.Stock, State: state, ISPs: it.ISPs, Provinces: it.Provinces, Segments: it.Segments, SegmentType: it.SegmentType, LastUsedAt: it.LastUsedAt})
			seen[v.Code] = true
			added++
		}
		if len(out) >= *raw.Total {
			c.store(key, myUIDItems(out))
			return out, nil
		}
		if added == 0 {
			return nil, errors.New("豪猪列表分页未取得完整数据")
		}
	}
	return nil, errors.New("豪猪列表超过分页限制")
}

func (c *Client) InvalidateMyUIDs() {
	c.cacheMu.Lock()
	for k := range c.cache {
		if strings.HasPrefix(k, "3:") {
			delete(c.cache, k)
		}
	}
	c.cacheMu.Unlock()
}

// InvalidateUIDLists 清空对接码列表缓存（type=8）。UIDWatcher 每轮拉
// 表前调用：它的间隔（≥60s）可以短于通用缓存 TTL（5 分钟），且监控
// 语义就是"看最新的库存/价格"，旧缓存只会制造假事件。
func (c *Client) InvalidateUIDLists() {
	c.InvalidateMyUIDs()
	c.cacheMu.Lock()
	for k := range c.cache {
		if strings.HasPrefix(k, "8:") {
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

func (c *Client) call(ctx context.Context, method, path string, query map[string]string, payload, out any) error {
	c.mu.Lock()
	token := c.session
	expired := c.expired
	c.mu.Unlock()
	if token == "" {
		return ErrNoSession
	}
	if expired {
		return ErrSessionExpired
	}
	body, err := c.raw(ctx, method, path, query, payload)
	if err != nil {
		c.markError(err)
		return err
	}
	var env struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err = json.Unmarshal(body, &env); err != nil {
		return errors.New("豪猪 Web 响应格式无效")
	}
	if env.Code != nil && *env.Code == 401 {
		c.mu.Lock()
		c.expired = true
		c.mu.Unlock()
		return ErrSessionExpired
	}
	if env.Code == nil || *env.Code != 0 {
		return errors.New("豪猪 Web 请求未成功")
	}
	if out != nil {
		if err = json.Unmarshal(env.Data, out); err != nil {
			return errors.New("豪猪 Web 数据格式无效")
		}
	}
	c.mu.Lock()
	c.lastSeen = time.Now()
	c.lastError = ""
	c.mu.Unlock()
	return nil
}
func (c *Client) raw(ctx context.Context, method, path string, query map[string]string, payload any) ([]byte, error) {
	q := url.Values{}
	for k, v := range query {
		q.Set(k, v)
	}
	u := BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var data []byte
	var err error
	if payload != nil {
		data, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("豪猪 Web 请求地址无效")
	}
	c.mu.Lock()
	token := c.session
	c.mu.Unlock()
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = newHTTPClient()
	}
	copyClient := *hc
	copyClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := copyClient.Do(req)
	if err != nil {
		return nil, errors.New("豪猪 Web 网络请求失败")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		c.mu.Lock()
		c.expired = true
		c.mu.Unlock()
		return nil, ErrSessionExpired
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("豪猪 Web HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(body) > 4<<20 {
		return nil, errors.New("豪猪 Web 响应读取失败或过大")
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
	c.mu.Lock()
	usable := c.session != "" && !c.expired
	c.mu.Unlock()
	if !usable {
		return nil, false
	}
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

// newHTTPClient 与接码 client 同参数：放宽单 host 连接池（选择器打开
// 时会连打两次：项目搜索 + 对接码列表）。
func newHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = 8
	tr.MaxIdleConnsPerHost = 8
	return &http.Client{Transport: tr}
}
