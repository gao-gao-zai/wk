// HealthFixup：测速后的槽位修正。
//
// 启动窗口期允许未测速节点被分配（否则启用模块后到首轮测速前分配不到
// 任何节点）。代价是可能押错节点——测速轮完成后把指向"确认是坏的"
// （不健康或超延迟上限）节点的槽位迁走，端口不变。
package reqproxy

import (
	"fmt"
)

// fixupUnhealthySlots 测速轮结束后：指向坏节点的槽位换指向。
// 迁移保持端口不变（账号零感知）；无替代节点时保留原状（下轮再试）。
func (m *Manager) fixupUnhealthySlots() int {
	if m.kernel == nil {
		return 0
	}
	slots, _ := m.pool.Views()
	fixed := 0
	for _, s := range slots {
		if s.NodeID == "" {
			continue
		}
		if !m.nodeIsBad(s.NodeID) {
			continue
		}
		// Retarget 前先确认有替代节点（Retarget 失败会报错，避免槽位悬空）
		cands := m.pool.Candidates(s.Region)
		if len(cands) == 0 {
			continue
		}
		op, err := m.pool.Retarget(s.ID, s.Region)
		if err != nil || op == nil {
			continue
		}
		m.kernMu.Lock()
		kerr := m.kernel.RetargetSlot(op.SlotID, op.NodeSpec)
		m.kernMu.Unlock()
		if kerr != nil {
			continue
		}
		fixed++
		m.emit("slot-switch", fmt.Sprintf("槽位 %s 从坏节点 %s 迁移到 %s（端口不变）", s.ID, s.NodeID, op.NodeSpec.Name))
	}
	return fixed
}

// nodeIsBad 节点确认不可用：已标不健康，或测过速且超延迟上限。
// 未测速节点不算坏（还没证据）。
func (m *Manager) nodeIsBad(nodeID string) bool {
	bad := false
	m.store.View(func(st *State) {
		h, ok := st.Health[nodeID]
		if !ok || h == nil {
			return
		}
		if h.Unhealthy {
			bad = true
			return
		}
		if st.Rules.MaxLatencyMs > 0 && latencyOverLimit(h, st.Rules) {
			bad = true
		}
	})
	return bad
}
