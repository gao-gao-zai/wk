package reqproxy

import (
	"testing"
	"time"
)

func TestWinRateBuckets(t *testing.T) {
	w := NewWinRate(time.Minute, 6)
	t0 := time.Unix(1700000000, 0).Truncate(time.Minute)

	// 第 0 分钟 3 次
	for i := 0; i < 3; i++ {
		w.Add(t0)
	}
	// 第 1 分钟 2 次
	w.Add(t0.Add(1 * time.Minute))
	w.Add(t0.Add(1 * time.Minute))
	// 第 3 分钟 1 次（第 2 分钟空）
	w.Add(t0.Add(3 * time.Minute))

	total, rpm := w.Value(t0.Add(3*time.Minute + 30*time.Second))
	if total != 6 {
		t.Fatalf("窗口总数 = %d, 期望 6", total)
	}
	if rpm != 1.0 { // 6 次 / 6 分钟
		t.Fatalf("rpm = %f, 期望 1.0", rpm)
	}
}

func TestWinRateExpiry(t *testing.T) {
	w := NewWinRate(time.Minute, 6)
	t0 := time.Unix(1700000000, 0).Truncate(time.Minute)

	for i := 0; i < 10; i++ {
		w.Add(t0)
	}
	// 10 分钟后（超过整个窗口）：全部过期归零
	total, _ := w.Value(t0.Add(10 * time.Minute))
	if total != 0 {
		t.Fatalf("过期后窗口总数 = %d, 期望 0", total)
	}

	// 归零后重新计数
	w.Add(t0.Add(10 * time.Minute))
	total, _ = w.Value(t0.Add(10*time.Minute + 10*time.Second))
	if total != 1 {
		t.Fatalf("重新计数后 = %d, 期望 1", total)
	}
}

func TestWinRatePartialExpiry(t *testing.T) {
	w := NewWinRate(time.Minute, 6)
	t0 := time.Unix(1700000000, 0).Truncate(time.Minute)

	// 第 0 分钟 5 次
	for i := 0; i < 5; i++ {
		w.Add(t0)
	}
	// 第 4 分钟 3 次：第 1-3 分钟空桶被归零，第 0 分钟仍在窗口（6 桶：0..5）
	w.Add(t0.Add(4 * time.Minute))
	w.Add(t0.Add(4 * time.Minute))
	w.Add(t0.Add(4 * time.Minute))
	total, _ := w.Value(t0.Add(4*time.Minute + 30*time.Second))
	if total != 8 {
		t.Fatalf("部分过期后窗口总数 = %d, 期望 8（5+3）", total)
	}

	// 再过 2 分钟（第 6 分钟）：第 0 分钟的桶被挤出窗口，剩第 4 分钟的 3 次
	total, _ = w.Value(t0.Add(6*time.Minute + 30*time.Second))
	if total != 3 {
		t.Fatalf("窗口滑动后总数 = %d, 期望 3（只剩第4分钟的）", total)
	}
}

func TestWinRateDefaults(t *testing.T) {
	// 非法参数退化
	w := NewWinRate(0, 0)
	w.Add(time.Now())
	total, _ := w.Value(time.Now())
	if total != 1 {
		t.Fatalf("默认参数计数失败: %d", total)
	}
}
