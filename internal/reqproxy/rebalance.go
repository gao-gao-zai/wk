// Rebalance：负载再平衡（v2，重写）。
//
// v1 的问题：只在"既有槽位间"迁移，且门槛（负载差 25%+50 请求）在重启后
// （请求数归零）永远达不到——用户点"再平衡"毫无效果。
//
// v2 语义：
//   - 有效负载 = 账号数×100 + 累计请求数/10。重启后按账号数摊开；流量
//     积累后请求数逐渐主导。
//   - 主动摊开：把账号从最重节点迁到最轻节点（差 > 1 个账号当量就动），
//     目标节点没有槽位就新建（内核 add-slot）。
//   - accounts_per_node 只是槽位容量上限，不再是"打包到几个节点"的目标
//     ——用户的诉求是请求摊到所有代理上。
//   - pinned 账号不动；迁移只换绑定，既有槽位端口不变（账号零感知）。
package reqproxy

import (
	"fmt"
	"time"
)

// RebalanceOp 一次账号迁移。
type RebalanceOp struct {
	UID      string
	FromSlot string
	ToSlot   string // 非空：迁入既有槽位（端口不变）
	ToNode   string // ToSlot 为空时：新建槽位指向该节点
	Region   string
}

// PlanRebalance 计算迁移方案（纯读，不改状态）：按 region 各自摊平。
func (p *Pool) PlanRebalance() []RebalanceOp {
	p.mu.Lock()
	defer p.mu.Unlock()

	apn := p.store.st.Rules.AccountsPerNode

	// 收集有绑定账号的 region
	regions := map[string]bool{}
	for _, s := range p.slots {
		if s.NodeID != "" {
			regions[s.Region] = true
		}
	}

	var ops []RebalanceOp
	for region := range regions {
		cands := p.candidatesLocked(region)
		if len(cands) < 2 {
			continue
		}
		candSet := map[string]bool{}
		for _, c := range cands {
			candSet[c.ID] = true
		}
		// uid → 其槽位指向的节点（仅候选内的、非 pin 的）
		uidNode := map[string]string{}
		nodeAccounts := map[string]int{}
		for uid, b := range p.bindngs {
			s := p.slots[b.SlotID]
			if s == nil || s.NodeID == "" || !candSet[s.NodeID] {
				continue
			}
			if b.PinnedNodeID != "" {
				continue // 手动 pin 的不动
			}
			uidNode[uid] = s.NodeID
			nodeAccounts[s.NodeID]++
		}
		if len(uidNode) == 0 {
			continue
		}
		eff := func(node string) int64 {
			return int64(nodeAccounts[node])*100 + p.loads[node]/10
		}
		for iter := 0; iter < 200; iter++ {
			heavy, light := "", ""
			for _, c := range cands {
				if heavy == "" || eff(c.ID) > eff(heavy) {
					heavy = c.ID
				}
				if light == "" || eff(c.ID) < eff(light) {
					light = c.ID
				}
			}
			if heavy == light {
				break
			}
			// 触发条件（其一）：
			//  a) 总负载差 > 1 账号当量（账号数不均）
			//  b) 流量差 ≥ 5×（纯流量倾斜：热账号集中）——账号当量不该稀释真实流量差
			loadGap := eff(heavy) - eff(light)
			trafficRatio := int64(0)
			if p.loads[light] > 0 {
				trafficRatio = p.loads[heavy] / p.loads[light]
			} else if p.loads[heavy] > 0 {
				trafficRatio = 6 // 轻端零流量、重端有流量：按显著倾斜处理
			}
			if loadGap <= 100 && trafficRatio < 5 {
				break // 已摊平
			}
			if nodeAccounts[heavy] == 0 {
				break // 重的是请求数而非账号数：流量不均，迁账号帮不上
			}
			// 从 heavy 挑一个可迁账号（map 迭代随机 → 天然轮换）
			var victim string
			for uid, node := range uidNode {
				if node == heavy {
					victim = uid
					break
				}
			}
			if victim == "" {
				break
			}
			// 单步收益检查：迁移后两端差距应缩小，否则不迁（防来回抖——
			// 1 热账号独占流量时，迁走它反而把轻端压成重端）。
			// 流量归属跟随账号：loads 不变，账号当量互换。
			gapBefore := eff(heavy) - eff(light)
			simHeavy := eff(heavy) - 100
			simLight := eff(light) + 100
			gapAfter := simHeavy - simLight
			if simLight > simHeavy && gapAfter < -gapBefore {
				// 迁后会反向超调且比原来更糟 → 停（热账号独占流量的场景）
				break
			}
			// light 上找有余量的既有槽位
			var toSlot string
			for _, s := range p.slots {
				if s.NodeID == light && s.Region == region &&
					(apn <= 0 || p.slotCountLocked(s.ID) < apn) {
					toSlot = s.ID
					break
				}
			}
			from := p.bindngs[victim].SlotID
			if toSlot != "" {
				ops = append(ops, RebalanceOp{UID: victim, FromSlot: from, ToSlot: toSlot, Region: region})
			} else {
				ops = append(ops, RebalanceOp{UID: victim, FromSlot: from, ToNode: light, Region: region})
			}
			// 模拟执行，继续下一轮摊
			delete(uidNode, victim)
			nodeAccounts[heavy]--
			nodeAccounts[light]++
		}
	}
	return ops
}

