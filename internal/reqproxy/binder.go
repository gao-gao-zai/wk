// Binder：账号→槽位→节点 的分配与迁移（设计文档 3.5）。
//
// 锁纪律：Assign/Bind/DeleteAccount 等热路径在锁内只做纯内存操作；
// 内核调用（AddSlot/RetargetSlot）在锁外由调用方执行。
package reqproxy

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNoNode 候选节点为空（池非空但全部不合格：不健康、超延迟、被规则过滤）。
// D4：调用方必须让请求失败，绝不回退直连。
// 这是池级故障，不是账号的错——调用方不应据此惩罚账号。
var ErrNoNode = errors.New("无可用代理节点")

// Node 节点池里的运行时节点（订阅刷新产物 + 手动导入）。
// 纯数据可值拷贝（快照到处传）；负载计数在 Pool.loads（锁保护），不内嵌
// atomic——避免 noCopy 拷贝违规。
type Node struct {
	NodeSpec
	// 已测速且有数据才参与分配（首轮隔离）
	Probed bool `json:"probed"`
}

// Pool 节点池 + 槽位 + 绑定的内存态（持久化经 Store）。
type Pool struct {
	mu      sync.Mutex
	store   *Store
	nodes   map[string]*Node   // node_id → node
	slots   map[string]*Slot   // slot_id → slot
	bindngs map[string]Binding // uid → binding（store.st.Bindings 的运行时镜像）
	loads   map[string]int64   // node_id → 累计请求数（保留：API/审计用，不再是均衡主信号）
	// 三层滑动窗口（近 6 分钟速率，均衡主信号）：
	//   nodeWin 节点级——替换累计值的历史包袱（昨天热今天闲不再被惩罚）
	//   slotWin 槽位级——同节点槽位间分流（热连接不再挤同一个 inbound）
	//   uidWin  账号级——账号自身的"占位体积"（热账号只能进真空闲的槽）
	nodeWin  map[string]*WinRate
	slotWin  map[string]*WinRate
	uidWin   map[string]*WinRate
	nextPort int
	portMin  int
	portMax  int
	// probePorts 测速临时通道正在占用的端口。它们不在 slots 里（用完即弃），
	// 但必须计入端口占用——否则端口游标耗尽后的空洞回收会把同一个端口
	// 同时发给并发的测速和新建槽位，内核绑定失败。
	probePorts map[int]bool
	// unhealthyCooldown 节点摘除后的复测冷却。零值 = 10 分钟。
	// 来自配置 reqproxy.unhealthy_cooldown（此前 MarkProbed 里写死 10m，配置无效）。
	unhealthyCooldown time.Duration
}

// NewPool 构建池。portMin/portMax 是槽位端口段。
func NewPool(store *Store, portMin, portMax int) *Pool {
	p := &Pool{
		store:      store,
		nodes:      map[string]*Node{},
		slots:      map[string]*Slot{},
		bindngs:    map[string]Binding{},
		loads:      map[string]int64{},
		nodeWin:    map[string]*WinRate{},
		slotWin:    map[string]*WinRate{},
		uidWin:     map[string]*WinRate{},
		nextPort:   portMin,
		portMin:    portMin,
		portMax:    portMax,
		probePorts: map[int]bool{},
	}
	// 从 store 恢复槽位/绑定
	store.View(func(st *State) {
		for i := range st.Slots {
			s := st.Slots[i]
			p.slots[s.ID] = &s
			if s.Port >= p.nextPort && s.Port <= portMax {
				p.nextPort = s.Port + 1
			}
		}
		for uid, b := range st.Bindings {
			p.bindngs[uid] = b
		}
	})
	return p
}

// SetNodes 订阅刷新/导入后整体替换节点集（保留 health 状态）。
// 返回新增、消失、定义变更（同 ID 不同 Raw）三类节点 ID，供 kernel 增量更新。
type NodeDiff struct {
	Added   []NodeSpec
	Removed []string
	Changed []NodeSpec // 定义变更（kernel 需更新 outbound；指向它的槽位同步刷新）
}

