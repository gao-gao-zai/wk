package reqproxy

import (
	"testing"
	"time"
)

// ---- Rules 归一化与校验 ----

func TestNormalizeTTFBRulesDefaults(t *testing.T) {
	st := DefaultState()
	// 模拟旧 state：全部字段缺失（零值）。
	st.Rules = Rules{}
	st.Normalize()
	if st.Rules.TTFBGroup != "" {
		t.Fatalf("TTFBGroup 默认应为空，got %q", st.Rules.TTFBGroup)
	}
	if st.Rules.TTFBIntervalSeconds != 60 {
		t.Fatalf("TTFBIntervalSeconds 默认 60，got %d", st.Rules.TTFBIntervalSeconds)
	}
	if st.Rules.TTFBConcurrency != 1 {
		t.Fatalf("TTFBConcurrency 默认 1，got %d", st.Rules.TTFBConcurrency)
	}
	if st.Rules.TTFBTimeoutSeconds != 60 {
		t.Fatalf("TTFBTimeoutSeconds 默认 60，got %d", st.Rules.TTFBTimeoutSeconds)
	}
}

func TestValidateTTFBRules(t *testing.T) {
	ok := Rules{TTFBIntervalSeconds: 60, TTFBConcurrency: 4, TTFBTimeoutSeconds: 90, TTFBModel: "glm-5.3"}
	if err := ValidateTTFBRules(ok); err != nil {
		t.Fatalf("合法参数不应报错: %v", err)
	}
	bad := []Rules{
		{TTFBIntervalSeconds: 4000, TTFBConcurrency: 1, TTFBTimeoutSeconds: 60},                                     // 间隔越界
		{TTFBIntervalSeconds: 60, TTFBConcurrency: 9, TTFBTimeoutSeconds: 60},                                       // 并发越界
		{TTFBIntervalSeconds: 60, TTFBConcurrency: 1, TTFBTimeoutSeconds: 200},                                      // 超时越界
		{TTFBIntervalSeconds: 60, TTFBConcurrency: 1, TTFBTimeoutSeconds: 60, TTFBModel: string(make([]byte, 100))}, // 模型过长
	}
	for i, r := range bad {
		if err := ValidateTTFBRules(r); err == nil {
			t.Fatalf("case %d 应报错", i)
		}
	}
}

// ---- MarkTTFB 语义 ----

func TestMarkTTFBFailStreakAndUnhealthy(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("n1")})
	now := time.Now()
	// 连续 3 次节点侧失败 → 摘除。
	p.MarkTTFB("n1", -1, "connect timeout", now)
	p.MarkTTFB("n1", -1, "connect timeout", now)
	p.MarkTTFB("n1", -1, "connect timeout", now)
	if hh := p.HealthOf0("n1"); !hh.Unhealthy || hh.FailStreak != 3 {
		t.Fatalf("3 次失败应摘除：unhealthy=%v streak=%d", hh.Unhealthy, hh.FailStreak)
	}
	// 成功 → 清零连败 + 取消摘除。
	p.MarkTTFB("n1", 8200, "", now)
	if hh := p.HealthOf0("n1"); hh.Unhealthy || hh.FailStreak != 0 || hh.TTFBMs != 8200 {
		t.Fatalf("成功应清零：unhealthy=%v streak=%d ttfb=%d", hh.Unhealthy, hh.FailStreak, hh.TTFBMs)
	}
}

func TestMarkTTFBSkippedDoesNotPunish(t *testing.T) {
	p := newTestPool(t)
	p.SetNodes([]NodeSpec{probedNode("n1")})
	now := time.Now()
	p.MarkTTFB("n1", -1, "节点侧失败", now)       // streak=1
	p.MarkTTFBSkipped("n1", -1, "额度用尽", now) // 账号侧：不计失败
	if hh := p.HealthOf0("n1"); hh.FailStreak != 1 {
		t.Fatalf("账号侧失败不应增加 FailStreak，got %d", hh.FailStreak)
	}
	if hh := p.HealthOf0("n1"); hh.TTFBError != "额度用尽" {
		t.Fatalf("展示字段应更新，got %q", hh.TTFBError)
	}
}

