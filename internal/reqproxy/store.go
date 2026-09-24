// Package reqproxy 实现面向实际请求账号的代理池（区别于 smslogin 的注册代理）。
//
// 设计文档：docs/proxy-ng-design.md。核心概念：
//   - 节点（node）：订阅或手动导入的代理（share-link / socks5 / http）。
//   - 槽位（slot）：绑定与流量的稳定锚点——一个固定本地端口 + 一条静态路由，
//     当前指向某个节点；换节点只换槽位背后的 outbound，端口/路由/账号绑定不动。
//   - 绑定（binding）：账号 UID → 槽位，落盘持久化。
//
// 本文件定义磁盘状态（state.json）的数据模型与加载/保存。
package reqproxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Userinfo 订阅的流量/到期信息（subscription-userinfo 响应头解析结果）。
type Userinfo struct {
	UploadBytes   int64      `json:"upload_bytes"`
	DownloadBytes int64      `json:"download_bytes"`
	TotalBytes    int64      `json:"total_bytes"`
	ExpireAt      *time.Time `json:"expire_at,omitempty"` // 零值省略 = 订阅未提供
}

// Subscription 一个 v2rayN 风格订阅。
type Subscription struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	URL             string    `json:"url"`
	AutoRefresh     bool      `json:"auto_refresh"`
	Interval        string    `json:"interval"` // Go duration 字符串，默认 "1h"
	LastRefreshedAt time.Time `json:"last_refreshed_at,omitempty"`
	LastStatus      string    `json:"last_status,omitempty"` // ok|error
	LastError       string    `json:"last_error,omitempty"`
	Userinfo        *Userinfo `json:"userinfo,omitempty"`
}

// ManualNode 手动导入的固定代理。
type ManualNode struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Raw       string `json:"raw"`        // 原始 share-link / socks5:// / host:port:user:pass
	RegionTag string `json:"region_tag"` // 可选显式地区（HK/JP/...）；空 = 按名称识别
}

// Rules 筛选规则（全池生效）。
type Rules struct {
	IncludeKeywords []string `json:"include_keywords"` // 任一命中即保留；空 = 不过滤
	ExcludeKeywords []string `json:"exclude_keywords"` // 任一命中即剔除
	MaxLatencyMs    int      `json:"max_latency_ms"`   // 0 = 不检查
	// LatencySource 分配/摘除用哪份延迟："" 或 "connect" = 连通性测速（HEAD），
	// "ttfb" = 真实首字测速。没测过首字的节点在 ttfb 模式下不因延迟被排除。
	LatencySource   string              `json:"latency_source,omitempty"`
	RegionRules     map[string][]string `json:"region_rules"`      // 账号region → 允许的节点地区；空 = 不区分
	AccountsPerNode int                 `json:"accounts_per_node"` // 槽位容量硬上限（防预算信号失灵的兜底）；0 = 不限
	SlotBudgetRPM   int                 `json:"slot_budget_rpm"`   // 槽位流量软预算（每分钟请求数）；0 = 默认 60

	// ---- 自动真实首字测速（TTFBGroup 非空时取代周期连通性 HEAD）----
	// TTFBGroup 用哪个分组的账号打真实对话；空 = 关闭自动首字测速。
	TTFBGroup string `json:"ttfb_group,omitempty"`
	// TTFBIntervalSeconds 两批探测之间的停顿秒数（0 = 不停顿）。默认 60。
	TTFBIntervalSeconds int `json:"ttfb_interval_seconds,omitempty"`
	// TTFBConcurrency 每批同时探测的节点数。默认 1（每拍一个节点，减账号/代理压力）。
	TTFBConcurrency int `json:"ttfb_concurrency,omitempty"`
	// TTFBTimeoutSeconds 单节点探测超时（含等响应头和第一帧）。默认 60。
	TTFBTimeoutSeconds int `json:"ttfb_timeout_seconds,omitempty"`
	// TTFBModel 探测用的模型。空 = glm-5.3。
	TTFBModel string `json:"ttfb_model,omitempty"`
}

// TTFB 扫描参数的合法范围（Manager/handler 校验共用）。
const (
	TTFBIntervalMaxSeconds = 3600
	TTFBConcurrencyMax     = 8
	TTFBTimeoutMaxSeconds  = 180
	TTFBModelMaxLen        = 64
)

