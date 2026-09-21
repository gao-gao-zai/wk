package reqproxy

import (
	"fmt"
	"testing"
	"time"
)

// newTestPool 内存池（无 state 文件）。
func newTestPool(t *testing.T) *Pool {
	t.Helper()
	store, err := NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	return NewPool(store, 31080, 31999)
}

func probedNode(id string) NodeSpec {
	return NodeSpec{
		ID: id, Name: id, Protocol: "http", Source: "manual",
		Spec: map[string]any{"host": "127.0.0.1", "port": 8080},
	}
}

// markProbedLocked 辅助：把节点标记为已测速（绕过真实探测）。
func markHealthy(t *testing.T, p *Pool, nodeID string, latency int64) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.nodes[nodeID]
	if n == nil {
		t.Fatalf("node %s not in pool", nodeID)
	}
	n.Probed = true
	h := p.healthLocked(nodeID)
	h.LatencyMs = latency
	h.Unhealthy = false
	h.FailStreak = 0
}

// seedWindow 在窗口计数器里预置 n 次"刚刚"的请求（测试辅助）。
func seedWindow(p *Pool, nodeID string, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w := p.nodeWinLocked(nodeID)
	for i := 0; i < n; i++ {
		w.Add(WinClock())
	}
}

// TestAssignLoadBalanceNewAccounts 新账号应按节点窗口速率分到轻载节点：
// node-A 近窗口 100 请求、node-B 0 请求 → 新账号的新槽位应指向 node-B。
func TestAssignLoadBalanceNewAccounts(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("a"), probedNode("b")})
	markHealthy(t, p, "a", 100)
	markHealthy(t, p, "b", 100)

	// 预置窗口负载：a=100, b=0
	seedWindow(p, "a", 100)

	// 第一个账号：无既有槽位，选节点 → b 更轻
	_, op, err := p.Assign("u1", "cn")
	if err != nil || op == nil {
		t.Fatalf("assign: %v %v", err, op)
	}
	if op.NodeSpec.ID != "b" {
		t.Fatalf("新槽位应指向轻载节点 b，实际 %s", op.NodeSpec.ID)
	}
}

// TestAssignJoinsLightestSlot 并入槽位时选剩余预算最大的槽位
// （节点侧流量均摊到其槽位数）。
func TestAssignJoinsLightestSlot(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("a"), probedNode("b")})
	markHealthy(t, p, "a", 100)
	markHealthy(t, p, "b", 100)

	// 手工建两个槽位（模拟既有状态）
	p.mu.Lock()
	p.slots["slot_a"] = &Slot{ID: "slot_a", Port: 31080, NodeID: "a", Region: "cn", Since: time.Now()}
	p.slots["slot_b"] = &Slot{ID: "slot_b", Port: 31081, NodeID: "b", Region: "cn", Since: time.Now()}
	p.mu.Unlock()
	seedWindow(p, "a", 500)
	seedWindow(p, "b", 10)

	port, _, err := p.Assign("u1", "cn")
	if err != nil {
		t.Fatal(err)
	}
	if port != 31081 {
		t.Fatalf("应并入轻载槽位 slot_b(31081)，实际端口 %d", port)
	}
}

// TestAssignBudgetBlocksHotAccount 槽位预算不足时，热账号进不去：
// slot_a 窗口流量已接近预算 → uidRPM 高的账号被拒，冷账号可进。
func TestAssignBudgetBlocksHotAccount(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("a"), probedNode("b")})
	markHealthy(t, p, "a", 100)
	markHealthy(t, p, "b", 100)

	p.mu.Lock()
	p.slots["slot_a"] = &Slot{ID: "slot_a", Port: 31080, NodeID: "a", Region: "cn", Since: time.Now()}
	p.mu.Unlock()
	// 预算默认 60；slot_a 已流 50 rpm → 剩 10：容得下冷账号（冷启动占位 6），
	// 容不下热账号
	p.mu.Lock()
	w := p.slotWinLocked("slot_a")
	for i := 0; i < 50*6; i++ { // 6 分钟窗口共 50 rpm × 6 min
		w.Add(WinClock())
	}
	p.mu.Unlock()

	// 冷账号（uidRPM=0，无历史）可进 slot_a
	port, _, err := p.Assign("cold_user", "cn")
	if err != nil {
		t.Fatal(err)
	}
	if port != 31080 {
		t.Fatalf("冷账号应进 slot_a，实际端口 %d", port)
	}

	// 热账号：uid 窗口 30 rpm → 剩余预算 2 容不下，应落到新槽（指向 b）
	// 先把 hot_user 的窗口加热
	p.mu.Lock()
	hw := p.uidWinLocked("hot_user")
	for i := 0; i < 30*6; i++ {
		hw.Add(WinClock())
	}
	p.mu.Unlock()
	_, op, err := p.Assign("hot_user", "cn")
	if err != nil {
		t.Fatal(err)
	}
	if op == nil {
		t.Fatal("热账号应被预算拒绝后新建槽位")
	}
	if op.NodeSpec.ID != "b" {
		t.Fatalf("热账号新槽应指向无流量的 b，实际 %s", op.NodeSpec.ID)
	}
}

