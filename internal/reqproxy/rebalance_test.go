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

// TestAssignLoadBalanceNewAccounts 新账号应按节点负载分到轻载节点：
// node-A 已有 100 请求、node-B 0 请求 → 新账号的新槽位应指向 node-B。
func TestAssignLoadBalanceNewAccounts(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("a"), probedNode("b")})
	markHealthy(t, p, "a", 100)
	markHealthy(t, p, "b", 100)

	// 预置负载：a=100, b=0
	p.mu.Lock()
	p.loads["a"] = 100
	p.mu.Unlock()

	// 第一个账号：无既有槽位，选节点 → b 更轻
	_, op, err := p.Assign("u1", "cn")
	if err != nil || op == nil {
		t.Fatalf("assign: %v %v", err, op)
	}
	if op.NodeSpec.ID != "b" {
		t.Fatalf("新槽位应指向轻载节点 b，实际 %s", op.NodeSpec.ID)
	}
}

// TestAssignJoinsLightestSlot 并入槽位时选节点负载最轻的槽位。
func TestAssignJoinsLightestSlot(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("a"), probedNode("b")})
	markHealthy(t, p, "a", 100)
	markHealthy(t, p, "b", 100)

	// 手工建两个槽位（模拟既有状态）
	p.mu.Lock()
	p.slots["slot_a"] = &Slot{ID: "slot_a", Port: 31080, NodeID: "a", Region: "cn", Since: time.Now()}
	p.slots["slot_b"] = &Slot{ID: "slot_b", Port: 31081, NodeID: "b", Region: "cn", Since: time.Now()}
	p.loads["a"] = 500
	p.loads["b"] = 10
	p.mu.Unlock()

	port, _, err := p.Assign("u1", "cn")
	if err != nil {
		t.Fatal(err)
	}
	if port != 31081 {
		t.Fatalf("应并入轻载槽位 slot_b(31081)，实际端口 %d", port)
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

// TestRebalanceMigratesFromHeavyToLight 账号数倾斜时迁移（3v0 → 2v1），
// 绑定换、既有端口不变。
func TestRebalanceMigratesFromHeavyToLight(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("a"), probedNode("b")})
	markHealthy(t, p, "a", 100)
	markHealthy(t, p, "b", 100)

	p.mu.Lock()
	p.slots["slot_a"] = &Slot{ID: "slot_a", Port: 31080, NodeID: "a", Region: "cn", Since: time.Now()}
	p.slots["slot_b"] = &Slot{ID: "slot_b", Port: 31081, NodeID: "b", Region: "cn", Since: time.Now()}
	// a 上 3 个账号，b 上 0 个 → 摊到 2v1
	for _, uid := range []string{"u1", "u2", "u3"} {
		p.bindngs[uid] = Binding{SlotID: "slot_a", Since: time.Now()}
	}
	p.mu.Unlock()

	ops := p.PlanRebalance()
	if len(ops) == 0 {
		t.Fatal("账号倾斜 3v0 应有迁移方案")
	}
	n, _ := p.ApplyRebalance(ops)
	if n == 0 {
		t.Fatal("应有迁移")
	}
	// 验证摊开：a 上账号数 - b 上账号数 ≤ 1
	p.mu.Lock()
	accA, accB := 0, 0
	for _, b := range p.bindngs {
		switch p.slots[b.SlotID].NodeID {
		case "a":
			accA++
		case "b":
			accB++
		}
	}
	p.mu.Unlock()
	if accA-accB > 1 {
		t.Fatalf("未摊平：a=%d b=%d", accA, accB)
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
	p.loads["a"] = 1000 // u1 打出来的
	p.loads["b"] = 100
	p.mu.Unlock()

	// 1v1 账号 + 流量跟人走：迁 u1 到 b 会让 b 更重（210 vs 100），无收益
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

// TestRebalanceNoopWhenBalanced 负载接近时不迁移（防抖动）。
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
	p.loads["a"] = 120
	p.loads["b"] = 100
	p.mu.Unlock()

	// 1v1 账号均衡 + 请求差（20/10=2）远小于 1 账号当量 → 不迁
	if ops := p.PlanRebalance(); len(ops) != 0 {
		t.Fatalf("已均衡（1v1 账号）不应迁移，实际 %+v", ops)
	}
}

// TestRebalanceSpreadsAcrossNodes 17 账号挤在 1 个节点、10 个候选节点
// → 再平衡应主动摊开到多个节点（新建槽位），不再受 apn=30 打包影响。
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
	// apn=30（用户线上值）：v1 会把 17 个账号打包进 1 个节点；v2 应摊开
	p.store.st.Rules.AccountsPerNode = 30
	p.slots["slot_a"] = &Slot{ID: "slot_a", Port: 31080, NodeID: "n00", Region: "cn", Since: time.Now()}
	for i := 1; i <= 17; i++ {
		p.bindngs[fmt.Sprintf("u%02d", i)] = Binding{SlotID: "slot_a", Since: time.Now()}
	}
	p.mu.Unlock()

	ops := p.PlanRebalance()
	if len(ops) == 0 {
		t.Fatal("17 账号 / 10 节点应摊开")
	}
	n, addOps := p.ApplyRebalance(ops)
	if n == 0 {
		t.Fatal("应有迁移")
	}
	if len(addOps) == 0 {
		t.Fatal("目标节点没有既有槽位，应产生新建槽位操作")
	}
	// 验证摊开效果：最重节点账号数 ≤ 最轻节点 + 1
	p.mu.Lock()
	acc := map[string]int{}
	for _, b := range p.bindngs {
		if s := p.slots[b.SlotID]; s != nil {
			acc[s.NodeID]++
		}
	}
	p.mu.Unlock()
	maxN, minN := 0, 1<<30
	for _, v := range acc {
		if v > maxN {
			maxN = v
		}
		if v < minN {
			minN = v
		}
	}
	if maxN-minN > 1 {
		t.Fatalf("摊开不均：max=%d min=%d", maxN, minN)
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