func (p *Pool) SetNodes(nodes []NodeSpec) NodeDiff {
	p.mu.Lock()
	defer p.mu.Unlock()
	diff := NodeDiff{}
	seen := map[string]bool{}
	for _, n := range nodes {
		seen[n.ID] = true
		if old, ok := p.nodes[n.ID]; ok {
			if old.Raw != n.Raw {
				old.NodeSpec = n // 定义变更：保留 reqCount/Probed（负载记忆不因刷新清零）
				diff.Changed = append(diff.Changed, n)
			}
			continue
		}
		nn := Node{NodeSpec: n}
		p.nodes[n.ID] = &nn
		diff.Added = append(diff.Added, n)
	}
	for id := range p.nodes {
		if !seen[id] {
			delete(p.nodes, id)
			delete(p.loads, id) // 负载记忆随节点消失清理
			diff.Removed = append(diff.Removed, id)
		}
	}
	return diff
}

// MarkTTFB 记录真实首字测速结果（手动轮与自动扫描共用）。
//
// 成功：写入 TTFBMs、清零 FailStreak、取消 Unhealthy（真实对话通了是
// 最强的健康证据，连通性测速的连败记录应当被覆盖）。
// 节点侧失败（连接不上/超时/上游 5xx）：与 MarkProbed 同一套连续失败
// 计数——满 3 次摘除，冷却相同。
// 账号侧失败由调用方过滤（probeNode 分类后只对节点侧失败调用本函数的
// 计失败路径），不在此区分。
func (p *Pool) MarkTTFB(nodeID string, ttfbMs int64, probeErr string, checkedAt time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := p.healthLocked(nodeID)
	h.TTFBChecked = checkedAt
	if probeErr != "" {
		h.TTFBMs = -1
		h.TTFBError = probeErr
		h.FailStreak++
		if h.FailStreak >= 3 {
			h.Unhealthy = true
			cool := p.unhealthyCooldown
			if cool <= 0 {
				cool = 10 * time.Minute
			}
			h.UnhealthyUntil = time.Now().Add(cool)
		}
		return
	}
	h.TTFBMs = ttfbMs
	h.TTFBError = ""
	h.FailStreak = 0
	h.Unhealthy = false
}

// MarkTTFBSkipped 记录账号侧原因的未测成（额度/刷新/风控）：只更新 TTFB
// 展示字段，不动 FailStreak/Unhealthy——节点没有被定罪。
func (p *Pool) MarkTTFBSkipped(nodeID string, ttfbMs int64, probeErr string, checkedAt time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := p.healthLocked(nodeID)
	h.TTFBMs = ttfbMs
	h.TTFBError = probeErr
	h.TTFBChecked = checkedAt
}

// MarkProbed 测速完成后标记（probed + 延迟/健康）。
func (p *Pool) MarkProbed(nodeID string, latencyMs int64, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := p.healthLocked(nodeID)
	h.CheckedAt = time.Now()
	if err != nil {
		h.FailStreak++
		if h.FailStreak >= 3 {
			h.Unhealthy = true
			cool := p.unhealthyCooldown
			if cool <= 0 {
				cool = 10 * time.Minute
			}
			h.UnhealthyUntil = time.Now().Add(cool)
		}
		return
	}
	h.LatencyMs = latencyMs
	h.FailStreak = 0
	h.Unhealthy = false
	if n := p.nodes[nodeID]; n != nil {
		n.Probed = true
	}
}

func (p *Pool) healthLocked(nodeID string) *NodeHealth {
	h, ok := p.store.st.Health[nodeID]
	if !ok {
		h = &NodeHealth{LatencyMs: -1}
		p.store.st.Health[nodeID] = h
	}
	return h
}