// TestCountRequestAccumulates 计数落到节点上。
func TestCountRequestAccumulates(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("a")})
	markHealthy(t, p, "a", 50)

	_, op, _ := p.Assign("u1", "cn")
	if op == nil {
		t.Fatal("expect add-slot op")
	}
	// u1 绑定在指向 a 的槽位 → 3 次请求计到 a
	for i := 0; i < 3; i++ {
		p.CountRequest("u1")
	}
	loads := p.NodeLoadsSnapshot()
	if loads["a"] != 3 {
		t.Fatalf("a 应计数 3，实际 %d", loads["a"])
	}
	// 未绑定账号不计
	p.CountRequest("nobody")
	if p.NodeLoadsSnapshot()["a"] != 3 {
		t.Fatal("未绑定账号不应计数")
	}
}

// TestRebalanceMigratesFromHeavyToLight 流量倾斜时迁移：
// a 节点窗口 600 rpm、b 节点 0 → a 上的账号应迁到 b。
func TestRebalanceMigratesFromHeavyToLight(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("a"), probedNode("b")})
	markHealthy(t, p, "a", 100)
	markHealthy(t, p, "b", 100)

	p.mu.Lock()
	p.slots["slot_a"] = &Slot{ID: "slot_a", Port: 31080, NodeID: "a", Region: "cn", Since: time.Now()}
	p.slots["slot_b"] = &Slot{ID: "slot_b", Port: 31081, NodeID: "b", Region: "cn", Since: time.Now()}
	p.bindngs["u1"] = Binding{SlotID: "slot_a", Since: time.Now()}
	p.mu.Unlock()
	// a 窗口 600 rpm（热），b 0：差 > 预算(60) → 应迁移 u1
	seedWindow(p, "a", 3600)

	ops := p.PlanRebalance()
	if len(ops) == 0 {
		t.Fatal("流量倾斜 600v0 应有迁移方案")
	}
	n, _ := p.ApplyRebalance(ops)
	if n == 0 {
		t.Fatal("应有迁移")
	}
	p.mu.Lock()
	moved := p.bindngs["u1"].SlotID == "slot_b"
	p.mu.Unlock()
	if !moved {
		t.Fatal("u1 应迁到 slot_b")
	}
}

// TestRebalanceMigratesHottestAccount 迁移受害者选择：重端有多个账号时，
// 迁 uidRPM 最大的（迁冷账号不降压）。
func TestRebalanceMigratesHottestAccount(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("a"), probedNode("b")})
	markHealthy(t, p, "a", 100)
	markHealthy(t, p, "b", 100)

	p.mu.Lock()
	p.slots["slot_a"] = &Slot{ID: "slot_a", Port: 31080, NodeID: "a", Region: "cn", Since: time.Now()}
	p.slots["slot_b"] = &Slot{ID: "slot_b", Port: 31081, NodeID: "b", Region: "cn", Since: time.Now()}
	// a 上两个账号：cold（10 rpm）与 hot（300 rpm）
	p.bindngs["cold"] = Binding{SlotID: "slot_a", Since: time.Now()}
	p.bindngs["hot"] = Binding{SlotID: "slot_a", Since: time.Now()}
	// 热账号的 uid 窗口
	hw := p.uidWinLocked("hot")
	for i := 0; i < 300*6; i++ {
		hw.Add(WinClock())
	}
	cw := p.uidWinLocked("cold")
	for i := 0; i < 10*6; i++ {
		cw.Add(WinClock())
	}
	// 节点 a 的窗口流量 = 两账号之和
	aw := p.nodeWinLocked("a")
	for i := 0; i < 310*6; i++ {
		aw.Add(WinClock())
	}
	p.mu.Unlock()

	ops := p.PlanRebalance()
	if len(ops) == 0 {
		t.Fatal("a=310rpm vs b=0 应有迁移")
	}
	if ops[0].UID != "hot" {
		t.Fatalf("应迁最热的账号 hot，实际 %s", ops[0].UID)
	}
}

