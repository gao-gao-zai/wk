package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestSpendCreditsReducesEffectiveWeight 影子扣减的核心行为：消耗领先的号
// 有效余额下降 → credits ×10 权重项变小 → 被选概率下降。
// 用确定性随机源（r=0 恒选权重最高）断言：扣减后高快照号不再霸榜。
func TestSpendCreditsReducesEffectiveWeight(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "rich"})
	p.Add(&auth.Auth{UID: "poor"})
	p.SetCredits("rich", 1000)
	p.SetCredits("poor", 600)

	// 基线：rich 快照高 → 首选 rich。
	if got := p.Pick(); got == nil || got.UID != "rich" {
		t.Fatalf("baseline pick=%v, want rich", got)
	}

	// rich 消耗领先：影子扣减把有效余额打到 poor 之下。
	p.SpendCredits("rich", 500) // 有效余额 500 < poor 600

	// 现在 poor 权重最高 → 首选 poor（负反馈生效）。
	if got := p.Pick(); got == nil || got.UID != "poor" {
		t.Fatalf("after spend pick=%v, want poor (spent leader must lose weight)", got)
	}
}

// TestSpendCreditsClampAtZero 烧穿的号有效余额钳 0，不出现负权重比例；
// 全员烧穿时 credits 项退化，选择回落 idle+成功率继续轮换。
func TestSpendCreditsClampAtZero(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetCredits("a", 100)
	p.SetCredits("b", 100)

	// a 烧穿（spent 超过快照），b 只花一点。
	p.SpendCredits("a", 300)
	p.SpendCredits("b", 10)

	// b 有效余额 90 > a 的 0 → b 首选。
	if got := p.Pick(); got == nil || got.UID != "b" {
		t.Fatalf("pick=%v, want b (clamped-zero still loses to positive)", got)
	}
	// EffectiveCredits 观测口径也钳 0。
	if eff, ok := p.EffectiveCredits("a"); !ok || eff != 0 {
		t.Fatalf("effective(a)=%d ok=%v, want 0", eff, ok)
	}
}

// TestReconciliationResetsShadow 对账快照（SetCredits/SetCreditDetail）必须
// 清零影子账：真实快照已覆盖消耗，不清零会双重扣减。
func TestReconciliationResetsShadow(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u"})
	p.SetCredits("u", 1000)
	p.SpendCredits("u", 400)
	if eff, _ := p.EffectiveCredits("u"); eff != 600 {
		t.Fatalf("effective=%d, want 600", eff)
	}
	// 签到刷新：上游说还剩 550。影子账必须清零，550 直接生效。
	p.SetCredits("u", 550)
	if eff, _ := p.EffectiveCredits("u"); eff != 550 {
		t.Fatalf("after reconcile effective=%d, want 550 (shadow must reset)", eff)
	}
	// SetCreditDetail 同样清零。
	p.SpendCredits("u", 50)
	p.SetCreditDetail("u", CreditDetail{Remaining: 480})
	if eff, _ := p.EffectiveCredits("u"); eff != 480 {
		t.Fatalf("after detail reconcile effective=%d, want 480", eff)
	}
}

// TestSpendCreditsIgnoresInvalid 负值/零不扣（上游费用字段异常时不动账本）。
func TestSpendCreditsIgnoresInvalid(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u"})
	p.SetCredits("u", 100)
	p.SpendCredits("u", -5)
	p.SpendCredits("u", 0)
	if eff, _ := p.EffectiveCredits("u"); eff != 100 {
		t.Fatalf("effective=%d, want 100 (invalid spends ignored)", eff)
	}
	// 不存在的账号：无操作、不 panic。
	p.SpendCredits("ghost", 10)
}

// TestShadowBalancingConverges 负反馈的方向性验证（确定性断言，不依赖
// 随机模拟的阈值）：x 消耗领先 40% 时，其三因子权重必须低于 y——
// 即"被打得多 → 更少被选"的负反馈方向成立，马太效应（领先者权重更高）
// 不存在。收敛速度是比例式的（追赶需要时间），方向才是本质。
func TestShadowBalancingConverges(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "x"})
	p.Add(&auth.Auth{UID: "y"})
	p.SetCredits("x", 10000)
	p.SetCredits("y", 10000)
	p.SpendCredits("x", 4000) // x 领先消耗：有效 6000 vs 10000

	// idle/成功率因子两边一致（同样的使用/成功记录不存在），差异只来自
	// credits 项。站在同一时刻比较两个号的权重。
	now := time.Now()
	wx := p.weightOf(p.byUID["x"], 10000, now)
	wy := p.weightOf(p.byUID["y"], 10000, now)
	if wx >= wy {
		t.Fatalf("leader must weigh less: w(x)=%.2f w(y)=%.2f (spent leader must lose weight)", wx, wy)
	}
	// 追赶方向的动态验证：y 被打 200 次后，权重差必须缩小（y 的有效
	// 余额从 10000 掉到 8000，与 x 的差距从 4000 缩到 2000）。
	for i := 0; i < 200; i++ {
		p.SpendCredits("y", 10)
	}
	wx = p.weightOf(p.byUID["x"], 10000, now)
	wy = p.weightOf(p.byUID["y"], 10000, now)
	// credits 项差从 4.0（4000/10000×10）缩到 2.0（2000/10000×10）。
	if wy-wx > 2.1 {
		t.Fatalf("weight gap must narrow as y catches up: w(x)=%.2f w(y)=%.2f", wx, wy)
	}
	// 完全追平后权重相等（对称性）。
	p.SpendCredits("y", 2000) // y 有效 6000 = x
	wx = p.weightOf(p.byUID["x"], 10000, now)
	wy = p.weightOf(p.byUID["y"], 10000, now)
	if wx != wy {
		t.Fatalf("equal effective balance must weigh equal: w(x)=%.2f w(y)=%.2f", wx, wy)
	}
}
