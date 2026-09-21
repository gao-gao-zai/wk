// Manager：reqproxy 的组装层——把 Store/Pool/Kernel/Health/订阅刷新粘在一起，
// 对外提供 DialProxy(uid, region) 与管理 API 的实现。
package reqproxy

import (
	"fmt"
	"net/url"
	"sync"
	"time"
)

// Config reqproxy 模块配置（config.json 的 reqproxy 块）。
type Config struct {
	StateFile          string        // 默认 ./data/reqproxy/state.json
	HealthInterval     time.Duration // 默认 15m
	LatencyTimeout     time.Duration // 默认 5s
	UnhealthyCooldown  time.Duration // 默认 10m
	PortMin            int           // 槽位端口段，默认 31080
	PortMax            int           // 默认 31999
}

// DefaultConfig 默认配置。
func DefaultConfig() Config {
	return Config{
		StateFile:         "./data/reqproxy/state.json",
		HealthInterval:    15 * time.Minute,
		LatencyTimeout:    5 * time.Second,
		UnhealthyCooldown: 10 * time.Minute,
		PortMin:           31080,
		PortMax:           31999,
	}
}

// Event 事件流条目（环形缓冲）。
type Event struct {
	ID   int64     `json:"id"`
	At   time.Time `json:"at"`
	Kind string    `json:"kind"` // slot-switch|node-removed|sub-refresh|warn|info
	Msg  string    `json:"msg"`
}

// Manager 模块门面。
type Manager struct {
	cfg    Config
	store  *Store
	pool   *Pool
	kernel *Kernel
	health *Health

	// 事件环形缓冲
	evMu   sync.Mutex
	events []Event
	evNext int64

	// 订阅刷新循环
	subStop chan struct{}
	subDone chan struct{}

	// 账号清单（预热用；main 注入，nil = 关闭预热）
	acctSrc AccountSource

	// 内核热替换期间的槽位出站保护
	kernMu sync.Mutex
}

// NewManager 构建并启动模块。kernel 为 nil 时纯逻辑模式（测试）。
func NewManager(cfg Config, kernel *Kernel) (*Manager, error) {
	if cfg.StateFile == "" {
		cfg = DefaultConfig()
	}
	store, err := NewStore(cfg.StateFile)
	if err != nil {
		return nil, err
	}
	m := &Manager{
		cfg:     cfg,
		store:   store,
		kernel:  kernel,
		subStop: make(chan struct{}),
		subDone: make(chan struct{}),
	}
	m.pool = NewPool(store, cfg.PortMin, cfg.PortMax)

	hcfg := HealthConfig{
		Interval:          cfg.HealthInterval,
		Timeout:           cfg.LatencyTimeout,
		UnhealthyCooldown: cfg.UnhealthyCooldown,
		RegionTargets: map[string]string{
			"cn":     "https://copilot.tencent.com",
			"global": "https://www.workbuddy.ai",
		},
	}
	m.health = NewHealth(hcfg, m.pool, m)

	// 启动时的槽位内核恢复延后到订阅刷新完成（节点池就绪后才有 outbound
	// 可接），见下面的启动 goroutine。
	// 每轮测速结束后：修正坏节点槽位 → 预热未绑定账号 → 负载再平衡
	// （修正优先：先清掉押错的，预热/均衡再基于干净状态）
	m.health.SetAfterRound(func() {
		m.fixupUnhealthySlots()
		m.PrewarmIfDue()
		m.Rebalance()
	})
	m.health.Start()
	go m.subscriptionLoop()
	// 启动恢复：订阅的节点池是内存态（state.json 不存节点定义），
	// 启动时刷新全部订阅拉回节点，然后恢复槽位指向 + 预热。
	// 顺序：refresh（节点入池）→ restoreSlots（内核接回）→ prewarm。
	// 注意：先拷出订阅 ID 再刷新——RefreshSubscription 内部要拿 Store.mu，
	// 不能在 View 回调（已持锁）里调用（自死锁，线上抓过）。
	go func() {
		var subIDs []string
		m.store.View(func(st *State) {
			for _, sub := range st.Subscriptions {
				subIDs = append(subIDs, sub.ID)
			}
		})
		// 无条件先重建一次池：manual 节点不依赖任何订阅。
		// 否则"无订阅/订阅全挂"时 manual 节点不进池——WebUI 节点列表
		// 为空，看起来像手动节点被自动删掉。
		m.rebuildPool()
		for _, id := range subIDs {
			if err := m.RefreshSubscription(id); err != nil {
				// 刷新失败（网络/订阅失效）：槽位保持原指向，事件告警
			}
		}
		if m.kernel != nil {
			m.restoreSlots()
		}
		// 等 main 注入 acctSrc（SetAccounts 在 NewManager 之后调用）
		time.Sleep(2 * time.Second)
		m.PrewarmIfDue()
	}()
	return m, nil
}

