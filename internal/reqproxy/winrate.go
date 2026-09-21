// WinRate：滑动窗口请求速率（节点/槽位/账号三层）。
//
// 旧均衡信号是进程生命周期的累计请求数（p.loads），两个盲区：
//  1. 历史包袱——昨天热过的节点今天很闲，但累计数永远把它排到最后；
//     重启后全零，前几分钟信号失真。它度量"过去的热度"，不是"现在的热度"。
//  2. 只到节点不到槽位——同一节点上多个槽位无差别，热连接挤同一个 inbound。
//
// 实现：环形桶（默认 6 桶 × 1 分钟 = 近 6 分钟窗口）。
// 计数 O(1)；Value() 归零窗口内过期桶——锁内调用方保证（Pool.mu）。
// 桶推进是惰性的：读/写时才归零中间跨过的桶，无后台 goroutine。
package reqproxy

import "time"
type WinRate struct {
	bucketLen time.Duration // 单桶时长
	n         int           // 桶数（窗口 = bucketLen × n）
	cur       int           // 当前桶下标（环形）
	curStart  time.Time     // 当前桶起始时刻
	counts    []int64       // 环形计数
	last      int64         // 跨桶溢出保护前的累计（可重启语义，不做持久化）
}

// NewWinRate 构建窗口。bucketLen/n 非法时退化为 60s×6。
func NewWinRate(bucketLen time.Duration, n int) *WinRate {
	if bucketLen <= 0 {
		bucketLen = time.Minute
	}
	if n <= 0 {
		n = 6
	}
	return &WinRate{
		bucketLen: bucketLen,
		n:         n,
		counts:    make([]int64, n),
	}
}

// advance 推进到 t 所在桶（归零路过的桶）。调用方须持锁。
// 首次调用做懒初始化（curStart 零值 → 以 t 为起点），避免构造时刻
// 与首个事件时刻不同源（测试注入历史时钟）。
func (w *WinRate) advance(t time.Time) {
	if w.curStart.IsZero() {
		w.curStart = t.Truncate(w.bucketLen)
		return
	}
	elapsed := t.Sub(w.curStart)
	if elapsed < w.bucketLen {
		return // 仍在当前桶
	}
	steps := int(elapsed / w.bucketLen)
	if steps >= w.n {
		// 跨越整个窗口：全部归零（长期无流量后的重新开始）
		for i := range w.counts {
			w.counts[i] = 0
		}
		w.cur = 0
		w.curStart = t.Truncate(w.bucketLen)
		return
	}
	for i := 0; i < steps; i++ {
		w.cur = (w.cur + 1) % w.n
		w.counts[w.cur] = 0
	}
	w.curStart = w.curStart.Add(time.Duration(steps) * w.bucketLen)
}

// Add 记一次事件。调用方须持锁。
func (w *WinRate) Add(t time.Time) {
	w.advance(t)
	w.counts[w.cur]++
	w.last++
}

// Value 窗口内事件总数（折算成每分钟速率）。调用方须持锁。
// 返回 (窗口总数, 每分钟速率)。
func (w *WinRate) Value(t time.Time) (total int64, rpm float64) {
	w.advance(t)
	for _, c := range w.counts {
		total += c
	}
	windowMin := float64(w.bucketLen.Minutes()) * float64(w.n)
	if windowMin <= 0 {
		windowMin = 6
	}
	return total, float64(total) / windowMin
}

// RPM 窗口内每分钟速率（便捷封装）。
func (w *WinRate) RPM(t time.Time) float64 {
	_, rpm := w.Value(t)
	return rpm
}

// WinClock 窗口时钟：测试注入用（生产用 time.Now）。
var WinClock = func() time.Time { return time.Now() }