// Candidates 计算 region 的合格节点（设计文档 3.5 的 candidate()）。
func (p *Pool) Candidates(region string) []Node {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.candidatesLocked(region)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// AssignRequest 是 Assign 的返回：需要内核执行的操作。
type AssignResult struct {
	Port int    // 账号应使用的本地端口；0 = 直连（模块关闭/空池降级）
	Node string // 分配说明（事件用）
	Err  error  // 无可用节点（D4）
}

// Assign 为账号分配槽位。返回端口；内核操作由调用方按返回的 kernelOp 执行。
type KernelOp struct {
	Kind     string // "add-slot" | "retarget" | none
	SlotID   string
	Port     int
	NodeSpec NodeSpec // 目标节点（add-slot / retarget）
}

// Assign 惰性分配：查绑定 → 选槽位 → 新建槽位（设计文档 3.5 两层算法）。
// 锁内纯内存；返回的 KernelOp 由 Manager 在锁外执行。
func (p *Pool) Assign(uid, region string) (int, *KernelOp, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 已有绑定 → 不变（稳定性核心）。
	// 但槽位指向的节点必须还在池里：节点被订阅刷新删掉、且当时没有
	// 替代节点可切换时，槽位会悬空指向一个已消失的节点。继续返回它的
	// 端口只会让请求打到死出口（connection refused），而不是给出明确的
	// "无可用代理节点"。节点已消失 → 落到重选。
	if b, ok := p.bindngs[uid]; ok {
		if s := p.slots[b.SlotID]; s != nil && s.NodeID != "" {
			if _, alive := p.nodes[s.NodeID]; alive {
				return s.Port, nil, nil
			}
		}
		// 槽位指向空或指向已消失节点 → 落到重选
	}

	cands := p.candidatesLocked(region)
	if len(cands) == 0 {
		// 空池降级：整个节点池为空时直连（D4 的边界修正）
		if len(p.nodes) == 0 {
			return 0, nil, nil
		}
		return 0, nil, ErrNoNode
	}

	// 槽位选择（流量预算模型，v2）：
	//   剩余预算 = slot_budget_rpm − slotRPM − 节点侧约束 nodeRPM/节点槽数
	//   热账号（uidRPM 高）只能进剩余预算足够的槽；冷账号可填充。
	//   accounts_per_node 退化为硬上限兜底（窗口信号失灵时不至于全挤一个槽）。
	//   窗口全零（冷启动/重启）时自然退化为旧行为，几分钟内信号出现。
	apn := p.store.st.Rules.AccountsPerNode
	budget := p.store.st.Rules.SlotBudgetRPM
	if budget <= 0 {
		budget = 60
	}
	uidRPM := p.uidRPMLocked(uid) // -1 = 新账号无历史
	if uidRPM < 0 {
		uidRPM = float64(budget) / 10 // 冷启动占位：中位数试探，首个窗口后自动修正
	}

	var target *Slot
	var joinable []*Slot
	for _, s := range p.slots {
		if s.Region != region {
			continue
		}
		if s.NodeID == "" {
			continue
		}
		if _, alive := p.nodes[s.NodeID]; !alive {
			continue
		}
		joinable = append(joinable, s)
	}
	// 评分 = 剩余预算（大者优先）；并列时账号占用少者优先；再并列槽位 ID（确定性）
	nodeSlotCount := map[string]int{}
	for _, s := range joinable {
		nodeSlotCount[s.NodeID]++
	}
	slotBudgetLeft := func(s *Slot) float64 {
		nodeShare := 0.0 // 节点侧占用：把节点流量均摊到它的槽位数上
		if c := nodeSlotCount[s.NodeID]; c > 0 {
			nodeShare = p.nodeRPMLocked(s.NodeID) / float64(c)
		}
		return float64(budget) - p.slotRPMLocked(s.ID) - nodeShare
	}
	var bestLeft float64
	for _, s := range joinable {
		if apn > 0 && p.slotCountLocked(s.ID) >= apn {
			continue // 硬上限
		}
		left := slotBudgetLeft(s)
		if left < uidRPM {
			continue // 预算不足以容纳该账号的当前热度
		}
		if target == nil || left > bestLeft ||
			(left == bestLeft && p.slotCountLocked(s.ID) < p.slotCountLocked(target.ID)) ||
			(left == bestLeft && p.slotCountLocked(s.ID) == p.slotCountLocked(target.ID) && s.ID < target.ID) {
			target = s
			bestLeft = left
		}
	}

	if target != nil {
		p.bindngs[uid] = Binding{SlotID: target.ID, Since: time.Now()}
		p.persistLocked()
		return target.Port, nil, nil
	}

	// 新建槽位：选节点（该节点累计请求最少 → 被指向槽位最少 → 延迟最低 → ID）
	node := p.pickNodeLocked(cands)
	slotID := newID("slot")
	port, err := p.allocPortLocked()
	if err != nil {
		return 0, nil, err
	}
	slot := &Slot{ID: slotID, Port: port, NodeID: node.ID, Region: region, Since: time.Now()}
	p.slots[slotID] = slot
	p.bindngs[uid] = Binding{SlotID: slotID, Since: time.Now()}
	p.persistLocked()
	return port, &KernelOp{Kind: "add-slot", SlotID: slotID, Port: port, NodeSpec: node.NodeSpec}, nil
}

// Retarget 槽位换指向（节点故障/被过滤/消失时）。返回需内核执行的操作。
func (p *Pool) Retarget(slotID, region string) (*KernelOp, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.slots[slotID]
	if s == nil {
		return nil, fmt.Errorf("槽位 %s 不存在", slotID)
	}
	cands := p.candidatesLocked(region)
	if len(cands) == 0 {
		return nil, fmt.Errorf("无可用节点可切换")
	}
	node := p.pickNodeLocked(cands)
	old := s.NodeID
	s.NodeID = node.ID
	s.Since = time.Now()
	p.persistLocked()
	_ = old
	return &KernelOp{Kind: "retarget", SlotID: slotID, Port: s.Port, NodeSpec: node.NodeSpec}, nil
}

// UnbindAccount 解绑账号（账号删除或手动解绑）。返回槽位是否变空（可回收）。
func (p *Pool) UnbindAccount(uid string) (slotID string, empty bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, ok := p.bindngs[uid]
	if !ok {
		return "", false
	}
	delete(p.bindngs, uid)
	p.persistLocked()
	if p.slotCountLocked(b.SlotID) == 0 {
		return b.SlotID, true
	}
	return b.SlotID, false
}

// RemoveSlot 删除槽位（空槽回收）。返回其端口（内核据此关 inbound）。
func (p *Pool) RemoveSlot(slotID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.slots[slotID]
	if s == nil {
		return 0
	}
	delete(p.slots, slotID)
	p.persistLocked()
	return s.Port
}

// BindingsSnapshot 导出绑定视图（API 用）。
type BindingView struct {
	UID    string    `json:"uid"`
	SlotID string    `json:"slot_id"`
	Port   int       `json:"port"`
	NodeID string    `json:"node_id"`
	Pinned string    `json:"pinned_node_id,omitempty"`
	Region string    `json:"region"`
	Since  time.Time `json:"since"`
}

func (p *Pool) Views() (slots []Slot, bindings []BindingView) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.slots {
		slots = append(slots, *s)
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].ID < slots[j].ID })
	for uid, b := range p.bindngs {
		v := BindingView{UID: uid, SlotID: b.SlotID, Pinned: b.PinnedNodeID, Since: b.Since}
		if s := p.slots[b.SlotID]; s != nil {
			v.Port = s.Port
			v.NodeID = s.NodeID
			v.Region = s.Region
		}
		bindings = append(bindings, v)
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].UID < bindings[j].UID })
	return
}