// Close 停止全部后台任务。
func (m *Manager) Close() {
	close(m.subStop)
	<-m.subDone
	m.health.Stop()
	if m.kernel != nil {
		m.kernel.Close()
	}
	m.store.Close()
}

// DialProxy upstream 钩子：返回该账号应走的本地代理端口。
// (nil, nil) = 直连（模块关闭/空池降级）；err = 无可用节点（D4）。
func (m *Manager) DialProxy(uid, region string) (*url.URL, error) {
	enabled := false
	m.store.View(func(st *State) { enabled = st.Enabled })
	if !enabled {
		return nil, nil
	}

	port, op, err := m.pool.Assign(uid, region)
	if err != nil {
		return nil, err
	}
	if port == 0 {
		return nil, nil // 空池降级直连
	}
	// 新建槽位 → 内核开槽（锁外）
	if op != nil && m.kernel != nil {
		if err := m.applyKernelOp(*op); err != nil {
			return nil, fmt.Errorf("内核开槽失败: %w", err)
		}
		m.emit("info", fmt.Sprintf("账号 %s 新建槽位 %s (port %d) 指向节点 %s", uid, op.SlotID, op.Port, op.NodeSpec.Name))
	}
	// 负载计数：真实请求数是均衡的依据（账号数会骗人）
	m.pool.CountRequest(uid)
	return mustParseURL(fmt.Sprintf("http://127.0.0.1:%d", port)), nil
}

// applyKernelOp 执行内核操作（add-slot / retarget）。
func (m *Manager) applyKernelOp(op KernelOp) error {
	m.kernMu.Lock()
	defer m.kernMu.Unlock()
	switch op.Kind {
	case "add-slot":
		return m.kernel.AddSlot(op.SlotID, op.Port, op.NodeSpec)
	case "retarget":
		return m.kernel.RetargetSlot(op.SlotID, op.NodeSpec)
	default:
		return fmt.Errorf("未知内核操作 %q", op.Kind)
	}
}

// restoreSlots 恢复内核槽位：对 state 里的每个槽位，其节点已在池里但内核
// 还没这个槽位时 AddSlot。幂等（已存在的先 Remove 再 Add），可安全重入——
// 订阅刷新成功后、周期兜底时都再调一次，保证节点池就绪后槽位最终被接上。
func (m *Manager) restoreSlots() {
	slots, _ := m.pool.Views()
	nodes := indexNodes(m.pool.NodesSnapshot())
	// 内核操作统一走 kernMu（与 applyNodeDiff/fixup 的锁序一致：kernMu → kernel.mu）
	m.kernMu.Lock()
	defer m.kernMu.Unlock()
	for _, s := range slots {
		if s.NodeID == "" {
			continue
		}
		node, ok := nodes[s.NodeID]
		if !ok {
			continue // 节点已消失，等订阅刷新/健康检查处理
		}
		if m.kernel.HasSlot(s.ID) {
			continue // 内核里已有（正常路径），不动
		}
		if err := m.kernel.AddSlot(s.ID, s.Port, node.NodeSpec); err != nil {
			m.emit("warn", fmt.Sprintf("恢复槽位 %s 失败: %v", s.ID, err))
		}
	}
}