// normalizedTTFBRules 把 TTFB 扫描参数收敛到默认值/合法范围（Normalize 和
// API 保存共用：旧 state 缺字段、手写 state 越界都能被收编）。
func (r *Rules) normalizedTTFBRules() {
	if r.TTFBGroup != "" {
		r.TTFBGroup = strings.TrimSpace(r.TTFBGroup)
	}
	if r.TTFBIntervalSeconds <= 0 {
		r.TTFBIntervalSeconds = 60
	}
	if r.TTFBIntervalSeconds > TTFBIntervalMaxSeconds {
		r.TTFBIntervalSeconds = TTFBIntervalMaxSeconds
	}
	if r.TTFBConcurrency <= 0 {
		r.TTFBConcurrency = 1
	}
	if r.TTFBConcurrency > TTFBConcurrencyMax {
		r.TTFBConcurrency = TTFBConcurrencyMax
	}
	if r.TTFBTimeoutSeconds <= 0 {
		r.TTFBTimeoutSeconds = 60
	}
	if r.TTFBTimeoutSeconds > TTFBTimeoutMaxSeconds {
		r.TTFBTimeoutSeconds = TTFBTimeoutMaxSeconds
	}
	if r.TTFBModel != "" {
		r.TTFBModel = strings.TrimSpace(r.TTFBModel)
		if len(r.TTFBModel) > TTFBModelMaxLen {
			r.TTFBModel = r.TTFBModel[:TTFBModelMaxLen]
		}
	}
}

// NormalizeTTFB 导出的收敛入口（server 保存规则时把缺省字段补默认）。
func (r *Rules) NormalizeTTFB() { r.normalizedTTFBRules() }

// ValidateTTFBRules 保存前校验（不修改入参）。返回的第一个错误给 API 400。
func ValidateTTFBRules(r Rules) error {
	switch {
	case r.TTFBIntervalSeconds < 0 || r.TTFBIntervalSeconds > TTFBIntervalMaxSeconds:
		return fmt.Errorf("ttfb_interval_seconds 取值 %d 越界（0–%d）", r.TTFBIntervalSeconds, TTFBIntervalMaxSeconds)
	case r.TTFBConcurrency <= 0 || r.TTFBConcurrency > TTFBConcurrencyMax:
		return fmt.Errorf("ttfb_concurrency 取值 %d 越界（1–%d）", r.TTFBConcurrency, TTFBConcurrencyMax)
	case r.TTFBTimeoutSeconds < 5 || r.TTFBTimeoutSeconds > TTFBTimeoutMaxSeconds:
		return fmt.Errorf("ttfb_timeout_seconds 取值 %d 越界（5–%d）", r.TTFBTimeoutSeconds, TTFBTimeoutMaxSeconds)
	case len(r.TTFBModel) > TTFBModelMaxLen:
		return fmt.Errorf("ttfb_model 过长（≤%d 字符）", TTFBModelMaxLen)
	}
	return nil
}

// Slot 槽位：固定端口 + 当前指向的节点。
type Slot struct {
	ID     string    `json:"id"`
	Port   int       `json:"port"`
	NodeID string    `json:"node_id"` // 空 = 空槽
	Region string    `json:"region"`  // cn|global
	Since  time.Time `json:"since"`
}

// Binding 账号 → 槽位。
type Binding struct {
	SlotID       string    `json:"slot_id"`
	Since        time.Time `json:"since"`
	PinnedNodeID string    `json:"pinned_node_id,omitempty"` // pin 到指定节点；空 = 自动
}

// NodeHealth 测速/摘除状态。
type NodeHealth struct {
	LatencyMs int64     `json:"latency_ms"` // -1 = 未测过
	CheckedAt time.Time `json:"checked_at,omitempty"`
	// TTFBMs 真实首字（毫秒）。-1 = 测过但失败，0 = 从未测过。
	// 与 LatencyMs 分开存：选哪份由 Rules.LatencySource 决定。
	TTFBMs         int64     `json:"ttfb_ms,omitempty"`
	TTFBError      string    `json:"ttfb_error,omitempty"`
	TTFBChecked    time.Time `json:"ttfb_checked_at,omitempty"`
	FailStreak     int       `json:"fail_streak"`
	Unhealthy      bool      `json:"unhealthy"`
	UnhealthyUntil time.Time `json:"unhealthy_until,omitempty"`
}

// State reqproxy 的完整磁盘状态。
type State struct {
	Enabled       bool                   `json:"enabled"`
	Subscriptions []Subscription         `json:"subscriptions"`
	ManualNodes   []ManualNode           `json:"manual_nodes"`
	Rules         Rules                  `json:"rules"`
	Slots         []Slot                 `json:"slots"`
	Bindings      map[string]Binding     `json:"bindings"`
	Health        map[string]*NodeHealth `json:"health"`
}