// TestRebalanceCooldownBlocksImmediateRemigrate 迁移后 10 分钟冷却：
// 刚迁过的账号不参与下一轮迁移。
func TestRebalanceCooldownBlocksImmediateRemigrate(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("a"), probedNode("b")})
	markHealthy(t, p, "a", 100)
	markHealthy(t, p, "b", 100)

	// 冷却记录是包级全局（进程生命周期语义），清掉其他测试的残留
	p.mu.Lock()
	for k := range lastMovedAt {
		delete(lastMovedAt, k)
	}
	p.mu.Unlock()

	p.mu.Lock()
	p.slots["slot_a"] = &Slot{ID: "slot_a", Port: 31080, NodeID: "a", Region: "cn", Since: time.Now()}
	p.slots["slot_b"] = &Slot{ID: "slot_b", Port: 31081, NodeID: "b", Region: "cn", Since: time.Now()}
	p.bindngs["cooldown_u1"] = Binding{SlotID: "slot_a", Since: time.Now()}
	p.mu.Unlock()
	seedWindow(p, "a", 3600)

	// 第一轮：迁移
	ops := p.PlanRebalance()
	if len(ops) != 1 {
		t.Fatalf("应有 1 个迁移，实际 %d", len(ops))
	}
	p.ApplyRebalance(ops)

	// 冷却期内：u1 已在 slot_b（b 现在是重端），再倾斜回去也不会迁 u1
	seedWindow(p, "b", 3600)
	if ops2 := p.PlanRebalance(); len(ops2) != 0 {
		t.Fatalf("冷却期内 u1 不应再迁，实际 %+v", ops2)
	}
}

// TestRebalanceKeepsHotAccountHot 流量是账号自己打的，迁走账号 = 迁走流量：
// 1 热账号独占流量时迁移无收益（反向超调），不应迁移。
func TestRebalanceKeepsHotAccountHot(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("a"), probedNode("b")})
	markHealthy(t, p, "a", 100)
	markHealthy(t, p, "b", 100)

	p.mu.Lock()
	p.slots["slot_a"] = &Slot{ID: "slot_a", Port: 31080, NodeID: "a", Region: "cn", Since: time.Now()}
	p.slots["slot_b"] = &Slot{ID: "slot_b", Port: 31081, NodeID: "b", Region: "cn", Since: time.Now()}
	p.bindngs["u1"] = Binding{SlotID: "slot_a", Since: time.Now()} // 热账号在 a
	p.bindngs["u2"] = Binding{SlotID: "slot_b", Since: time.Now()}
	// u1 热账号：uid 窗口 200 rpm；节点窗口 a=200 b=20
	hw := p.uidWinLocked("u1")
	for i := 0; i < 200*6; i++ {
		hw.Add(WinClock())
	}
	p.mu.Unlock()
	seedWindow(p, "a", 200*6)
	seedWindow(p, "b", 20*6)

	// 迁 u1(200rpm) 到 b：a=0、b=220 → 反向超调比原来（200 vs 20）更糟 → 不迁
	if ops := p.PlanRebalance(); len(ops) != 0 {
		t.Fatalf("热账号独占流量时迁移无收益，不应迁移，实际 %+v", ops)
	}
}

