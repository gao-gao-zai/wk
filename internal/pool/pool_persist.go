// pool_persist.go DB 模式（StatePersister 注入）的增量落盘。
//
// 与文件模式的分工（saveLocked/persistSnapshot 在 pool.go）：
//   - 文件模式：dirty 标记 → 全量 stateOverviewLocked → MarshalIndent →
//     tmp+rename 原子替换 state.json。行为与历史版本完全一致。
//   - DB 模式：dirtySet 按 uid 记增量操作 → flush 时只 marshal 变更行 →
//     单事务 UpsertPoolAccounts / DeletePoolAccounts。
//
// 锁纪律与文件模式相同：p.mu 内只做 O(dirtySet) 的快照拷贝（微秒级），
// 序列化与 SQL 都在 persistMu 下、p.mu 之外。写库失败：行保留在 dirtySet
// （下轮 flush 重试），沿用 notePersistFail 的节流日志——与"整文件失败
// 下轮全量重来"等价的自愈语义。
package pool

import (
	"encoding/json"
	"log"
	"time"
)

// dirtyDirtyUp 标记 uid 待 upsert（DB 模式）。调用方必须已持 p.mu。
// 同 uid 重复标记保留最新版本号；delete 后再 upsert 覆盖回 upsert。
// 不动 p.dirty（全量标记是文件模式的专属状态，仅 noteChangeLocked 置位）。
func (p *Pool) markDirtyUpLocked(uid string) {
	if uid == "" {
		return
	}
	p.versionC++
	p.dirtySet[uid] = dirtyOp{op: 0, version: p.versionC}
}

// markDirtyDeleteLocked 标记 uid 待删除（DB 模式）。调用方必须已持 p.mu。
func (p *Pool) markDirtyDeleteLocked(uid string) {
	if uid == "" {
		return
	}
	p.versionC++
	p.dirtySet[uid] = dirtyOp{op: 1, version: p.versionC}
}

// markDirtyAllLocked 标记全部账号待 upsert（Redis 快照被采用后，把采用的
// 状态整体写穿到 DB）。调用方必须已持 p.mu。
func (p *Pool) markDirtyAllLocked() {
	for uid := range p.byUID {
		p.markDirtyUpLocked(uid)
	}
}

// stateAccountOfLocked 把单个 entry 转成持久化形态（stateAccount）。
// 调用方必须已持 p.mu。与 stateOverviewLocked 的字段拷贝逻辑完全一致，
// 仅作用域收窄到单账号。
func (p *Pool) stateAccountOfLocked(e *entry) stateAccount {
	return stateAccount{
		Credits:             e.credits,
		CapacitySize:        e.capacitySize,
		CapacityRemain:      e.capacityRemain,
		CapacityUsed:        e.capacityUsed,
		CycleCapacitySize:   e.cycleCapacitySize,
		CycleCapacityRemain: e.cycleCapacityRemain,
		CycleCapacityUsed:   e.cycleCapacityUsed,
		CreditUpdatedAt:     e.creditUpdatedAt,
		Disabled:            e.disabled,
		Reason:              e.reason,
		Until:               e.until,
		CoolKind:            e.coolKind,
		ModelCooldowns:      saveModelCooldowns(e.modelCooldowns),
		SuccessCount:        e.successCount,
		ErrTotal:            e.errTotal,
		LastSuccess:         e.lastSuccess,
		LastErr:             e.lastErr,
		RiskStrikes:         e.riskStrikes,
	}
}

// flushDirtySet DB 模式的增量落盘入口（flusher/Flush 在 dirtySet 非空时
// 调用它替代 saveLocked）。锁纪律：p.mu 内拷贝 dirtySet + 逐行快照，
// 然后释放 p.mu，persistMu 下做 marshal + SQL。
//
// 版本号语义：写库事务期间某行又被改动（版本变化），该行保留在 dirtySet
// 下轮重写——"事务读到的旧值不可能覆盖新状态"由内存版本号保证（比
// persistMu 的整快照先序更细粒度：persistMu 只保证本轮不被并发 flush
// 打断，不保证"收集→提交"窗口内的更新不丢，版本号补的就是这个窗口）。
func (p *Pool) flushDirtySet() {
	p.mu.Lock()
	if len(p.dirtySet) == 0 || p.persister == nil {
		p.mu.Unlock()
		return
	}
	s := p.persister
	collected := make(map[string]dirtyOp, len(p.dirtySet))
	for uid, op := range p.dirtySet {
		collected[uid] = op
	}
	upserts := make(map[string]stateAccount, len(collected))
	deletes := make([]string, 0, len(collected))
	for uid, op := range collected {
		if op.op == 1 {
			deletes = append(deletes, uid)
			continue
		}
		if e, ok := p.byUID[uid]; ok {
			upserts[uid] = p.stateAccountOfLocked(e)
		}
		// 不在 byUID 的 upsert 标记（账号刚被删除又标 upsert 的竞态）：
		// 丢弃即可，删除路径会兜底。
	}
	p.mu.Unlock()

	p.persistMu.Lock()
	defer p.persistMu.Unlock()

	if err := p.writeDirtyRows(s, upserts, deletes); err != nil {
		p.notePersistFail(err, true)
		return // 行保留在 dirtySet，下轮 flush 重试
	}
	if p.persistFails > 0 {
		log.Printf("pool: state.db 落盘恢复（此前连续失败 %d 次）", p.persistFails)
		p.persistFails = 0
	}
	// 清除成功写出的行：版本未变的才清（事务期间又被改的行保留下轮重写）。
	p.mu.Lock()
	for uid, op := range collected {
		if cur, ok := p.dirtySet[uid]; ok && cur.version == op.version {
			delete(p.dirtySet, uid)
		}
	}
	p.mu.Unlock()
}

// writeDirtyRows 把增量行写入 DB（不在任何锁下由 flushDirtySet 调用）。
func (p *Pool) writeDirtyRows(s StatePersister, upserts map[string]stateAccount, deletes []string) error {
	if len(upserts) > 0 {
		rows := make(map[string][]byte, len(upserts))
		for uid, sa := range upserts {
			raw, err := json.Marshal(sa)
			if err != nil {
				return err
			}
			rows[uid] = raw
		}
		if err := s.UpsertPoolAccounts(rows); err != nil {
			return err
		}
	}
	if len(deletes) > 0 {
		return s.DeletePoolAccounts(deletes)
	}
	return nil
}

// redisMirrorDirtySet DB 模式的 Redis 镜像：只有 dirtySet 触发的 flush 才
// 需要镜像（镜像仍走全量快照，Redis 是"恢复备份"不是增量存储；频率与
// 文件模式一致——每次 flush 一份）。
func (p *Pool) mirrorSnapshotToRedis() {
	if p.store == nil {
		return
	}
	p.mu.RLock()
	sf := p.stateOverviewLocked()
	p.mu.RUnlock()
	snapRaw, err := json.Marshal(snapshot{stateFile: sf, SavedAt: time.Now()})
	if err == nil {
		p.store.SaveState(snapRaw)
	}
}
