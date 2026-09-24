// Manager 的管理门面：供 server 层（reqproxy_handlers.go）调用的 API。
package reqproxy

import (
	"fmt"
	"sort"
	"time"
)

// NewSubscription 构造带新 ID 的订阅。
func NewSubscription(name, rawURL string, autoRefresh bool, interval string) Subscription {
	if interval == "" {
		interval = "1h"
	}
	return Subscription{
		ID:          newID("sub"),
		Name:        name,
		URL:         rawURL,
		AutoRefresh: autoRefresh,
		Interval:    interval,
	}
}

// View 只读访问状态。
func (m *Manager) View(fn func(st *State)) { m.store.View(fn) }

// Update 修改状态。
func (m *Manager) Update(fn func(st *State)) { m.store.Update(fn) }

// NodesSnapshot 节点池快照。
func (m *Manager) NodesSnapshot() []Node { return m.pool.NodesSnapshot() }

// NodeLoads 各节点累计请求数（负载视图，API 用）。
func (m *Manager) NodeLoads() map[string]int64 { return m.pool.NodeLoadsSnapshot() }

// Views 槽位 + 绑定视图。
func (m *Manager) Views() ([]Slot, []BindingView) { return m.pool.Views() }

// HealthOf 节点健康快照（无记录返回零值）。
func (m *Manager) HealthOf(nodeID string) NodeHealth {
	m.store.View(func(st *State) {
		if h, ok := st.Health[nodeID]; ok {
			_ = h
		}
	})
	var out NodeHealth
	m.store.View(func(st *State) {
		if h, ok := st.Health[nodeID]; ok && h != nil {
			out = *h
		}
	})
	return out
}

// ImportManualNodes 手动批量导入节点。返回成功条数。
func (m *Manager) ImportManualNodes(text string) (int, error) {
	specs, errs := ParseText(text, "manual")
	for _, e := range errs {
		return 0, e
	}
	if len(specs) == 0 {
		return 0, fmt.Errorf("未解析出任何节点")
	}
	// 内核可用时立即验证可 Build：解析通过但 Xray 无法构造出站的定义
	// （如端口类型错误）在导入时就报出来，而不是入池后测速全挂。
	if m.kernel != nil {
		for _, s := range specs {
			if _, err := buildOutboundJSON(s.ID, s); err != nil {
				return 0, fmt.Errorf("节点 %q 定义无法生成出站: %w", s.Name, err)
			}
		}
	}
	// 落盘 manual_nodes（raw 原文按行保存）
	var added int
	m.store.Update(func(st *State) {
		for _, s := range specs {
			mn := ManualNode{ID: newID("node"), Name: s.Name, Raw: s.Raw, RegionTag: ""}
			st.ManualNodes = append(st.ManualNodes, mn)
			added++
		}
	})
	// 节点池同步（合并 manual + 现有订阅节点）
	m.rebuildPool()
	return added, nil
}

// DeleteManualNode 删除手动节点（重建池）。
func (m *Manager) DeleteManualNode(id string) {
	m.store.Update(func(st *State) {
		for i, mn := range st.ManualNodes {
			if mn.ID == id {
				st.ManualNodes = append(st.ManualNodes[:i], st.ManualNodes[i+1:]...)
				break
			}
		}
	})
	m.rebuildPool()
}

// DeleteSubscription 删除订阅（其节点一并移出池）。
func (m *Manager) DeleteSubscription(id string) {
	m.store.Update(func(st *State) {
		for i, s := range st.Subscriptions {
			if s.ID == id {
				st.Subscriptions = append(st.Subscriptions[:i], st.Subscriptions[i+1:]...)
				break
			}
		}
	})
	m.rebuildPool()
}

// rebuildPool 按 state（manual_nodes + 各订阅最近产物）重建节点池。
// 订阅节点用 RefreshSubscription 时已进池的快照；删除订阅时把它们踢出去。
func (m *Manager) rebuildPool() {
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
	// 现有池里保留非 manual 且订阅仍存在的节点
	liveSubs := map[string]bool{}
	m.store.View(func(st *State) {
		for _, s := range st.Subscriptions {
			liveSubs[s.ID] = true
		}
	})
	for _, n := range m.pool.NodesSnapshot() {
		if n.Source == "manual" {
			continue
		}
		if liveSubs[n.Source] {
			all = append(all, n.NodeSpec)
		}
	}
	diff := m.pool.SetNodes(all)
	m.applyNodeDiff(diff)
}

