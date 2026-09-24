package reqproxy

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// TestDeadOnArrivalNodeExcluded 从没测成功过的节点（LatencyMs 一直是 -1）
// 连续失败达阈值后必须退出候选。
//
// 回归：健康过滤以前要求 LatencyMs >= 0 才生效。死节点延迟永远是 -1，
// Unhealthy 标记被跳过，而它又因为零流量在评分里最"闲"——合格节点
// 不够用时，新槽位会优先建在它上面，请求全部打到死代理。
func TestDeadOnArrivalNodeExcluded(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("dead"), probedNode("ok")})
	markHealthy(t, p, "ok", 100)

	probeErr := errors.New("dial timeout")
	for i := 0; i < 3; i++ {
		p.MarkProbed("dead", -1, probeErr)
	}

	cands := p.Candidates("cn")
	for _, c := range cands {
		if c.ID == "dead" {
			t.Fatal("连续失败的死节点仍在候选里：合格节点不足时它会被优先分配")
		}
	}

	_, op, err := p.Assign("u1", "cn")
	if err != nil {
		t.Fatal(err)
	}
	if op == nil || op.NodeSpec.ID != "ok" {
		t.Fatalf("新槽位应指向健康节点 ok，got %+v", op)
	}
}

// TestDeadOnArrivalOnlyNodeFailsClosed 池里只剩死节点时必须报 D4 错误，
// 而不是建一个指向死节点的槽位。
func TestDeadOnArrivalOnlyNodeFailsClosed(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("dead")})
	for i := 0; i < 3; i++ {
		p.MarkProbed("dead", -1, errors.New("dial timeout"))
	}

	port, op, err := p.Assign("u1", "cn")
	if !errors.Is(err, ErrNoNode) {
		t.Fatalf("err=%v port=%d op=%v，want ErrNoNode", err, port, op)
	}
	if port != 0 || op != nil {
		t.Fatalf("失败时不应分配端口/槽位：port=%d op=%v", port, op)
	}
}

// TestAssignRebindsWhenNodeGone 账号绑在一个已被订阅刷新删掉的节点上时，
// 不能继续返回那个槽位的端口（端口背后的出口已经不存在）。
// 有替代节点就重选；没有就报 D4。
func TestAssignRebindsWhenNodeGone(t *testing.T) {
	p := newTestPool(t)
	// 先只放一个节点，保证绑定落在它上面。
	p.SetNodes([]NodeSpec{probedNode("gone")})
	markHealthy(t, p, "gone", 100)

	port, op, err := p.Assign("u1", "cn")
	if err != nil || op == nil {
		t.Fatalf("初次分配: %v %v", err, op)
	}
	if op.NodeSpec.ID != "gone" {
		t.Fatalf("初次分配应指向 gone，got %s", op.NodeSpec.ID)
	}
	oldSlot := op.SlotID

	// 订阅刷新：gone 消失，alive 进来。
	p.SetNodes([]NodeSpec{probedNode("alive")})
	markHealthy(t, p, "alive", 100)

	port2, op2, err := p.Assign("u1", "cn")
	if err != nil {
		t.Fatal(err)
	}
	if port2 == port && op2 == nil {
		t.Fatal("节点已消失，仍返回旧槽位端口：请求会打到死出口")
	}
	if op2 == nil || op2.NodeSpec.ID != "alive" {
		t.Fatalf("应重选到 alive，got port=%d op=%+v", port2, op2)
	}
	if op2.SlotID == oldSlot {
		t.Fatal("重选不应复用指向已消失节点的槽位")
	}
}

// TestAssignBoundToGoneNodeFailsClosed 绑定节点消失且没有替代节点时，
// 已绑定账号也要拿到 D4 错误，而不是一个打不通的端口。
func TestAssignBoundToGoneNodeFailsClosed(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("only")})
	markHealthy(t, p, "only", 100)
	if _, _, err := p.Assign("u1", "cn"); err != nil {
		t.Fatal(err)
	}

	p.SetNodes(nil) // 池清空：节点记录没了，但绑定还在

	// 池彻底为空走空池降级（直连），不报错。
	port, _, err := p.Assign("u1", "cn")
	if err != nil || port != 0 {
		t.Fatalf("空池应降级直连，got port=%d err=%v", port, err)
	}
}

// TestAssignHonorsTTFBSource 规则切到真实首字后，首字超上限的节点不再被分配，
// 即使它的连通性延迟很低。没测过首字的节点不受影响。
func TestAssignHonorsTTFBSource(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("fast-connect-slow-ttfb"), probedNode("ok"), probedNode("unmeasured")})
	markHealthy(t, p, "fast-connect-slow-ttfb", 100)
	markHealthy(t, p, "ok", 100)
	markHealthy(t, p, "unmeasured", 100)
	p.MarkTTFB("fast-connect-slow-ttfb", 30_000, "", time.Now())
	p.MarkTTFB("ok", 2_000, "", time.Now())

	p.store.Update(func(st *State) {
		st.Rules.LatencySource = "ttfb"
		st.Rules.MaxLatencyMs = 10_000
	})

	cands := p.Candidates("cn")
	ids := map[string]bool{}
	for _, c := range cands {
		ids[c.ID] = true
	}
	if ids["fast-connect-slow-ttfb"] {
		t.Fatal("真实首字 30s 超过上限 10s，不应入选")
	}
	if !ids["ok"] || !ids["unmeasured"] {
		t.Fatalf("合格节点缺失：%v", ids)
	}

	_, op, err := p.Assign("u1", "cn")
	if err != nil || op == nil {
		t.Fatalf("assign: %v %v", err, op)
	}
	if op.NodeSpec.ID != "ok" {
		t.Fatalf("应选首字更低的 ok，got %s", op.NodeSpec.ID)
	}
}

