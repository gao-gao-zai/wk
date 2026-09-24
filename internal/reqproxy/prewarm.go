// Manager 的账号预热：主动给全部账号提前建绑定 + 槽位。
//
// 背景：Assign 是惰性的——账号第一个请求才建槽位，首个请求要付内核开槽
// 成本，且 WebUI 看不到"谁会走哪"的预览。Prewarm 在启动后、订阅刷新后、
// 测速轮后主动把全量账号分配好，请求路径只剩查表。
//
// 不看 Enabled：模块未启用也预建分配（启用前就分配好，开关只控制
// "流量是否走代理"）。WebUI 可在开关未开时预览槽位与绑定。
package reqproxy

import (
	"fmt"
	"sort"
)

// AccountSource 账号清单来源（main 注入 pool.UIDs + region 查询；避免包循环）。
type AccountSource interface {
	// UIDs 全量账号。
	UIDs() []string
	// RegionOf uid 的 region（cn|global）；未知账号返回 ""。
	RegionOf(uid string) string
}

// SetAccounts 注入账号清单（nil = 关闭预热）。
func (m *Manager) SetAccounts(src AccountSource) {
	m.acctSrc = src
}

// PrewarmAll 给全部未绑定账号预建槽位。返回 (新建绑定数, 失败数)。
//
// 已绑定的跳过（稳定性优先）；无可用节点不报错（保持惰性路径兜底）；
// 单账号失败不阻塞其余。每个新建槽位立即接内核（端口立即可用）。
func (m *Manager) PrewarmAll() (bound, failed int) {
	if m.acctSrc == nil {
		return 0, 0
	}

	uids := m.acctSrc.UIDs()
	sort.Strings(uids)
	for _, uid := range uids {
		region := m.acctSrc.RegionOf(uid)
		if region == "" {
			region = "cn"
		}
		// 已绑定跳过：Assign 命中现有绑定即返回，天然幂等。
		// 内核为 nil（测试）时只建逻辑槽位。
		_, op, err := m.pool.Assign(uid, region)
		if err != nil {
			// 无可用节点（空池在 Assign 里返回 port=0 不报错；这里是"有节点但
			// 全不合格"）——预热阶段不硬失败，惰性路径会在请求时报结构化错误。
			failed++
			continue
		}
		if op != nil && op.Kind == "add-slot" {
			if m.kernel != nil {
				if err := m.applyKernelOp(*op); err != nil {
					// 绑定已落盘但内核没槽：回滚，否则这个账号被钉死在死端口上，
					// 下次预热还因为它"已绑定"直接跳过。
					m.rollbackKernelSlot(uid, op.SlotID)
					failed++
					continue
				}
			}
			bound++
		}
	}
	if bound > 0 || failed > 0 {
		m.emit("info", fmt.Sprintf("预热完成：新建 %d 个槽位，%d 个账号暂无合格节点（保留惰性兜底）", bound, failed))
	}
	return bound, failed
}

// PrewarmIfDue 池里有可分配节点（静态筛选通过的就算——启动窗口期未测速
// 节点立即可分配）时预热——供健康检查轮/订阅刷新后/启用切换后调用；
// 池空时静默跳过。
func (m *Manager) PrewarmIfDue() {
	if m.acctSrc == nil {
		return
	}
	// 池里没有任何候选（静态筛选后为空）→ 分配必失败，不浪费
	if len(m.pool.Candidates("cn")) == 0 && len(m.pool.Candidates("global")) == 0 {
		return
	}
	m.PrewarmAll()
}