// HealthOf0 测试辅助：直接从 store 读节点健康。
func (p *Pool) HealthOf0(nodeID string) NodeHealth {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.store.st.Health[nodeID]; ok && h != nil {
		return *h
	}
	return NodeHealth{LatencyMs: -1}
}

// ---- probeNode 失败分类（无内核环境：构造分类断言的最小路径） ----

func TestProbeNodeAccountSideClassification(t *testing.T) {
	// 直接验证分类逻辑：4xx 账号类 → Skipped；连接错误 → 不 Skipped。
	// probeNode 全链路需要内核端口，这里用分类函数级测试代替（同一 switch）。
	cases := []struct {
		status  int
		body    string
		skipped bool
	}{
		{402, ``, true}, // ErrHardCredit
		{429, `{"code":14018,"msg":"额度已用尽"}`, true}, // ErrHardCredit
		{429, ``, true}, // ErrSoftRate
		{401, `{"code":12153,"msg":"Offline user session not found"}`, true}, // ErrSessionDead
		{500, `boom`, false},                            // ErrServer → 节点侧
		{400, `{"code":9999,"msg":"bad token"}`, false}, // ErrClient → 节点侧（请求形态）
	}
	for _, c := range cases {
		res := classifyProbeError(c.status, c.body)
		if res != c.skipped {
			t.Fatalf("status=%d body=%q: skipped=%v want %v", c.status, c.body, res, c.skipped)
		}
	}
}

// ---- Health 让位（自动首字开启 → HEAD 周期轮跳过）----

func TestHealthSkipRoundWhenTTFBGroupSet(t *testing.T) {
	p := newTestPool(t)
	h := NewHealth(DefaultHealthConfig(), p, nil)
	// 未注入：不让位。
	if h.shouldSkip() {
		t.Fatal("未注入 skipRound 时不应让位")
	}
	// 注入：分组非空 → 让位；空 → 不让位。
	h.SetSkipRound(func() bool { return true })
	if !h.shouldSkip() {
		t.Fatal("注入返回 true 时应让位")
	}
	h.SetSkipRound(func() bool { return false })
	if h.shouldSkip() {
		t.Fatal("注入返回 false 时不应让位")
	}
}

// Manager 注入的让位判断（读规则里 TTFBGroup）。
func TestManagerHealthYieldWiring(t *testing.T) {
	m := newScanManager(t, nil)
	yield := func() bool {
		var group string
		m.store.View(func(st *State) { group = st.Rules.TTFBGroup })
		return group != ""
	}
	if yield() {
		t.Fatal("TTFBGroup 空时不应让位")
	}
	m.store.Update(func(st *State) {
		st.Rules.TTFBGroup = "g"
		st.Rules.NormalizeTTFB()
	})
	if !yield() {
		t.Fatal("TTFBGroup 非空时应让位")
	}
}

// classifyProbeError 把 upstream.Classify 映射为「账号侧失败」布尔。
// 与 probeNode 内的 switch 保持同一套判定（测试锁定映射不被改坏）。
func classifyProbeError(status int, body string) bool {
	// 与 probeNode 相同的 import 路径；抽出来是给上面的表驱动测试用。
	return probeErrIsAccountSide(status, body)
}

// ---- scanOneTick（自动扫描循环的一拍）----

// fakeScanSource 固定账号清单 + 记录被问到的分组。
type fakeScanSource struct {
	groups map[string][]TTFBAccount
	asked  []string
}

func (f *fakeScanSource) AccountsForGroup(group string) []TTFBAccount {
	f.asked = append(f.asked, group)
	return f.groups[group]
}

// newScanManager 构建最小 Manager（仅 pool + ttfb，无内核/订阅）。
func newScanManager(t *testing.T, src TTFBAccountSource) *Manager {
	t.Helper()
	store, err := NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{store: store}
	m.pool = NewPool(store, 31080, 31999)
	m.ttfb = NewTTFBProber(m.pool, nil)
	m.ttfb.SetAccountSource(src)
	return m
}

