// Rebalance：负载再平衡（v3，窗口版）。
//
// v2 的问题：eff = 账号数×100 + 累计请求/10。累计值带历史包袱（昨天热
// 今天闲的节点永远被视为重），且账号数均匀但热度倾斜时（10 冷账号 vs
// 1 热账号同节点）账号数信号完全掩盖流量倾斜。
//
// v3 语义：
//   - eff(node) = 节点近窗口速率 nodeRPM（纯当前流量，无历史包袱）。
//   - 触发条件：窗口流量差 ≥ 5× 或绝对差 > 1 个槽位预算。
//   - 受害者选择：从重端挑 uidRPM 最大的可迁账号——迁冷账号不降压，
//     只有迁热账号才有意义；热账号 10 分钟迁移冷却（正复用连接，频繁
//     换槽位撕连接得不偿失）。
//   - pinned 账号不动；迁移只换绑定，既有槽位端口不变（账号零感知）。
//   - accounts_per_node 仍是迁移目的地的容量硬上限。
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

// lastMovedAt 账号迁移冷却记录（进程内存；重启清零可接受——重启本身
// 就会重建内核连接，冷却的意义在进程生命周期内）。
var lastMovedAt = map[string]time.Time{}

// rebalanceCooldown 迁移冷却时长。
const rebalanceCooldown = 10 * time.Minute

// PlanRebalance 计算迁移方案（纯读，不改状态）：按 region 各自摊平。
func (p *Pool) PlanRebalance() []RebalanceOp {
	p.mu.Lock()
	defer p.mu.Unlock()

	apn := p.store.st.Rules.AccountsPerNode
	budget := p.store.st.Rules.SlotBudgetRPM
	if budget <= 0 {
		budget = 60
	}
	now := WinClock()

	// 冷却过期的记录清掉（顺手，避免 map 无限增长）
	for uid, at := range lastMovedAt {
		if now.Sub(at) >= rebalanceCooldown {
			delete(lastMovedAt, uid)
		}
	}

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
		// uid → 其槽位指向的节点（仅候选内的、非 pin 的、不在冷却期的）
		uidNode := map[string]string{}
		for uid, b := range p.bindngs {
			s := p.slots[b.SlotID]
			if s == nil || s.NodeID == "" || !candSet[s.NodeID] {
				continue
			}
			if b.PinnedNodeID != "" {
				continue // 手动 pin 的不动
			}
			if at, cooled := lastMovedAt[uid]; cooled && now.Sub(at) < rebalanceCooldown {
				continue // 迁移冷却中
			}
			uidNode[uid] = s.NodeID
		}
		if len(uidNode) == 0 {
			continue
		}
		// eff = 节点近窗口速率（纯当前流量）
		eff := func(node string) float64 {
			return p.nodeRPMLocked(node)
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
			//  a) 窗口流量差 > 1 个槽位预算（绝对差显著）
			//  b) 流量比 ≥ 5×（相对倾斜显著）
			loadGap := eff(heavy) - eff(light)
			trafficRatio := 0.0
			lightRPM := eff(light)
			if lightRPM > 0.01 {
				trafficRatio = eff(heavy) / lightRPM
			} else if eff(heavy) > 0.01 {
				trafficRatio = 6 // 轻端零流量、重端有流量：按显著倾斜处理
			}
			if loadGap <= float64(budget) && trafficRatio < 5 {
				break // 已摊平
			}
			// 从 heavy 挑 uidRPM 最大的可迁账号（迁冷账号不降压）
			var victim string
			var victimRPM float64
			for uid, node := range uidNode {
				if node != heavy {
					continue
				}
				rpm := p.uidRPMLocked(uid)
				if rpm < 0 {
					rpm = 0
				}
				if victim == "" || rpm > victimRPM {
					victim, victimRPM = uid, rpm
				}
			}
			if victim == "" {
				break // 重端无可迁账号（全 pin/冷却中）
			}
			// 单步收益检查：迁移后两端差距应缩小，否则不迁（防来回抖——
			// 1 热账号独占流量时，迁走它反而把轻端压成重端）。
			// 流量归属跟随账号：窗口计数不迁移，下一窗口自然修正。
			gapBefore := eff(heavy) - eff(light)
			gapAfter := (eff(heavy) - victimRPM) - (eff(light) + victimRPM)
			if gapAfter < 0 && -gapAfter > gapBefore {
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
			// v3 简化：每 region 每轮最多迁一个账号。窗口流量跟随账号移动
			// 要到下一窗口才反映出来，本轮继续迭代只会基于过时的 eff 误判。
			// 15 分钟周期 / 手动多点几次，几轮内自然收敛。
			delete(uidNode, victim)
			break
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
		lastMovedAt[op.UID] = time.Now() // 迁移冷却：10 分钟内不再迁
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