// TestCompactSlotsMergesSameNodeSlots 同节点多槽合并 + 空槽删除。
func TestCompactSlotsMergesSameNodeSlots(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("a"), probedNode("b")})
	markHealthy(t, p, "a", 100)
	markHealthy(t, p, "b", 100)

	p.mu.Lock()
	// 节点 a 上 3 个槽（历史散乱），b 上 1 个空槽
	p.slots["s1"] = &Slot{ID: "s1", Port: 31080, NodeID: "a", Region: "cn", Since: time.Now()}
	p.slots["s2"] = &Slot{ID: "s2", Port: 31081, NodeID: "a", Region: "cn", Since: time.Now()}
	p.slots["s3"] = &Slot{ID: "s3", Port: 31082, NodeID: "a", Region: "cn", Since: time.Now()}
	p.slots["s4"] = &Slot{ID: "s4", Port: 31083, NodeID: "b", Region: "cn", Since: time.Now()}
	p.bindngs["u1"] = Binding{SlotID: "s1", Since: time.Now()}
	p.bindngs["u2"] = Binding{SlotID: "s2", Since: time.Now()}
	p.bindngs["u3"] = Binding{SlotID: "s2", Since: time.Now()}
	p.mu.Unlock()

	merged, removed, _, removeOps := p.CompactSlots()
	if merged != 1 {
		t.Fatalf("应合并 1 个账号（u1 从 s1 → s2；u2/u3 已在 keep 槽），实际 %d", merged)
	}
	// s1(多余)、s3(空)、s4(空) 删；保留 s2。共删 3 槽
	if removed != 3 {
		t.Fatalf("应删除 3 个槽（s1/s3/s4），实际 %d", removed)
	}
	if len(removeOps) != removed {
		t.Fatalf("removeOps 应与删除数一致：%d vs %d", len(removeOps), removed)
	}
	// 验证：节点 a 只剩 1 个槽，u1/u2/u3 都在它上面
	p.mu.Lock()
	var aSlots []*Slot
	for _, s := range p.slots {
		if s.NodeID == "a" {
			aSlots = append(aSlots, s)
		}
	}
	sameSlot := p.bindngs["u1"].SlotID == p.bindngs["u2"].SlotID &&
		p.bindngs["u2"].SlotID == p.bindngs["u3"].SlotID
	p.mu.Unlock()
	if len(aSlots) != 1 {
		t.Fatalf("节点 a 应只剩 1 个槽，实际 %d", len(aSlots))
	}
	if !sameSlot {
		t.Fatal("u1/u2/u3 应在同一个槽上")
	}
}

// TestAssignSingleSlotPerNode 修复验证：并入优先，同节点不重复建槽。
func TestAssignSingleSlotPerNode(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("a"), probedNode("b")})
	markHealthy(t, p, "a", 100)
	markHealthy(t, p, "b", 120)

	// apn=30：10 个账号应全并入第一个槽（不建新槽）
	p.mu.Lock()
	p.store.st.Rules.AccountsPerNode = 30
	p.mu.Unlock()
	for i := 0; i < 10; i++ {
		if _, _, err := p.Assign(fmt.Sprintf("u%d", i), "cn"); err != nil {
			t.Fatalf("Assign u%d: %v", i, err)
		}
	}
	p.mu.Lock()
	n := len(p.slots)
	p.mu.Unlock()
	if n > 1 {
		t.Fatalf("10 个账号 apn=30 应只建 1 个槽（并入优先），实际 %d", n)
	}
}

// TestRebalanceNoopWhenBalanced 窗口流量接近时不迁移（防抖动）。
func TestRebalanceNoopWhenBalanced(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("a"), probedNode("b")})
	markHealthy(t, p, "a", 100)
	markHealthy(t, p, "b", 100)

	p.mu.Lock()
	p.slots["slot_a"] = &Slot{ID: "slot_a", Port: 31080, NodeID: "a", Region: "cn", Since: time.Now()}
	p.slots["slot_b"] = &Slot{ID: "slot_b", Port: 31081, NodeID: "b", Region: "cn", Since: time.Now()}
	p.bindngs["u1"] = Binding{SlotID: "slot_a", Since: time.Now()}
	p.bindngs["u2"] = Binding{SlotID: "slot_b", Since: time.Now()}
	p.mu.Unlock()
	// a=12rpm b=10rpm：差 2 << 预算 60，比值 1.2 < 5 → 不迁
	seedWindow(p, "a", 12*6)
	seedWindow(p, "b", 10*6)

	if ops := p.PlanRebalance(); len(ops) != 0 {
		t.Fatalf("已均衡（12 vs 10 rpm）不应迁移，实际 %+v", ops)
	}
}