// NodesSnapshot 节点池视图（API 用）。
func (p *Pool) NodesSnapshot() []Node {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Node, 0, len(p.nodes))
	for _, n := range p.nodes {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ---- 内部工具（调用方须持锁） ----

func (p *Pool) candidatesLocked(region string) []Node {
	rules := p.store.st.Rules
	now := time.Now()
	var out []Node
	for id, n := range p.nodes {
		// 健康过滤：
		//   - 未测速节点（无 health 记录，或测过但从未成功且未达摘除阈值）
		//     不因延迟被踢——启动窗口期它们是唯一可分配的。
		//   - 但"确认是坏的"（连续失败达阈值、冷却未过）必须排除，哪怕它
		//     从来没测成功过（LatencyMs 一直是 -1）。否则死节点会因为
		//     零流量看起来最闲，在合格节点不够用时被优先分配。
		if h := p.store.st.Health[id]; h != nil {
			if h.Unhealthy && now.Before(h.UnhealthyUntil) {
				continue
			}
			if rules.MaxLatencyMs > 0 && latencyOverLimit(h, rules) {
				continue
			}
		}
		// 首轮隔离的修正：静态筛选（关键词/地区）通过的节点立即可分配。
		// 未测速节点排序靠后（pickNodeLocked 延迟未知按最差处理），
		// 首轮测速后 Retarget 修正落在坏节点上的槽位。
		if matchKeywords(n.Name, rules.ExcludeKeywords) {
			continue
		}
		if len(rules.IncludeKeywords) > 0 && !matchKeywords(n.Name, rules.IncludeKeywords) {
			continue
		}
		if allowed, ok := rules.RegionRules[region]; ok && len(allowed) > 0 {
			if !containsStr(allowed, n.Region) {
				continue
			}
		}
		out = append(out, *n)
	}
	return out
}

func (p *Pool) slotCountLocked(slotID string) int {
	n := 0
	for _, b := range p.bindngs {
		if b.SlotID == slotID {
			n++
		}
	}
	return n
}

func (p *Pool) pointedCountLocked(nodeID string) int {
	n := 0
	for _, s := range p.slots {
		if s.NodeID == nodeID {
			n++
		}
	}
	return n
}

// nodeLoadLocked 节点累计请求数（真实负载）。节点已消失返回 max（不参与均衡）。
func (p *Pool) nodeLoadLocked(nodeID string) int64 {
	if _, ok := p.nodes[nodeID]; ok {
		return p.loads[nodeID]
	}
	return int64(1) << 60
}

// CountRequest 账号请求命中时计数（DialProxy 每次调用）。三层下钻：
// 节点（均衡主信号）/ 槽位（同节点槽位分流）/ 账号（自身热度=占位体积）。
// 无绑定/节点消失时静默忽略。loads 累计值保留仅作审计展示。
func (p *Pool) CountRequest(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, ok := p.bindngs[uid]
	if !ok {
		return
	}
	s := p.slots[b.SlotID]
	if s == nil {
		return
	}
	if _, alive := p.nodes[s.NodeID]; !alive {
		return
	}
	now := WinClock()
	p.loads[s.NodeID]++
	p.nodeWinLocked(s.NodeID).Add(now)
	p.slotWinLocked(s.ID).Add(now)
	p.uidWinLocked(uid).Add(now)
}

// ---- 窗口访问器（调用方须持 Pool.mu）----

func (p *Pool) nodeWinLocked(nodeID string) *WinRate {
	w := p.nodeWin[nodeID]
	if w == nil {
		w = NewWinRate(time.Minute, 6)
		p.nodeWin[nodeID] = w
	}
	return w
}

func (p *Pool) slotWinLocked(slotID string) *WinRate {
	w := p.slotWin[slotID]
	if w == nil {
		w = NewWinRate(time.Minute, 6)
		p.slotWin[slotID] = w
	}
	return w
}

func (p *Pool) uidWinLocked(uid string) *WinRate {
	w := p.uidWin[uid]
	if w == nil {
		w = NewWinRate(time.Minute, 6)
		p.uidWin[uid] = w
	}
	return w
}

// nodeRPM 节点近窗口每分钟速率（持锁调用）。
func (p *Pool) nodeRPMLocked(nodeID string) float64 {
	if _, ok := p.nodes[nodeID]; !ok {
		return 1 << 30 // 节点已消失：按超重处理，不参与均衡
	}
	return p.nodeWinLocked(nodeID).RPM(WinClock())
}

// slotRPM 槽位近窗口每分钟速率（持锁调用）。槽位不存在返回 +Inf 语义的超大值。
func (p *Pool) slotRPMLocked(slotID string) float64 {
	s := p.slots[slotID]
	if s == nil {
		return 1 << 30
	}
	return p.slotWinLocked(slotID).RPM(WinClock())
}

// uidRPM 账号近窗口每分钟速率（持锁调用）。
// 无历史的新账号返回负值（冷启动占位由调用方预算逻辑处理）。
func (p *Pool) uidRPMLocked(uid string) float64 {
	w := p.uidWin[uid]
	if w == nil {
		return -1
	}
	_, rpm := w.Value(WinClock())
	return rpm
}

// SlotRPMView 槽位速率视图（API/调试用，含锁）。
func (p *Pool) SlotRPMView(slotID string) float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.slotRPMLocked(slotID)
}