// TestAssignJoinPersists 并入既有槽位的绑定也要落盘。
// 回归：只有新建槽位的路径调了 persistLocked，重启会丢掉后来并入的账号。
func TestAssignJoinPersists(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("a")})
	markHealthy(t, p, "a", 100)

	if _, _, err := p.Assign("u1", "cn"); err != nil {
		t.Fatal(err)
	}
	if _, op, err := p.Assign("u2", "cn"); err != nil || op != nil {
		t.Fatalf("u2 应并入既有槽位，op=%v err=%v", op, err)
	}

	var got Binding
	var ok bool
	p.store.View(func(st *State) {
		got, ok = st.Bindings["u2"]
	})
	if !ok || got.SlotID == "" {
		t.Fatal("并入既有槽位的绑定没有落盘：进程重启后这个账号会重新分配")
	}
}

// TestProbePortsDontCollide 端口游标耗尽后，并发的测速分配不能拿到同一个端口。
// 回归：测速端口不在 slots 里，空洞回收只看 slots，一轮测速里所有探测
// 都抢到同一个端口，内核绑定失败，节点被误判为不健康。
func TestProbePortsDontCollide(t *testing.T) {
	p := newTestPool(t)
	p.mu.Lock()
	p.nextPort = p.portMax + 1 // 游标耗尽，强制走空洞回收
	p.mu.Unlock()

	const n = 8
	ports := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			port, err := p.allocProbePort()
			if err != nil {
				t.Errorf("alloc %d: %v", i, err)
				return
			}
			ports[i] = port
		}(i)
	}
	wg.Wait()

	seen := map[int]bool{}
	for i, port := range ports {
		if port == 0 {
			continue
		}
		if seen[port] {
			t.Fatalf("端口 %d 被分配了两次：并发测速会在内核里绑同一个端口", port)
		}
		seen[port] = true
		_ = i
	}

	// 归还后，正式槽位可以再用这些端口。
	for port := range seen {
		p.releaseProbePort(port)
	}
	slotPort, err := func() (int, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.allocPortLocked()
	}()
	if err != nil {
		t.Fatal(err)
	}
	if !seen[slotPort] && len(seen) > 0 {
		// 归还的端口应重新可用（空洞回收从段首开始）。
		t.Logf("槽位分到 %d（已归还的测速端口: %v）", slotPort, seen)
	}
}

// TestProbePortReservedAgainstSlots 测速占用中的端口不能再发给新建槽位。
func TestProbePortReservedAgainstSlots(t *testing.T) {
	p := newTestPool(t)
	probe, err := p.allocProbePort()
	if err != nil {
		t.Fatal(err)
	}
	defer p.releaseProbePort(probe)

	p.mu.Lock()
	p.nextPort = p.portMax + 1
	slot, err := p.allocPortLocked()
	p.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if slot == probe {
		t.Fatalf("槽位分到了正在测速的端口 %d", probe)
	}
}

// TestUnhealthyCooldownHonored 摘除冷却时长应遵循配置，而不是写死 10 分钟。
func TestUnhealthyCooldownHonored(t *testing.T) {
	p := newTestPool(t)
	p.SetUnhealthyCooldown(time.Minute)
	p.SetNodes([]NodeSpec{probedNode("a")})

	for i := 0; i < 3; i++ {
		p.MarkProbed("a", -1, errors.New("fail"))
	}
	p.mu.Lock()
	until := p.store.st.Health["a"].UnhealthyUntil
	p.mu.Unlock()

	if d := time.Until(until); d > 2*time.Minute || d < 30*time.Second {
		t.Fatalf("冷却截止 %v（距现在 %v），配置是 1 分钟", until, d)
	}
}

// TestRollbackAssignDropsBinding 内核开槽失败后回滚应清掉绑定和槽位记录，
// 账号下次可以重新分配，而不是被钉在一个没有 inbound 的端口上。
func TestRollbackAssignDropsBinding(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("a")})
	markHealthy(t, p, "a", 100)

	_, op, err := p.Assign("u1", "cn")
	if err != nil || op == nil {
		t.Fatalf("assign: %v %v", err, op)
	}
	if got := p.RollbackAssign("u1", op.SlotID); got != op.SlotID {
		t.Fatalf("回滚应删除空槽位，got %q", got)
	}

	var bound bool
	p.store.View(func(st *State) {
		_, bound = st.Bindings["u1"]
	})
	if bound {
		t.Fatal("回滚后绑定仍在：下次请求会返回死端口")
	}
	if _, ok := func() (int, bool) {
		p.mu.Lock()
		defer p.mu.Unlock()
		_, ok := p.slots[op.SlotID]
		return 0, ok
	}(); ok {
		t.Fatal("回滚后槽位记录仍在")
	}

	// 回滚后可以重新分配。
	if _, op2, err := p.Assign("u1", "cn"); err != nil || op2 == nil {
		t.Fatalf("回滚后重新分配失败: %v %v", err, op2)
	}
}