// TestRebalanceSpreadsAcrossNodes 17 账号挤在 1 个节点、10 个候选节点
// → v3 按窗口流量触发：n00 显著超载时迁最热账号到最轻节点。
// v3 每 region 每轮最多迁 1 个（窗口信号滞后，多轮收敛）。
func TestRebalanceSpreadsAcrossNodes(t *testing.T) {
	p := newTestPool(t)
	var specs []NodeSpec
	for i := 0; i < 10; i++ {
		specs = append(specs, probedNode(fmt.Sprintf("n%02d", i)))
	}
	p.SetNodes(specs)
	for i := 0; i < 10; i++ {
		markHealthy(t, p, fmt.Sprintf("n%02d", i), 100)
	}

	p.mu.Lock()
	p.store.st.Rules.AccountsPerNode = 30
	p.slots["slot_a"] = &Slot{ID: "slot_a", Port: 31080, NodeID: "n00", Region: "cn", Since: time.Now()}
	for i := 1; i <= 17; i++ {
		p.bindngs[fmt.Sprintf("u%02d", i)] = Binding{SlotID: "slot_a", Since: time.Now()}
	}
	p.mu.Unlock()
	// n00 窗口流量显著超载（17 账号合力 200 rpm），其余节点零
	seedWindow(p, "n00", 200*6)

	ops := p.PlanRebalance()
	if len(ops) != 1 {
		t.Fatalf("应有 1 个迁移（v3 单轮单账号），实际 %d", len(ops))
	}
	n, addOps := p.ApplyRebalance(ops)
	if n != 1 {
		t.Fatal("应迁移 1 个账号")
	}
	if len(addOps) == 0 {
		t.Fatal("目标节点没有既有槽位，应产生新建槽位操作")
	}
	// 迁移目标应是最轻节点（非 n00，且窗口为零）
	if ops[0].ToNode == "n00" {
		t.Fatal("不应迁回原节点")
	}
}

// TestAssignWithoutProbe 启动窗口期：未测速节点（静态筛选通过）立即可分配。
// 修正"首轮隔离"造成启用后到首轮测速前分配不到节点的问题。
func TestAssignWithoutProbe(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("a"), probedNode("b")})
	// 注意：不调 markHealthy——两个节点都未测速

	port, op, err := p.Assign("u1", "cn")
	if err != nil {
		t.Fatalf("未测速节点应可分配（静态筛选通过）：%v", err)
	}
	if op == nil || port == 0 {
		t.Fatalf("应建新槽位：port=%d op=%v", port, op)
	}
	// 第二个账号也应能分配（可能并入同一槽位或建新槽位——都合法）
	_, _, err = p.Assign("u2", "cn")
	if err != nil {
		t.Fatalf("第二个账号分配失败: %v", err)
	}
}

// TestAssignExcludeStillFilters 静态筛选仍生效：黑名单关键词照样排除未测速节点。
func TestAssignExcludeStillFilters(t *testing.T) {
	p := newTestPool(t)
	bad := probedNode("a")
	bad.Name = "剩余流量勿用"
	p.SetNodes([]NodeSpec{bad, probedNode("b")})
	p.mu.Lock()
	p.store.st.Rules.ExcludeKeywords = []string{"剩余流量"}
	p.mu.Unlock()

	_, op, err := p.Assign("u1", "cn")
	if err != nil {
		t.Fatal(err)
	}
	if op == nil {
		t.Fatal("expect new slot")
	}
	if op.NodeSpec.ID == "a" {
		t.Fatal("黑名单节点不应被分配，即使未测速")
	}
}

// TestAssignUnhealthyExcluded 有健康数据的坏节点仍被排除（测速后的修正）。
func TestAssignUnhealthyExcluded(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("a"), probedNode("b")})
	markHealthy(t, p, "a", 100)
	markHealthy(t, p, "b", 100)
	// a 变不健康
	p.mu.Lock()
	p.store.st.Health["a"].Unhealthy = true
	p.store.st.Health["a"].UnhealthyUntil = time.Now().Add(10 * time.Minute)
	p.mu.Unlock()

	_, op, err := p.Assign("u1", "cn")
	if err != nil {
		t.Fatal(err)
	}
	if op.NodeSpec.ID == "a" {
		t.Fatal("不健康节点不应被分配")
	}
}