// NodeRPMView 节点速率视图（API/调试用，含锁）。
func (p *Pool) NodeRPMView(nodeID string) float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.nodeRPMLocked(nodeID)
}

// NodeLoadsSnapshot 各节点累计请求数（负载视图）。
func (p *Pool) NodeLoadsSnapshot() map[string]int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int64, len(p.loads))
	for k, v := range p.loads {
		out[k] = v
	}
	return out
}

func (p *Pool) allocPortLocked() (int, error) {
	used := map[int]bool{}
	for _, s := range p.slots {
		used[s.Port] = true
	}
	for port := range p.probePorts {
		used[port] = true
	}
	for p.nextPort <= p.portMax {
		port := p.nextPort
		p.nextPort++
		if !used[port] {
			return port, nil
		}
	}
	// 回收段内空洞
	for port := p.portMin; port <= p.portMax; port++ {
		if !used[port] {
			return port, nil
		}
	}
	return 0, fmt.Errorf("槽位端口段已耗尽 (%d-%d)", p.portMin, p.portMax)
}

func (p *Pool) persistLocked() {
	// 把运行时槽位/绑定镜像写回 store（Update 自己加锁，这里不能嵌套 —— 直接改 st）
	// 注：Pool.mu 与 Store.mu 的顺序固定为 Pool.mu → Store.mu（仅此处），无死锁环。
	st := p.store.st
	st.Slots = st.Slots[:0]
	for _, s := range p.slots {
		cp := *s
		st.Slots = append(st.Slots, cp)
	}
	sort.Slice(st.Slots, func(i, j int) bool { return st.Slots[i].ID < st.Slots[j].ID })
	st.Bindings = p.bindngs
	p.store.dirty = true
}

