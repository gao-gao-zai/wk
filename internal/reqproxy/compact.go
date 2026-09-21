// 槽位整合与全量重分配（"重新分配槽位"按钮）。
//
// 背景：历史 bug（启动窗口期负载并列 + map 随机序）把账号散出大量
// 同节点重复槽/空槽。Compact 收敛槽位拓扑；ReassignAll 更彻底——
// 解绑全部账号后从当前节点池+规则重新计算分配（受 apn 容量约束），
// 用于规则大改后一键重排。两者都保持"同节点一个槽"。
package reqproxy

import (
	"sort"
	"time"
)

// CompactSlots 整合槽位：
//  1. 同节点同 region 的多个槽 → 合并到一个（账号全部并到保留槽，受 apn 容量约束时优先保留多账号的槽）
//  2. 零账号槽位 → 删除（返回 remove 操作清单让内核释放端口）
//
// 返回 (合并账号数, 删除槽位数, 需内核执行的 add/remove 操作)。
// 已有绑定的账号在合并时可能超 apn（同节点账号总数超限）——不丢账号，
// 溢出的留在原槽（下轮 rebalance 会迁走）。
func (p *Pool) CompactSlots() (merged int, removed int, addOps []KernelOp, removeOps []KernelOp) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 按 (NodeID, region) 分组
	type group struct {
		keep  *Slot
		extra []*Slot
	}
	groups := map[string]*group{}
	for _, s := range p.slots {
		if s.NodeID == "" {
			continue
		}
		g := groups[s.NodeID+"\x00"+s.Region]
		if g == nil {
			groups[s.NodeID+"\x00"+s.Region] = &group{keep: s}
			continue
		}
		// 保留账号多的（容量大）；并列保留 ID 小的（稳定）
		if p.slotCountLocked(s.ID) > p.slotCountLocked(g.keep.ID) ||
			(p.slotCountLocked(s.ID) == p.slotCountLocked(g.keep.ID) && s.ID < g.keep.ID) {
			g.extra = append(g.extra, g.keep)
			g.keep = s
		} else {
			g.extra = append(g.extra, s)
		}
	}

	for _, g := range groups {
		for _, old := range g.extra {
			for uid, b := range p.bindngs {
				if b.SlotID == old.ID {
					// 并入保留槽（容量满也并——账号不丢，rebalance 后续摊）
					p.bindngs[uid] = Binding{SlotID: g.keep.ID, Since: time.Now()}
					merged++ // 只数真正换槽的（已在 keep 上的不动）
				}
			}
			delete(p.slots, old.ID)
			removed++
			removeOps = append(removeOps, KernelOp{Kind: "remove-slot", SlotID: old.ID, Port: old.Port})
		}
	}

	// 零账号槽位删除（含悬空指向的——节点没了留着无意义）
	for _, s := range p.slots {
		if s.NodeID != "" && p.slotCountLocked(s.ID) == 0 {
			if _, alive := p.nodes[s.NodeID]; !alive {
				delete(p.slots, s.ID)
				removed++
				removeOps = append(removeOps, KernelOp{Kind: "remove-slot", SlotID: s.ID, Port: s.Port})
				continue
			}
			if _, has := groups[s.NodeID+"\x00"+s.Region]; has {
				// 该节点已有保留槽：空槽冗余，删
				delete(p.slots, s.ID)
				removed++
				removeOps = append(removeOps, KernelOp{Kind: "remove-slot", SlotID: s.ID, Port: s.Port})
			}
		}
	}

	if removed > 0 || merged > 0 {
		p.persistLocked()
	}
	return merged, removed, addOps, removeOps
}

// ResetSlots 清空全部槽位与绑定（PinnedNodeID 保留在返回的 uid→pin 映射里）。
// 返回需内核执行的 remove 操作（释放端口）。全量重分配的第一步。
func (p *Pool) ResetSlots() (pins map[string]string, removeOps []KernelOp) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pins = map[string]string{}
	for uid, b := range p.bindngs {
		if s := p.slots[b.SlotID]; s != nil && s.NodeID != "" {
			pins[uid] = s.NodeID // 记住 pin，重分配后由调用方恢复
		}
	}
	for _, s := range p.slots {
		removeOps = append(removeOps, KernelOp{Kind: "remove-slot", SlotID: s.ID, Port: s.Port})
	}
	p.slots = map[string]*Slot{}
	p.bindngs = map[string]Binding{}
	p.nextPort = p.portMin // 端口从头分配（内核 remove 后即释放）
	p.persistLocked()
	return pins, removeOps
}

// EmptySlots 零账号槽位清单（回收用）。
func (p *Pool) EmptySlots() []Slot {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Slot
	for _, s := range p.slots {
		if p.slotCountLocked(s.ID) == 0 {
			out = append(out, *s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// RemoveSlotRecord 删槽位记录（内核已 RemoveSlot 后调）。
func (p *Pool) RemoveSlotRecord(slotID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.slots[slotID]; ok {
		delete(p.slots, slotID)
		p.persistLocked()
	}
}

// sortSlotsByID 输出稳定排序用。
func sortSlotsByID(slots []*Slot) {
	sort.Slice(slots, func(i, j int) bool { return slots[i].ID < slots[j].ID })
}