// DefaultState 返回带默认值的空状态。
func DefaultState() *State {
	return &State{
		Enabled:       false,
		Subscriptions: []Subscription{},
		ManualNodes:   []ManualNode{},
		Rules: Rules{
			IncludeKeywords: []string{},
			ExcludeKeywords: []string{},
			RegionRules:     map[string][]string{},
			AccountsPerNode: 3,
			SlotBudgetRPM:   60,
		},
		Slots:    []Slot{},
		Bindings: map[string]Binding{},
		Health:   map[string]*NodeHealth{},
	}
}

// Normalize 补全空字段（加载旧/部分状态文件后调用）。
func (s *State) Normalize() {
	if s.Subscriptions == nil {
		s.Subscriptions = []Subscription{}
	}
	for i := range s.Subscriptions {
		if s.Subscriptions[i].Interval == "" {
			s.Subscriptions[i].Interval = "1h"
		}
	}
	if s.ManualNodes == nil {
		s.ManualNodes = []ManualNode{}
	}
	if s.Rules.IncludeKeywords == nil {
		s.Rules.IncludeKeywords = []string{}
	}
	if s.Rules.ExcludeKeywords == nil {
		s.Rules.ExcludeKeywords = []string{}
	}
	if s.Rules.RegionRules == nil {
		s.Rules.RegionRules = map[string][]string{}
	}
	if s.Slots == nil {
		s.Slots = []Slot{}
	}
	if s.Bindings == nil {
		s.Bindings = map[string]Binding{}
	}
	if s.Health == nil {
		s.Health = map[string]*NodeHealth{}
	}
	if s.Rules.SlotBudgetRPM <= 0 {
		s.Rules.SlotBudgetRPM = 60
	}
	if s.Rules.LatencySource != "ttfb" {
		s.Rules.LatencySource = "connect"
	}
	s.Rules.normalizedTTFBRules()
}

// Store 状态持久化：内存态 + 周期落盘（沿用项目 state.json 的模式）。
type Store struct {
	mu    sync.Mutex
	fp    string
	st    *State
	dirty bool
	stop  chan struct{}
	done  chan struct{}
}

// NewStore 加载（或初始化）状态文件并启动周期 flusher。
// fp 为空时纯内存（测试用）。
func NewStore(fp string) (*Store, error) {
	s := &Store{
		fp:   fp,
		st:   DefaultState(),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if fp != "" {
		data, err := os.ReadFile(fp)
		switch {
		case err == nil:
			if err := json.Unmarshal(data, s.st); err != nil {
				return nil, fmt.Errorf("解析 %s: %w", fp, err)
			}
			s.st.Normalize()
		case os.IsNotExist(err):
			// 首次启动：保持默认状态，落盘交给首次 flush。
		default:
			return nil, fmt.Errorf("读取 %s: %w", fp, err)
		}
	}
	go s.flusher()
	return s, nil
}

// flusher 周期落盘；进程退出由 Close 兜底。
func (s *Store) flusher() {
	defer close(s.done)
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.Flush()
		case <-s.stop:
			return
		}
	}
}

// Flush 立即落盘（若有变更）。
func (s *Store) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty || s.fp == "" {
		return
	}
	s.st.Normalize()
	data, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return // 状态结构必可序列化；失败静默保内存
	}
	if err := writeFileAtomic(s.fp, data, 0o600); err != nil {
		return
	}
	s.dirty = false
}

// Close 停止 flusher 并做最后落盘。
func (s *Store) Close() error {
	close(s.stop)
	<-s.done
	s.Flush()
	return nil
}

// Update 在锁内读取/修改状态；回调返回后标记 dirty 并（可选）立即落盘。
// 回调内不得做 IO——与 Binder 的锁纪律一致。
func (s *Store) Update(fn func(st *State)) {
	s.mu.Lock()
	fn(s.st)
	s.dirty = true
	s.mu.Unlock()
}

// View 在锁内只读访问状态快照。
func (s *Store) View(fn func(st *State)) {
	s.mu.Lock()
	fn(s.st)
	s.mu.Unlock()
}

// writeFileAtomic 临时文件 + rename 原子写（对齐 auth.SaveAtomic 的做法）。
func writeFileAtomic(fp string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(fp)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(fp)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, perm); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, fp)
}