func TestScanOneTickBatchAndCursor(t *testing.T) {
	src := &fakeScanSource{groups: map[string][]TTFBAccount{
		"g": {{Auth: nil, Region: "cn"}}, // Auth nil → 账号侧跳过，不动健康
	}}
	m := newScanManager(t, src)
	m.pool.SetNodes([]NodeSpec{probedNode("b"), probedNode("a"), probedNode("c")})

	rules := Rules{TTFBGroup: "g", TTFBConcurrency: 2, TTFBTimeoutSeconds: 5}
	cursor, acctIdx, roundDone, ok, fail, skip := 0, 0, false, 0, 0, 0
	cursor, acctIdx, roundDone, ok, fail, skip = m.scanOneTick(rules, cursor, acctIdx, roundDone, ok, fail, skip)
	// 一批 2 个节点（a、b），全部账号侧跳过（nil auth），健康记录不动。
	if cursor != 2 {
		t.Fatalf("游标应推进到 2，got %d", cursor)
	}
	if ok != 0 || fail != 0 || skip != 2 {
		t.Fatalf("nil auth 应记跳过：ok=%d fail=%d skip=%d", ok, fail, skip)
	}
	// 第二批：c（1 个节点），圈尾 → cursor 回卷、轮内计数清零（汇总已发事件）。
	cursor, acctIdx, roundDone, ok, fail, skip = m.scanOneTick(rules, cursor, acctIdx, roundDone, ok, fail, skip)
	if cursor != 0 {
		t.Fatalf("圈尾游标应回卷到 0，got %d", cursor)
	}
	if ok != 0 || fail != 0 || skip != 0 {
		t.Fatalf("圈尾应清零轮内计数：ok=%d fail=%d skip=%d", ok, fail, skip)
	}
	// c 的健康记录应已被本拍更新（账号侧跳过 → 展示字段写入，无 FailStreak）。
	if hh := m.pool.HealthOf0("c"); hh.TTFBChecked.IsZero() || hh.FailStreak != 0 {
		t.Fatalf("c 应有 TTFB 记录且不惩罚：checked=%v streak=%d", hh.TTFBChecked, hh.FailStreak)
	}
}

func TestScanOneTickYieldsToManualRun(t *testing.T) {
	src := &fakeScanSource{groups: map[string][]TTFBAccount{
		"g": {{Auth: nil, Region: "cn"}},
	}}
	m := newScanManager(t, src)
	m.pool.SetNodes([]NodeSpec{probedNode("a")})

	// 模拟手动测速在跑。
	m.ttfb.mu.Lock()
	m.ttfb.running = true
	m.ttfb.mu.Unlock()

	rules := Rules{TTFBGroup: "g", TTFBConcurrency: 1, TTFBTimeoutSeconds: 5}
	cursor, acctIdx, roundDone, ok, fail, skip := 0, 0, false, 0, 0, 0
	cursor, acctIdx, roundDone, ok, fail, skip = m.scanOneTick(rules, cursor, acctIdx, roundDone, ok, fail, skip)
	// 让路：游标不动、没有探测发生（账号来源没被问过）。
	if cursor != 0 || ok+fail+skip != 0 {
		t.Fatalf("手动测速在跑时应让路：cursor=%d ok=%d fail=%d skip=%d", cursor, ok, fail, skip)
	}
	if len(src.asked) != 0 {
		t.Fatalf("让路时不应取账号，asked=%v", src.asked)
	}
}

func TestScanOneTickNoAccountsKeepsHealth(t *testing.T) {
	// 分组无账号：一拍跳过，节点健康记录保持不变。
	src := &fakeScanSource{groups: map[string][]TTFBAccount{}}
	m := newScanManager(t, src)
	m.pool.SetNodes([]NodeSpec{probedNode("a")})
	markHealthy(t, m.pool, "a", 100)

	before := m.pool.HealthOf0("a")
	rules := Rules{TTFBGroup: "ghost", TTFBConcurrency: 1, TTFBTimeoutSeconds: 5}
	m.scanOneTick(rules, 0, 0, false, 0, 0, 0)
	afterH := m.pool.HealthOf0("a")
	if before.LatencyMs != afterH.LatencyMs || before.FailStreak != afterH.FailStreak {
		t.Fatalf("无账号时健康记录不应变化：%+v → %+v", before, afterH)
	}
}