// UnbindAccount 解绑账号。
func (m *Manager) UnbindAccount(uid string) {
	if slotID, empty := m.pool.UnbindAccount(uid); empty && slotID != "" {
		if m.kernel != nil {
			_ = m.kernel.RemoveSlot(slotID)
		}
	}
}

// PinAccount 把账号 pin 到指定节点（空 = 取消 pin）。
func (m *Manager) PinAccount(uid, nodeID string) {
	m.store.Update(func(st *State) {
		if b, ok := st.Bindings[uid]; ok {
			b.PinnedNodeID = nodeID
			st.Bindings[uid] = b
		}
	})
	m.emit("info", fmt.Sprintf("账号 %s pin 设置: %q", uid, nodeID))
}

// ManualRetarget 槽位手动换指向。
func (m *Manager) ManualRetarget(slotID, nodeID string) error {
	var spec NodeSpec
	found := false
	for _, n := range m.pool.NodesSnapshot() {
		if n.ID == nodeID {
			spec = n.NodeSpec
			found = true
		}
	}
	if !found {
		return fmt.Errorf("节点 %s 不存在", nodeID)
	}
	// 直接换内核出站 + 更新槽位记录
	if m.kernel != nil {
		if err := m.kernel.RetargetSlot(slotID, spec); err != nil {
			return err
		}
	}
	m.store.Update(func(st *State) {
		for i := range st.Slots {
			if st.Slots[i].ID == slotID {
				st.Slots[i].NodeID = nodeID
				st.Slots[i].Since = time.Now()
			}
		}
	})
	m.emit("slot-switch", fmt.Sprintf("槽位 %s 手动切换到节点 %s", slotID, spec.Name))
	return nil
}

// RunHealthOnce 手动全量测速（带任务跟踪；结束后自动预热+再平衡）。
// 返回任务 ID 供前端轮询。
func (m *Manager) RunHealthOnce() string {
	// 同类任务已在跑：直接返回（前端动画保持）
	if RunningJob("health-run") != nil {
		return ""
	}
	id := StartJob("health-run", "全量测速")
	go func() {
		ok, fail := m.health.RunOnce()
		note := fmt.Sprintf("测速完成：%d 成功 / %d 失败", ok, fail)
		errText := ""
		if ok == 0 && fail > 0 {
			errText = note
		}
		FinishJob(id, note, errText)
	}()
	return id
}

// RebalanceNow 手动触发一轮负载再平衡（带任务跟踪）。返回 (任务ID, 迁移数)。
func (m *Manager) RebalanceNow() (string, int) {
	if RunningJob("rebalance") != nil {
		return "", 0
	}
	id := StartJob("rebalance", "负载再平衡")
	go func() {
		n := m.Rebalance()
		FinishJob(id, fmt.Sprintf("迁移 %d 个账号", n), "")
	}()
	return id, 0
}

// PrewarmNow 手动预热（带任务跟踪）。
func (m *Manager) PrewarmNow() string {
	if RunningJob("prewarm") != nil {
		return ""
	}
	id := StartJob("prewarm", "账号预热")
	go func() {
		bound, failed := m.PrewarmAll()
		FinishJob(id, fmt.Sprintf("新建 %d 个槽位，%d 个账号暂无合格节点", bound, failed), "")
	}()
	return id
}

// CompactNow 槽位整合（"整合槽位"按钮，带任务跟踪）：同节点多槽合并、
// 删空槽。不动节点选择，只收敛拓扑。
func (m *Manager) CompactNow() string {
	if RunningJob("compact") != nil {
		return ""
	}
	id := StartJob("compact", "整合槽位")
	go func() {
		merged, removed, _, removeOps := m.pool.CompactSlots()
		// 同节点合并不会产生 add（槽位已在内核）；remove 释放端口
		for _, op := range removeOps {
			if m.kernel != nil {
				if err := m.kernel.RemoveSlot(op.SlotID); err != nil {
					m.emit("warn", fmt.Sprintf("整合：删槽 %s 失败: %v", op.SlotID, err))
				}
			}
		}
		FinishJob(id, fmt.Sprintf("合并 %d 个账号，删除 %d 个冗余槽位", merged, removed), "")
	}()
	return id
}

