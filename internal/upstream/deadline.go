package upstream

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// ErrStreamIdleTimeout：上游在空闲窗口内一个字节都没发（流卡住了）。
//
// 有意包装 context.DeadlineExceeded：sse.go 用 errors.Is 判定超时并归类为
// upstream_timeout，客户端因此收到可识别的错误帧 + [DONE]，而不是一句语焉不详的
// "连接被关闭"。包装而非新建 net.Error，可让既有超时分支（含 /responses 的
// response.failed 处理）无需改动就继续工作。
var ErrStreamIdleTimeout = fmt.Errorf("upstream stream idle: %w", context.DeadlineExceeded)

// ErrStreamTotalTimeout：整个流式请求超过总时长上限（兜底，防止跑飞的流长期占住租约）。
var ErrStreamTotalTimeout = fmt.Errorf("upstream stream exceeded total deadline: %w", context.DeadlineExceeded)

// ErrStreamHeaderTimeout：上游接受了连接却在窗口内没回响应头。
// 同样包装 context.DeadlineExceeded，好让 sse.go 的分类与 upstream_timeout 逻辑复用。
var ErrStreamHeaderTimeout = fmt.Errorf("upstream stream header wait exceeded: %w", context.DeadlineExceeded)

// timeoutBody 给上游响应体加两个约束：总时长上限与两次读取之间的空闲上限。
//
// 为什么需要它：http.Client.Timeout 是**整个请求**（含读完响应体）的墙钟上限，
// 用在流式响应上等于给回答长度设了死限——长回答会被硬性掐断。流式真正需要的是
// 另一个约束：上游卡住不发数据时尽快失败，而持续产出时可以一直流下去。
//
// 为什么不用 SetReadDeadline：net/http 只把读截止时间暴露给 http.ResponseWriter
// （ResponseController），客户端响应体不提供该能力。所以这里用看门狗 goroutine 在
// 超时时 Close 底层 body——实测这能在毫秒级解除一个正在阻塞的 Read。关闭本身产生的
// "use of closed network connection" 并不是超时错误，因此 Read 会把它替换成可识别的
// 哨兵。
//
// 为什么 Total 既走 ctx 又走看门狗：两者覆盖的阶段不同，缺一不可。
//
//	ctx         覆盖**等响应头**阶段（此时还没有 body 可看门狗可守）；
//	看门狗      覆盖**读响应体**阶段，且不依赖 Transport 的实现——
//	            ctx 只能中断真实的网络读，测试里的假 RoundTripper body 它管不了。
//
// 精度：看门狗按窗口的 1/4 轮询（下限 20ms），触发时刻最多晚于窗口 25%。
type timeoutBody struct {
	rc     io.ReadCloser
	total  time.Duration
	idle   time.Duration
	onDone func() // 可选：随 Close 释放请求级资源（如 ctx 的 cancel）

	mu       sync.Mutex
	lastSeen time.Time
	reason   error // 非 nil 表示看门狗已判定超时，Read 据此改写错误

	done      chan struct{}
	closeOnce sync.Once
}

// newTimeoutBody 按需包装：total 与 idle 都不设时原样返回，不额外起 goroutine。
func newTimeoutBody(rc io.ReadCloser, total, idle time.Duration, onClose func()) io.ReadCloser {
	if rc == nil {
		return rc
	}
	b := &timeoutBody{
		rc:       rc,
		total:    total,
		idle:     idle,
		onDone:   onClose,
		lastSeen: time.Now(),
		done:     make(chan struct{}),
	}
	if total <= 0 && idle <= 0 {
		// 没有任何窗口要守：只保留 onClose 回调，不白起 goroutine。
		return &passThroughBody{timeoutBody: b}
	}
	go b.watch()
	return b
}

func (b *timeoutBody) tick() time.Duration {
	window := b.idle
	if window <= 0 || (b.total > 0 && b.total < window) {
		window = b.total
	}
	t := window / 4
	if t < 20*time.Millisecond {
		t = 20 * time.Millisecond
	}
	return t
}

// watch 只在超时时才关 body；正常读完由调用方 Close 通过 done 让它退出。
func (b *timeoutBody) watch() {
	t := time.NewTicker(b.tick())
	defer t.Stop()

	var totalC <-chan time.Time
	if b.total > 0 {
		tt := time.NewTimer(b.total)
		defer tt.Stop()
		totalC = tt.C
	}

	for {
		select {
		case <-b.done:
			return
		case <-totalC:
			b.expire(ErrStreamTotalTimeout)
			return
		case <-t.C:
			if b.idle <= 0 {
				continue
			}
			b.mu.Lock()
			idleFor := time.Since(b.lastSeen)
			b.mu.Unlock()
			if idleFor >= b.idle {
				b.expire(ErrStreamIdleTimeout)
				return
			}
		}
	}
}

// expire 记下原因并打断阻塞中的 Read。done 与 onClose 仍由调用方 Close 负责，
// 因此这里只关底层 body，不改 Close 的语义。
func (b *timeoutBody) expire(reason error) {
	b.mu.Lock()
	if b.reason == nil {
		b.reason = reason
	}
	b.mu.Unlock()
	_ = b.rc.Close()
}

func (b *timeoutBody) Read(p []byte) (int, error) {
	// 进入时先看是否已经判定超时。这一步不能省：看门狗只关得掉底层 body，**关不掉**
	// 一个还在产出数据的上游——通道里可能仍排着若干帧。超时已经成立，就该停止交付，
	// 否则调用方会在总上限之后继续读到数据，把超时的流当成正常结束。
	if reason := b.tripped(); reason != nil {
		return 0, reason
	}

	n, err := b.rc.Read(p)
	if n > 0 {
		// 有字节就算活跃：空闲窗口从最后一次成功读取重新计时。
		b.mu.Lock()
		b.lastSeen = time.Now()
		b.mu.Unlock()
	}
	// 读到数据与判定超时可能并发发生，这里再确认一次，保证错误一定能浮出水面。
	if reason := b.tripped(); reason != nil {
		return n, reason
	}
	return n, err
}

// tripped 返回看门狗判定出的超时原因（未超时为 nil）。
func (b *timeoutBody) tripped() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reason
}

func (b *timeoutBody) Close() error {
	b.closeOnce.Do(func() { close(b.done) })
	err := b.rc.Close()
	if b.onDone != nil {
		b.onDone()
	}
	return err
}

// passThroughBody 是不需要看门狗时的外壳（total 与 idle 都为 0），
// 只保留 Close 时的 onDone 回调。
type passThroughBody struct{ timeoutBody *timeoutBody }

func (w *passThroughBody) Read(p []byte) (int, error) { return w.timeoutBody.rc.Read(p) }

func (w *passThroughBody) Close() error { return w.timeoutBody.Close() }