// RefreshSubscription 手动/定时刷新一个订阅。
// 返回节点增减摘要。sourceID = 订阅 ID。
func (m *Manager) RefreshSubscription(subID string) error {
	var sub *Subscription
	m.store.View(func(st *State) {
		for i := range st.Subscriptions {
			if st.Subscriptions[i].ID == subID {
				cp := st.Subscriptions[i]
				sub = &cp
			}
		}
	})
	if sub == nil {
		return fmt.Errorf("订阅 %s 不存在", subID)
	}
	res, err := FetchSubscription(sub.URL)
	if err != nil {
		m.store.Update(func(st *State) {
			for i := range st.Subscriptions {
				if st.Subscriptions[i].ID == subID {
					st.Subscriptions[i].LastStatus = "error"
					st.Subscriptions[i].LastError = err.Error()
					st.Subscriptions[i].LastRefreshedAt = time.Now()
				}
			}
		})
		m.emit("warn", fmt.Sprintf("订阅 %s 刷新失败: %v", sub.Name, err))
		// 刷新失败也要重建池：manual 节点和其他订阅（含本订阅上一轮）的
		// 节点不依赖本次拉取。否则订阅一挂，手动导入的节点跟着从池里
		// "消失"（WebUI 节点列表只显示池快照，看起来像被自动删掉）。
		m.rebuildPool()
		return err
	}
	// 打上订阅源标记
	for i := range res.Nodes {
		res.Nodes[i].Source = subID
		res.Nodes[i].ID = StableNodeID(subID, res.Nodes[i].Name)
	}
	// 合并：手动节点 + 该订阅节点 + 其他订阅节点（SetNodes 全量替换，先收集现有其他来源）
	var all []NodeSpec
	m.store.View(func(st *State) {
		for _, mn := range st.ManualNodes {
			spec, err := ParseLink(mn.Raw)
			if err != nil || spec == nil {
				continue
			}
			spec.ID = mn.ID
			spec.Name = mn.Name
			if mn.RegionTag != "" {
				spec.Region = mn.RegionTag
			}
			spec.Source = "manual"
			all = append(all, *spec)
		}
	})
	for _, existing := range m.pool.NodesSnapshot() {
		if existing.Source != subID {
			all = append(all, existing.NodeSpec)
		}
	}
	all = append(all, res.Nodes...)
	diff := m.pool.SetNodes(all)
	m.applyNodeDiff(diff)
	m.store.Update(func(st *State) {
		for i := range st.Subscriptions {
			if st.Subscriptions[i].ID == subID {
				st.Subscriptions[i].LastStatus = "ok"
				st.Subscriptions[i].LastError = ""
				st.Subscriptions[i].LastRefreshedAt = time.Now()
				if res.Userinfo != nil {
					st.Subscriptions[i].Userinfo = res.Userinfo
				}
			}
		}
	})
	msg := fmt.Sprintf("订阅 %s 刷新: +%d -%d ~%d", sub.Name, len(diff.Added), len(diff.Removed), len(diff.Changed))
	for _, w := range res.Warnings {
		msg += "；" + w
	}
	m.emit("sub-refresh", msg)
	// 刷新后：恢复悬空槽位（节点刚入池）→ 预热未绑定账号。
	// 悬空槽位 = state 里有槽位但内核里没有（启动时订阅还没刷成功的窗口）。
	if m.kernel != nil {
		m.restoreSlots()
	}
	// 同步预热：任务（动画）结束时刷新+预热必须都已落地。
	// 旧版 go 异步预热会在动画停止后才生效，造成"完成了但槽位还没建"。
	m.PrewarmIfDue()
	return nil
}

// applyNodeDiff 把节点池 diff 应用到内核。
func (m *Manager) applyNodeDiff(diff NodeDiff) {
	if m.kernel == nil {
		return
	}
	m.kernMu.Lock()
	defer m.kernMu.Unlock()
	for _, n := range diff.Added {
		if err := m.kernel.AddNodeOutbound(n); err != nil {
			// 节点定义无法生成出站（如端口/参数类型错误）：告警而不是静默吞掉，
			// 否则节点留在池里但内核无出站，测速必然失败且无从排查。
			m.emit("warn", fmt.Sprintf("节点 %s 添加出站失败: %v", n.Name, err))
		}
	}
	for _, id := range diff.Removed {
		_ = m.kernel.RemoveNodeOutbound(id)
		// 指向它的槽位换指向
		slots, _ := m.pool.Views()
		for _, s := range slots {
			if s.NodeID == id {
				if op, err := m.pool.Retarget(s.ID, s.Region); err == nil && op != nil {
					if err := m.kernel.RetargetSlot(op.SlotID, op.NodeSpec); err == nil {
						m.emit("slot-switch", fmt.Sprintf("槽位 %s 从节点 %s 切换（节点消失）", s.ID, id))
					}
				}
			}
		}
	}
	for _, n := range diff.Changed {
		// 定义变更：更新节点 outbound + 指向它的槽位 outbound（指向不变）
		_ = m.kernel.RemoveNodeOutbound(n.ID)
		if err := m.kernel.AddNodeOutbound(n); err != nil {
			m.emit("warn", fmt.Sprintf("节点 %s 更新出站失败: %v", n.Name, err))
		}
		slots, _ := m.pool.Views()
		for _, s := range slots {
			if s.NodeID == n.ID {
				if err := m.kernel.RetargetSlot(s.ID, n); err != nil {
					m.emit("warn", fmt.Sprintf("槽位 %s 更新出站失败: %v", s.ID, err))
				}
			}
		}
	}
}