// ReassignNow 全量重分配槽位（"重新分配槽位"按钮，带任务跟踪）：
// 清空全部槽位/绑定 → 按当前节点池+规则从头分配 → 恢复 pin。
// 用于规则大改后重排 / 清理历史散乱槽位。端口全量重建。
func (m *Manager) ReassignNow() string {
	if RunningJob("reassign") != nil {
		return ""
	}
	id := StartJob("reassign", "重新分配槽位")
	go func() {
		n, errText := m.reassignAll()
		FinishJob(id, fmt.Sprintf("重分配 %d 个账号", n), errText)
	}()
	return id
}

// reassignAll 重分配主体（ReassignNow 的后台执行体）。
func (m *Manager) reassignAll() (int, string) {
	if m.acctSrc == nil {
		return 0, "账号清单不可用（预热未启用）"
	}
	// 1. 清空（ResetSlots 返回 uid→原节点 pin 映射）
	pins, removeOps := m.pool.ResetSlots()
	// 2. 内核释放全部旧槽端口
	for _, op := range removeOps {
		if m.kernel != nil {
			if err := m.kernel.RemoveSlot(op.SlotID); err != nil {
				m.emit("warn", fmt.Sprintf("重分配：删旧槽 %s 失败: %v", op.SlotID, err))
			}
		}
	}
	// 3. 逐个重新分配（Assign 锁内选节点/槽位；返回 op 锁外接内核）
	uids := m.acctSrc.UIDs()
	sort.Strings(uids)
	bound := 0
	var addOps []KernelOp
	for _, uid := range uids {
		region := m.acctSrc.RegionOf(uid)
		if region == "" {
			region = "cn"
		}
		_, op, err := m.pool.Assign(uid, region)
		if err != nil {
			continue // 无合格节点等：惰性路径兜底
		}
		if op != nil && op.Kind == "add-slot" {
			addOps = append(addOps, *op)
		}
		bound++
	}
	// 4. 接内核
	for _, op := range addOps {
		if m.kernel != nil {
			if err := m.applyKernelOp(op); err != nil {
				m.emit("warn", fmt.Sprintf("重分配：建槽 %s 失败: %v", op.SlotID, err))
				// 槽位建失败就撤掉指向它的绑定，账号回到未绑定，下次请求重新分配。
				m.rollbackSlotBindings(op.SlotID)
			}
		}
	}
	// 5. 恢复 pin（pin 语义是"指定节点"，重分配不该把它丢掉）
	for uid, nodeID := range pins {
		m.PinAccount(uid, nodeID)
	}
	m.emit("info", fmt.Sprintf("全量重分配完成：%d 个账号，%d 个槽位", bound, len(addOps)))
	return bound, ""
}

// RefreshSubscriptionJob 订阅刷新（带任务跟踪）。返回任务 ID。
func (m *Manager) RefreshSubscriptionJob(subID string) string {
	if RunningJob("sub-refresh") != nil {
		return ""
	}
	id := StartJob("sub-refresh", "订阅刷新")
	go func() {
		if err := m.RefreshSubscription(subID); err != nil {
			FinishJob(id, "", err.Error())
		} else {
			FinishJob(id, "订阅刷新完成", "")
		}
	}()
	return id
}

// Jobs 最近任务列表（前端恢复动画/历史展示）。
func (m *Manager) Jobs(limit int) []Job { return RecentJobs(limit) }

// SetTTFBAccounts 注入真实首字测速的账号来源（按分组取号）。
func (m *Manager) SetTTFBAccounts(src TTFBAccountSource) {
	if m.ttfb != nil {
		m.ttfb.SetAccountSource(src)
	}
}

// SetTTFBRefresher 注入账号 token 刷新（探测前按需刷新）。
func (m *Manager) SetTTFBRefresher(r TokenRefresher) {
	if m.ttfb != nil {
		m.ttfb.SetTokenRefresher(r)
	}
}

// RunTTFB 启动一轮真实首字测速（任务化，立即返回任务 ID）。
// 已有一轮在跑时返回空 ID。
func (m *Manager) RunTTFB(group, model string, concurrency int, timeout time.Duration) (string, error) {
	if m.ttfb == nil {
		return "", fmt.Errorf("首字测速未初始化")
	}
	if RunningJob("ttfb-run") != nil {
		return "", nil
	}
	id := StartJob("ttfb-run", "真实首字测速")
	go func() {
		ok, fail, err := m.ttfb.Run(group, model, concurrency, timeout)
		if err != nil {
			FinishJob(id, "", err.Error())
			return
		}
		note := fmt.Sprintf("首字测速完成：%d 个测出首字 / %d 个失败", ok, fail)
		errText := ""
		if ok == 0 && fail > 0 {
			errText = note
		}
		FinishJob(id, note, errText)
	}()
	return id, nil
}