// ApplyRebalance 执行迁移。返回 (迁移数, 需内核执行的 add-slot 操作)。
// 只换绑定；新建槽位由调用方（Manager）拿 KernelOp 接内核。
func (p *Pool) ApplyRebalance(ops []RebalanceOp) (int, []KernelOp) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	var kernelOps []KernelOp
	for _, op := range ops {
		b, ok := p.bindngs[op.UID]
		if !ok || b.SlotID != op.FromSlot {
			continue // 状态已变（并发迁移/解绑），跳过
		}
		if op.ToSlot != "" {
			if _, ok := p.slots[op.ToSlot]; !ok {
				continue
			}
			b.SlotID = op.ToSlot
		} else {
			node := p.nodes[op.ToNode]
			if node == nil {
				continue
			}
			port, err := p.allocPortLocked()
			if err != nil {
				continue
			}
			slotID := newID("slot")
			p.slots[slotID] = &Slot{ID: slotID, Port: port, NodeID: op.ToNode, Region: op.Region, Since: time.Now()}
			kernelOps = append(kernelOps, KernelOp{Kind: "add-slot", SlotID: slotID, Port: port, NodeSpec: node.NodeSpec})
			b.SlotID = slotID
		}
		b.Since = time.Now()
		p.bindngs[op.UID] = b
		n++
	}
	if n > 0 {
		p.persistLocked()
	}
	return n, kernelOps
}

// Rebalance 执行一轮再平衡（周期/手动共用）。返回迁移账号数。
func (m *Manager) Rebalance() int {
	ops := m.pool.PlanRebalance()
	if len(ops) == 0 {
		return 0
	}
	n, addOps := m.pool.ApplyRebalance(ops)
	for _, op := range addOps {
		if m.kernel != nil {
			if err := m.applyKernelOp(op); err != nil {
				m.emit("warn", fmt.Sprintf("再平衡新建槽位 %s 失败: %v", op.SlotID, err))
			}
		}
	}
	if n > 0 {
		// 迁移后清理空槽（账号迁走的旧槽不再占端口）
		if removed := m.cleanupEmptySlots(); removed > 0 {
			m.emit("info", fmt.Sprintf("回收 %d 个空槽位", removed))
		}
		m.emit("slot-switch", fmt.Sprintf("负载再平衡：迁移 %d 个账号（新建槽位 %d 个）", n, len(addOps)))
	}
	return n
}

// cleanupEmptySlots 删除零账号槽位（内核释放端口）。返回删除数。
func (m *Manager) cleanupEmptySlots() int {
	empty := m.pool.EmptySlots()
	for _, s := range empty {
		if m.kernel != nil {
			if err := m.kernel.RemoveSlot(s.ID); err != nil {
				continue // 内核没有（未接）：直接删记录
			}
		}
		m.pool.RemoveSlotRecord(s.ID)
	}
	return len(empty)
}
