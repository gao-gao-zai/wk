// Binder：账号→槽位→节点 的分配与迁移（设计文档 3.5）。
//
// 锁纪律：Assign/Bind/DeleteAccount 等热路径在锁内只做纯内存操作；
// 内核调用（AddSlot/RetargetSlot）在锁外由调用方执行。
package reqproxy

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

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
	nodes   map[string]*Node          // node_id → node
	slots   map[string]*Slot           // slot_id → slot
	bindngs map[string]Binding         // uid → binding（store.st.Bindings 的运行时镜像）
	loads   map[string]int64           // node_id → 累计请求数（真实负载，均衡依据）
	nextPort int
	portMin  int
	portMax  int
}

// NewPool 构建池。portMin/portMax 是槽位端口段。
func NewPool(store *Store, portMin, portMax int) *Pool {
	p := &Pool{
		store:   store,
		nodes:   map[string]*Node{},
		slots:   map[string]*Slot{},
		bindngs: map[string]Binding{},
		loads:   map[string]int64{},
		nextPort: portMin,
		portMin:  portMin,
		portMax:  portMax,
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
	Added    []NodeSpec
	Removed  []string
	Changed  []NodeSpec // 定义变更（kernel 需更新 outbound；指向它的槽位同步刷新）
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
			h.UnhealthyUntil = time.Now().Add(10 * time.Minute)
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
	Port    int    // 账号应使用的本地端口；0 = 直连（模块关闭/空池降级）
	Node    string // 分配说明（事件用）
	Err     error  // 无可用节点（D4）
}

// Assign 为账号分配槽位。返回端口；内核操作由调用方按返回的 kernelOp 执行。
type KernelOp struct {
	Kind     string    // "add-slot" | "retarget" | none
	SlotID   string
	Port     int
	NodeSpec NodeSpec // 目标节点（add-slot / retarget）
}

// Assign 惰性分配：查绑定 → 选槽位 → 新建槽位（设计文档 3.5 两层算法）。
// 锁内纯内存；返回的 KernelOp 由 Manager 在锁外执行。
func (p *Pool) Assign(uid, region string) (int, *KernelOp, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 已有绑定 → 不变（稳定性核心）
	if b, ok := p.bindngs[uid]; ok {
		if s := p.slots[b.SlotID]; s != nil && s.NodeID != "" {
			return s.Port, nil, nil
		}
		// 槽位指向空（节点消失未处理）→ 落到重选
	}

	cands := p.candidatesLocked(region)
	if len(cands) == 0 {
		// 空池降级：整个节点池为空时直连（D4 的边界修正）
		if len(p.nodes) == 0 {
			return 0, nil, nil
		}
		return 0, nil, fmt.Errorf("无可用代理节点")
	}

	// 槽位选择（负载均衡）：同 region 有未满槽位 → 并入"节点累计请求最少"的。
	// 依据真实请求数而不是账号数——冷热账号差一个量级，账号数会骗人。
	// 排序 tie-break：请求数并列时按槽位 ID——否则启动窗口期全是 0 并列，
	// map 随机序会把账号随机散到各槽（线上 120 账号散出 69 个槽的教训）。
	apn := p.store.st.Rules.AccountsPerNode
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
	sort.Slice(joinable, func(i, j int) bool {
		li, lj := p.nodeLoadLocked(joinable[i].NodeID), p.nodeLoadLocked(joinable[j].NodeID)
		if li != lj {
			return li < lj
		}
		return joinable[i].ID < joinable[j].ID
	})
	for _, s := range joinable {
		if apn <= 0 || p.slotCountLocked(s.ID) < apn {
			target = s
			break
		}
	}

	if target != nil {
		p.bindngs[uid] = Binding{SlotID: target.ID, Since: time.Now()}
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
	UID     string    `json:"uid"`
	SlotID  string    `json:"slot_id"`
	Port    int       `json:"port"`
	NodeID  string    `json:"node_id"`
	Pinned  string    `json:"pinned_node_id,omitempty"`
	Region  string    `json:"region"`
	Since   time.Time `json:"since"`
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
		// 健康过滤只对"有数据"的节点生效：未测速节点不因延迟/摘除被踢——
		// 启动窗口期（首轮测速前）它们是唯一可分配的。
		if h := p.store.st.Health[id]; h != nil && h.LatencyMs >= 0 {
			if h.Unhealthy && now.Before(h.UnhealthyUntil) {
				continue
			}
			if rules.MaxLatencyMs > 0 && h.LatencyMs > int64(rules.MaxLatencyMs) {
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

// CountRequest 账号请求命中节点时计数（DialProxy 调用；真实负载信号）。
// 无绑定/节点消失时静默忽略。
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
	if _, alive := p.nodes[s.NodeID]; alive {
		p.loads[s.NodeID]++
	}
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
func (p *Pool) pickNodeLocked(cands []Node) Node {
	var best Node
	bestScore := int64(1) << 62
	for _, n := range cands {
		load := int64(1) << 40 // 池里查不到（刚消失）按超重处理
		if _, ok := p.nodes[n.ID]; ok {
			load = p.loads[n.ID]
		}
		// 延迟：已知低延迟优先；未测速（-1）垫底但不排除——启动窗口期
		// 全部未测速时仍要选得出节点。
		lat := int64(1) << 20 // 未测速按"最差已知延迟"处理
		if h := p.store.st.Health[n.ID]; h != nil && h.LatencyMs >= 0 {
			lat = h.LatencyMs
		}
		score := load*1_000_000_000 + int64(p.pointedCountLocked(n.ID))*10_000_000 + lat
		// tie-break by ID：cands 是 map 迭代序，并列时不稳定会随机选
		if score < bestScore || (score == bestScore && n.ID < best.ID) {
			bestScore = score
			best = n
		}
	}
	return best
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

// allocProbePort 给临时测速通道分配端口（槽位段之外临时占用，用后即弃）。
func (p *Pool) allocProbePort() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.allocPortLocked()
}

// StableNodeID 导出稳定 ID 构造（Manager 用）。
func StableNodeID(subID, name string) string {
	return fmt.Sprintf("node_%s_%s", shortHash(subID+"|"+name), sanitize(name))
}