// TTFBResults 最近一轮真实首字测速的结果。
func (m *Manager) TTFBResults() map[string]TTFBResult {
	if m.ttfb == nil {
		return map[string]TTFBResult{}
	}
	return m.ttfb.Results()
}

// ProbeNode 单节点测速（同步执行：单次探测秒级完成，同步返回结果
// 供按钮动画与结果提示共用一条时序）。
func (m *Manager) ProbeNode(nodeID string) (int64, error) {
	var spec NodeSpec
	found := false
	for _, n := range m.pool.NodesSnapshot() {
		if n.ID == nodeID {
			spec = n.NodeSpec
			found = true
		}
	}
	if !found {
		return -1, fmt.Errorf("节点 %s 不在池里（可能刚被移除或未导入）", nodeID)
	}
	latency, err := m.health.probe(Node{NodeSpec: spec, Probed: true})
	m.pool.MarkProbed(nodeID, latency, err)
	return latency, err
}

// PreviewRules 规则预览（草稿模拟）。
type RulesPreview struct {
	Total         int            `json:"total"`
	Qualified     int            `json:"qualified"`
	FilteredOut   map[string]int `json:"filtered_out"`    // 原因分布
	BySource      map[string]int `json:"by_source"`       // 按订阅分组合格数
	Regions       map[string]int `json:"regions"`         // 各 region 合格节点数
	SharedPerNode map[string]int `json:"shared_per_node"` // 各 region 每节点账号数预估
	Notes         []string       `json:"notes"`
}

func (m *Manager) PreviewRules(draft *Rules) *RulesPreview {
	if draft == nil {
		m.store.View(func(st *State) { cp := st.Rules; draft = &cp })
	}
	out := &RulesPreview{
		FilteredOut:   map[string]int{},
		BySource:      map[string]int{},
		Regions:       map[string]int{},
		SharedPerNode: map[string]int{},
	}
	nodes := m.pool.NodesSnapshot()
	out.Total = len(nodes)
	// 各 region 账号数
	regionAccounts := map[string]int{}
	m.pool.mu.Lock()
	for uid, b := range m.pool.bindngs {
		_ = uid
		if s := m.pool.slots[b.SlotID]; s != nil {
			regionAccounts[s.Region]++
		}
	}
	m.pool.mu.Unlock()

	for _, n := range nodes {
		h := m.HealthOf(n.ID)
		switch {
		case h.Unhealthy:
			out.FilteredOut["unhealthy"]++
			continue
		case draft.MaxLatencyMs > 0 && latencyOverLimit(&h, *draft):
			out.FilteredOut["latency"]++
			continue
		case matchKeywords(n.Name, draft.ExcludeKeywords):
			out.FilteredOut["exclude"]++
			continue
		case len(draft.IncludeKeywords) > 0 && !matchKeywords(n.Name, draft.IncludeKeywords):
			out.FilteredOut["include"]++
			continue
		}
		// 地区规则（两 region 分别统计）
		qualifiedAny := false
		for region, allowed := range draft.RegionRules {
			if len(allowed) == 0 || containsStr(allowed, n.Region) {
				out.Regions[region]++
				qualifiedAny = true
			}
		}
		if len(draft.RegionRules) == 0 {
			qualifiedAny = true
		}
		if !qualifiedAny {
			out.FilteredOut["region"]++
			continue
		}
		out.Qualified++
		src := n.Source
		if src == "" {
			src = "manual"
		}
		out.BySource[src]++
	}
	// 共享度预估
	for region, accounts := range regionAccounts {
		qn := out.Regions[region]
		if qn == 0 {
			continue
		}
		out.SharedPerNode[region] = (accounts + qn - 1) / qn
		if draft.AccountsPerNode > 0 && out.SharedPerNode[region] > draft.AccountsPerNode {
			out.Notes = append(out.Notes, fmt.Sprintf(
				"%s 区合格节点不足以按目标 %d 账号/节点容纳 %d 个账号（预估最大 %d/节点）",
				region, draft.AccountsPerNode, accounts, out.SharedPerNode[region]))
		}
	}
	return out
}

// emitPublic 外部触发事件的导出版（供 handler 用，必要时）。
func (m *Manager) Emit(kind, msg string) { m.emit(kind, msg) }