func matchKeywords(name string, kws []string) bool {
	for _, kw := range kws {
		if kw != "" && strings.Contains(name, kw) {
			return true
		}
	}
	return false
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// pickNodeLocked 指向选择（须持 Pool.mu）：累计请求最少 → 被指向槽位最少 →
// 延迟最低 → ID 字典序。首因子是真实请求数（负载均衡），账号数只做次级
// 稳健信号，延迟作 tie-break。
// pickNodeLocked 新建槽位时选节点。
// v2 排序：节点近窗口速率（主）→ 被指向槽位数 → 延迟 → ID。
// 窗口速率替换了旧的累计值（历史包袱）；"已有槽位数"仍先于延迟——
// 先在轻节点上开满共享，再开新节点（摊开优先于延迟最优）。
func (p *Pool) pickNodeLocked(cands []Node) Node {
	var best Node
	bestScore := float64(1 << 62)
	for _, n := range cands {
		rpm := float64(1 << 30) // 池里查不到（刚消失）按超重处理
		if _, ok := p.nodes[n.ID]; ok {
			rpm = p.nodeWinLocked(n.ID).RPM(WinClock())
		}
		// 延迟：已知低延迟优先；没测过的垫底但不排除——启动窗口期
		// 全部未测速时仍要选得出节点。用哪份延迟由规则决定。
		lat := int64(1) << 20
		if h := p.store.st.Health[n.ID]; h != nil {
			if v, ok := rankingLatency(h, p.store.st.Rules); ok {
				lat = v
			}
		}
		score := rpm*10_000_000 + float64(p.pointedCountLocked(n.ID))*1_000_000 + float64(lat)
		// tie-break by ID：cands 是 map 迭代序，并列时不稳定会随机选
		if score < bestScore || (score == bestScore && n.ID < best.ID) {
			bestScore = score
			best = n
		}
	}
	return best
}

// latencyOverLimit 当前规则下，节点延迟是否超过上限。
// 连通性模式看 LatencyMs；真实首字模式看 TTFBMs，但只对测出过首字的节点生效
// （没测过的不因延迟被排除，否则切到 ttfb 的瞬间全池都会变成「无可用节点」）。
func latencyOverLimit(h *NodeHealth, rules Rules) bool {
	if rules.LatencySource == "ttfb" {
		return h.TTFBMs > 0 && h.TTFBMs > int64(rules.MaxLatencyMs)
	}
	return h.LatencyMs >= 0 && h.LatencyMs > int64(rules.MaxLatencyMs)
}

// rankingLatency 选节点排序用的延迟。ok=false 表示这份延迟还没测过。
func rankingLatency(h *NodeHealth, rules Rules) (int64, bool) {
	if rules.LatencySource == "ttfb" {
		if h.TTFBMs > 0 {
			return h.TTFBMs, true
		}
		return 0, false
	}
	if h.LatencyMs >= 0 {
		return h.LatencyMs, true
	}
	return 0, false
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// newID 生成带前缀的短 ID。
func newID(prefix string) string {
	return fmt.Sprintf("%s_%s", prefix, randHex(4))
}

// allocProbePort 给临时测速通道分配端口，并登记占用（releaseProbePort 归还）。
// 端口来自槽位段：登记进 probePorts 后，空洞回收不会把正在测速的端口
// 再发给别的测速或新建槽位。
func (p *Pool) allocProbePort() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	port, err := p.allocPortLocked()
	if err != nil {
		return 0, err
	}
	p.probePorts[port] = true
	return port, nil
}

// releaseProbePort 归还测速临时端口。
func (p *Pool) releaseProbePort(port int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.probePorts, port)
}

// SetUnhealthyCooldown 设置节点摘除后的复测冷却（来自配置）。零值 = 默认 10 分钟。
func (p *Pool) SetUnhealthyCooldown(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unhealthyCooldown = d
}

// RollbackAssign 撤销一次新建槽位的分配（内核开槽失败时调用）。
// 只撤这个账号的绑定；槽位上没有其他账号时连槽位记录一起删（端口回收）。
// 返回被删除的槽位 ID（空 = 槽位上还有别的账号，记录保留）。
func (p *Pool) RollbackAssign(uid, slotID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if b, ok := p.bindngs[uid]; ok && b.SlotID == slotID {
		delete(p.bindngs, uid)
	}
	removed := ""
	if p.slotCountLocked(slotID) == 0 {
		if _, ok := p.slots[slotID]; ok {
			delete(p.slots, slotID)
			removed = slotID
		}
	}
	p.persistLocked()
	return removed
}

// StableNodeID 导出稳定 ID 构造（Manager 用）。
func StableNodeID(subID, name string) string {
	return fmt.Sprintf("node_%s_%s", shortHash(subID+"|"+name), sanitize(name))
}