// subscriptionLoop 定时刷新所有 auto_refresh 订阅。
func (m *Manager) subscriptionLoop() {
	defer close(m.subDone)
	t := time.NewTicker(1 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			// 先拷出到期订阅（回调内只读快照），锁外刷新——
			// RefreshSubscription 内部要拿 Store.mu，不能在 View 回调里同步调用。
			var due []string
			m.store.View(func(st *State) {
				for _, sub := range st.Subscriptions {
					if !sub.AutoRefresh {
						continue
					}
					dur, err := time.ParseDuration(sub.Interval)
					if err != nil || dur <= 0 {
						dur = time.Hour
					}
					if time.Since(sub.LastRefreshedAt) >= dur {
						due = append(due, sub.ID)
					}
				}
			})
			for _, id := range due {
				id := id
				go m.RefreshSubscription(id) // 各订阅独立刷新，不互相阻塞
			}
		case <-m.subStop:
			return
		}
	}
}

// TempPort 实现 TempPortDialer：为测速临时开节点 outbound + 本地端口。
func (m *Manager) TempPort(node NodeSpec, region string) (int, func(), error) {
	if m.kernel == nil {
		return 0, nil, fmt.Errorf("内核未启用")
	}
	// 复用已存在的槽位端口
	slots, _ := m.pool.Views()
	for _, s := range slots {
		if s.NodeID == node.ID {
			return s.Port, nil, nil
		}
	}
	// 临时通道：开一个专用槽位（用完删）
	tag := "probe_" + node.ID
	port, err := m.pool.allocProbePort()
	if err != nil {
		return 0, nil, err
	}
	if err := m.kernel.AddSlot(tag, port, node); err != nil {
		return 0, nil, err
	}
	release := func() {
		_ = m.kernel.RemoveSlot(tag)
	}
	return port, release, nil
}

// ---- 事件流 ----

func (m *Manager) emit(kind, msg string) {
	m.evMu.Lock()
	defer m.evMu.Unlock()
	m.evNext++
	m.events = append(m.events, Event{ID: m.evNext, At: time.Now(), Kind: kind, Msg: msg})
	if len(m.events) > 500 {
		m.events = m.events[len(m.events)-500:]
	}
}

// EventsSince 增量拉取事件（since 之后；since=0 全量）。
func (m *Manager) EventsSince(since int64) []Event {
	m.evMu.Lock()
	defer m.evMu.Unlock()
	var out []Event
	for _, e := range m.events {
		if e.ID > since {
			out = append(out, e)
		}
	}
	return out
}

// ---- 工具 ----

func indexNodes(nodes []Node) map[string]*Node {
	m := map[string]*Node{}
	for i := range nodes {
		m[nodes[i].ID] = &nodes[i]
	}
	return m
}

// shortHash 稳定短哈希（FNV-1a，hex 8 位）。
func shortHash(s string) string {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return fmt.Sprintf("%08x", h)
}

// sanitize 名称转安全 ID 片段。
func sanitize(s string) string {
	var b []rune
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b = append(b, r)
		case r >= 'A' && r <= 'Z':
			b = append(b, r+('a'-'A'))
		default:
			b = append(b, '_')
		}
	}
	out := string(b)
	if len(out) > 24 {
		out = out[:24]
	}
	if out == "" {
		out = "x"
	}
	return out
}
