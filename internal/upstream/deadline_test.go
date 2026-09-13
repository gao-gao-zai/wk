package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// stallReader 先给一段数据，然后永远阻塞，直到被 Close 打断——模拟上游把流挂在
// 半途（既不发数据也不关连接）。这是空闲超时唯一要处理的情形。
type stallReader struct {
	first  bool
	closed chan struct{}
}

func newStallReader() *stallReader {
	return &stallReader{closed: make(chan struct{})}
}

func (s *stallReader) Read(p []byte) (int, error) {
	if !s.first {
		s.first = true
		return copy(p, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"), nil
	}
	// 第二次起永久阻塞：只有看门狗 Close 才能解开。
	<-s.closed
	return 0, errors.New("read tcp: use of closed network connection")
}

func (s *stallReader) Close() error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return nil
}

// 空闲超时必须在 Idle 窗口附近触发，并把错误换成可识别的哨兵；底层 Close 产生的
// "use of closed network connection" 不能泄漏给调用方（sse.go 靠 errors.Is 分类）。
func TestStreamIdleTimeoutTripsAndIsRecognizable(t *testing.T) {
	sr := newStallReader()
	c := testClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       sr,
		}, nil
	})
	rc, status, _, _, err := c.ChatStreamWithPolicy(context.Background(),
		&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{}`),
		StreamPolicy{Idle: 120 * time.Millisecond})
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	defer rc.Close()

	buf := make([]byte, 256)
	if n, rerr := rc.Read(buf); n == 0 || rerr != nil {
		t.Fatalf("first read n=%d err=%v (want the pre-stall frame)", n, rerr)
	}

	// 第二次 Read 会阻塞在 stallReader 上，只有空闲看门狗能解开它。
	start := time.Now()
	_, rerr := rc.Read(buf)
	elapsed := time.Since(start)
	if !errors.Is(rerr, ErrStreamIdleTimeout) {
		t.Fatalf("err=%v want ErrStreamIdleTimeout", rerr)
	}
	// 必须被 sse.go 现有的超时判定认出，否则客户端会收到 upstream_stream_error
	// 而不是上游超时，且 /responses 也不会走 response.failed 分支。
	if !errors.Is(rerr, context.DeadlineExceeded) {
		t.Fatalf("err=%v must wrap context.DeadlineExceeded", rerr)
	}
	if elapsed < 60*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("idle timeout fired after %v, want ~120ms", elapsed)
	}
}

// 上游持续产出时,空闲检查绝不能被触发——否则长回答仍会被误杀,就失去了本次改造的意义。
func TestStreamIdleDoesNotTripWhileDataFlows(t *testing.T) {
	// 每 30ms 一帧,共 6 帧; idle 窗口 150ms > 帧间隔,故不应超时。
	c := testClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       &slowFrameBody{frames: 6, gap: 30 * time.Millisecond},
		}, nil
	})
	rc, status, _, _, err := c.ChatStreamWithPolicy(context.Background(),
		&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{}`),
		StreamPolicy{Idle: 150 * time.Millisecond})
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	defer rc.Close()

	got, rerr := io.ReadAll(rc)
	if rerr != nil {
		t.Fatalf("read all: %v (idle check killed a healthy stream)", rerr)
	}
	if n := strings.Count(string(got), "data:"); n != 6 {
		t.Fatalf("received %d frames, want 6: %q", n, got)
	}
}

type slowFrameBody struct {
	frames int
	gap    time.Duration
	sent   int
}

func (b *slowFrameBody) Read(p []byte) (int, error) {
	if b.sent >= b.frames {
		return 0, io.EOF
	}
	time.Sleep(b.gap)
	b.sent++
	return copy(p, "data: {}\n\n"), nil
}

// Close 故意做成 no-op：模拟"上游仍在供数 / 传输层缓冲区里还排着数据帧"的情形。
// 看门狗关得掉连接，却关不掉已经缓冲好的字节，所以超时必须由 Read 的入口检查
// 强制浮出水面，否则调用方会在总上限之后继续读到数据，把超时的流当成正常结束。
func (b *slowFrameBody) Close() error { return nil }

// 总时长上限是兜底:即使上游一直在发数据,超过 Total 也必须断开。
func TestStreamTotalTimeoutCapsRunawayStream(t *testing.T) {
	c := testClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			// 帧很密,空闲检查不会触发;只有 Total 能拦住它。
			Body: &slowFrameBody{frames: 10000, gap: 10 * time.Millisecond},
		}, nil
	})
	rc, status, _, _, err := c.ChatStreamWithPolicy(context.Background(),
		&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{}`),
		StreamPolicy{Total: 200 * time.Millisecond, Idle: 10 * time.Second})
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	defer rc.Close()

	start := time.Now()
	_, rerr := io.ReadAll(rc)
	elapsed := time.Since(start)
	if rerr == nil {
		t.Fatal("runaway stream was not capped by Total")
	}
	if !errors.Is(rerr, context.DeadlineExceeded) {
		t.Fatalf("err=%v want a context deadline error", rerr)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Total fired after %v, want ~200ms", elapsed)
	}
}

// 零策略 = 不限总时长、不查空闲:数据必须原样通过,且不能施加任何时间限制。
// 注意 body 仍会被包一层——那是为了在 Close 时调用 cancel 释放请求 ctx;
// 若为了"零策略不包装"而省掉它,每次请求的 context 都会泄漏到父 ctx 结束。
func TestZeroPolicyImposesNoTimeLimit(t *testing.T) {
	orig := io.NopCloser(strings.NewReader("data: one\n\ndata: two\n\n"))
	c := testClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: orig}, nil
	})
	rc, _, _, _, err := c.ChatStreamWithPolicy(context.Background(),
		&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{}`), StreamPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	got, rerr := io.ReadAll(rc)
	if rerr != nil {
		t.Fatalf("zero policy must not impose a timeout: %v", rerr)
	}
	if string(got) != "data: one\n\ndata: two\n\n" {
		t.Fatalf("body altered: %q", got)
	}
}

// Close 必须释放请求 ctx(Total 定时器/取消链),否则每次流式请求都会泄漏一个 ctx。
func TestStreamCloseReleasesRequestContext(t *testing.T) {
	var cancelled bool
	var mu sync.Mutex
	c := testClient(func(r *http.Request) (*http.Response, error) {
		// 观察请求 ctx:它在 body Close 后应当被取消。
		go func() {
			<-r.Context().Done()
			mu.Lock()
			cancelled = true
			mu.Unlock()
		}()
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("data: x\n\n")),
		}, nil
	})
	rc, _, _, _, err := c.ChatStreamWithPolicy(context.Background(),
		&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{}`),
		StreamPolicy{Total: time.Hour, Idle: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := cancelled
		mu.Unlock()
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("request context was not cancelled by Close (leaked ctx/timer)")
}

// 非流式必须仍然带着整个请求的上限:若被流式策略覆盖成 0,一次挂死的非流式请求
// 会永久占住账号租约。
func TestRequestPolicyKeepsWholeRequestCapForNonStream(t *testing.T) {
	c := New()
	c.HTTP.Timeout = 120 * time.Second
	c.Stream = StreamPolicy{Total: 0, Idle: 300 * time.Second}

	nonStream := c.RequestPolicy(false)
	if nonStream.Total != 120*time.Second {
		t.Errorf("non-stream Total=%v want 120s", nonStream.Total)
	}
	if nonStream.Idle != 0 {
		t.Errorf("non-stream Idle=%v want 0 (whole-request cap only)", nonStream.Idle)
	}

	stream := c.RequestPolicy(true)
	if stream.Total != 0 || stream.Idle != 300*time.Second {
		t.Errorf("stream policy=%+v want the Stream field", stream)
	}
}

// New().Stream.Idle 必须非零
func TestNewClientHasStreamIdleDefault(t *testing.T) {
	c := New()
	if c.Stream.Idle <= 0 {
		t.Errorf("New().Stream.Idle=%v, want a non-zero idle guard", c.Stream.Idle)
	}
}

// 真实 TCP 上的端到端验证。前面的用例都用假 RoundTripper，而时钟行为、连接关闭
// 与 ctx 取消在真实 net.Conn 上路径不同（见改造前的探测），所以这里补一轮真机验证。
//
// 最核心的一条：一个**持续产出、总时长远超 HTTP.Timeout** 的流式回答必须能完整跑完。
// 这正是改造要解决的问题——旧实现里 HTTP.Timeout=120s 会把长回答拦腰砍断。
func TestRealNetworkLongStreamSurvivesWholeRequestTimeout(t *testing.T) {
	const frames = 9
	const gap = 60 * time.Millisecond
	total := time.Duration(frames) * gap // ≈540ms

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < frames; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"c%d\"}}]}\n\n", i)
			fl.Flush()
			time.Sleep(gap)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	// 故意把整请求超时压到远小于流的总时长：旧语义下这个流必死。
	c.HTTP.Timeout = 100 * time.Millisecond
	// 流式策略：不限总时长，空闲窗口远大于帧间隔。
	c.Stream = StreamPolicy{Total: 0, Idle: 2 * time.Second}

	policy := c.RequestPolicy(true)
	rc, status, _, _, err := c.ChatStreamWithPolicy(context.Background(),
		&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{"model":"m","messages":[]}`), policy)
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	defer rc.Close()

	start := time.Now()
	got, rerr := io.ReadAll(rc)
	elapsed := time.Since(start)
	if rerr != nil {
		t.Fatalf("long stream was killed after %v: %v (HTTP.Timeout=%v must not cap streams)",
			elapsed.Round(time.Millisecond), rerr, c.HTTP.Timeout)
	}
	if elapsed < total {
		t.Fatalf("stream finished in %v, expected ≥%v", elapsed, total)
	}
	if n := strings.Count(string(got), "data: {"); n != frames {
		t.Fatalf("got %d content frames, want %d: %q", n, frames, got)
	}
	if !strings.Contains(string(got), "[DONE]") {
		t.Fatalf("stream did not complete normally: %q", got)
	}
}

// 真实 TCP 上,上游卡住不发数据时必须在空闲窗口附近失败,且错误可被 sse.go 识别。
func TestRealNetworkIdleStallTrips(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		fl.Flush()
		<-release // 之后彻底不发数据(也不关连接),模拟上游卡住
	}))
	defer srv.Close()
	defer close(release)

	c := New()
	c.ChatBaseCN = srv.URL
	c.HTTP.Timeout = 0
	c.Stream = StreamPolicy{Total: 0, Idle: 250 * time.Millisecond}

	rc, status, _, _, err := c.ChatStreamWithPolicy(context.Background(),
		&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{}`), c.RequestPolicy(true))
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	defer rc.Close()

	start := time.Now()
	_, rerr := io.ReadAll(rc)
	elapsed := time.Since(start)
	if !errors.Is(rerr, ErrStreamIdleTimeout) {
		t.Fatalf("err=%v after %v, want ErrStreamIdleTimeout", rerr, elapsed.Round(time.Millisecond))
	}
	if !errors.Is(rerr, context.DeadlineExceeded) {
		t.Fatalf("err=%v must wrap context.DeadlineExceeded so sse.go classifies it as upstream_timeout", rerr)
	}
	if elapsed > 3*time.Second {
		t.Errorf("idle stall detected after %v, want ~250ms", elapsed.Round(time.Millisecond))
	}
}

// 默认 Total=0（不限总时长）时,"等响应头"这一阶段必须仍然有上限:上游接了连接却
// 不回响应头,那个阶段还没有 body 可给看门狗守,否则请求会永久挂住并占着在途租约。
func TestRealNetworkStreamHeaderWaitIsBounded(t *testing.T) {
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 既不回响应头也不关连接。等客户端放弃后立刻返回，否则 httptest.Server.Close()
		// 会一直等这个 handler，让用例白白多跑好几秒。
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}))
	defer srv.Close()
	defer close(done)

	c := New()
	c.ChatBaseCN = srv.URL
	c.HTTP.Timeout = 0 // 流式路径不依赖它
	c.Stream = StreamPolicy{Total: 0, Idle: time.Hour, HeaderWait: 250 * time.Millisecond}

	start := time.Now()
	_, _, _, _, err := c.ChatStreamWithPolicy(context.Background(),
		&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{}`), c.RequestPolicy(true))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("stream with no response headers was not bounded (it would hang forever)")
	}
	if !errors.Is(err, ErrStreamHeaderTimeout) {
		t.Fatalf("err=%v want ErrStreamHeaderTimeout", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("header wait capped after %v, want ~250ms", elapsed.Round(time.Millisecond))
	}
}

// HeaderWait 必须显式配得比 Total 大时也不失控,且一旦响应头到达就不再计时——
// 否则"修好长回答"会被这个新窗口重新破坏。
func TestHeaderWaitDoesNotCutOffAfterHeadersArrive(t *testing.T) {
	const frames = 6
	const gap = 80 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fl.Flush() // 立刻给响应头
		for i := 0; i < frames; i++ {
			fmt.Fprint(w, "data: {}\n\n")
			fl.Flush()
			time.Sleep(gap)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	c.HTTP.Timeout = 0
	// HeaderWait 远小于流的总时长:响应头已到,它就不该再产生任何影响。
	c.Stream = StreamPolicy{Total: 0, Idle: 2 * time.Second, HeaderWait: 100 * time.Millisecond}

	rc, status, _, _, err := c.ChatStreamWithPolicy(context.Background(),
		&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{}`), c.RequestPolicy(true))
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	defer rc.Close()

	got, rerr := io.ReadAll(rc)
	if rerr != nil {
		t.Fatalf("stream was cut off after headers arrived: %v", rerr)
	}
	if !strings.Contains(string(got), "[DONE]") {
		t.Fatalf("stream did not complete: %q", got)
	}
}

// 边界扫描:把响应头到达时刻在 HeaderWait 两侧扫一遍,断言"要么明确 header 超时、
// 要么拿到一个可读的流",不允许出现第三种结果——即拿到了 body 却已被取消
// (下游只会看到语焉不详的 context canceled,且 /responses 会把这种流当成正常开始)。
//
// 说明其能力边界:这个用例是在**断言不变式**,不是在确定性复现那个竞态。真正的竞态窗口
// 只有"响应头到达"与"定时器回调"相隔几微秒的瞬间,靠 sleep 无法稳定命中——实测把实现
// 换成有竞态的版本它依然通过。因此它守的是回归(例如将来有人把 Read 路径改坏),
// 竞态本身靠实现里的 CAS 保证:回调与主流程争抢同一个原子量,只有抢到的一方能 cancel。
func TestHeaderWaitRaceAtBoundaryIsNeverHalfBroken(t *testing.T) {
	const headerWait = 5 * time.Millisecond
	for i := 0; i < 60; i++ {
		// 延迟跨过边界:从"远早于窗口"扫到"明显晚于窗口"。
		delay := time.Duration(i) * headerWait / 20

		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(delay)
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			fmt.Fprint(w, "data: [DONE]\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))

		c := New()
		c.ChatBaseCN = srv.URL
		c.HTTP.Timeout = 0
		c.Stream = StreamPolicy{Total: 0, Idle: time.Hour, HeaderWait: headerWait}

		rc, status, _, _, err := c.ChatStreamWithPolicy(context.Background(),
			&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{}`), c.RequestPolicy(true))
		if err != nil {
			// 明确报 header 超时是可接受的结果之一。
			if !errors.Is(err, ErrStreamHeaderTimeout) {
				close(release)
				srv.Close()
				t.Fatalf("iter %d (delay=%v): unexpected error: %v", i, delay, err)
			}
		} else {
			if status != 200 {
				close(release)
				srv.Close()
				t.Fatalf("iter %d: status=%d", i, status)
			}
			// 拿到了 body 就必须真的可读。这里只读一次而不用 io.ReadAll：handler 在
			// 写完 [DONE] 后仍阻塞着（连接未关），ReadAll 会一直等 EOF 而挂住。
			// 单次 Read 足以区分两种状态——半坏的流在这里立刻返回 context canceled，
			// 健康的流则返回已写入的 [DONE]。
			buf := make([]byte, 128)
			if _, rerr := rc.Read(buf); rerr != nil {
				rc.Close()
				close(release)
				srv.Close()
				t.Fatalf("iter %d (delay=%v): returned a body that cannot be read (%v); "+
					"the header timer cancelled a stream whose headers had arrived", i, delay, rerr)
			}
			rc.Close()
		}
		close(release)
		srv.Close()
	}
}

// 非流式仍必须被整个请求超时兜住:上游一直不返回响应头时,不能永久挂起。
func TestRealNetworkNonStreamStillCapped(t *testing.T) {
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 等客户端放弃(请求 ctx 结束)而不是死睡,否则 httptest.Server.Close() 会
		// 一直等这个 handler,让本用例白白多跑几秒。
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}))
	defer srv.Close()
	defer close(done)

	c := New()
	c.ChatBaseCN = srv.URL
	c.HTTP.Timeout = 250 * time.Millisecond

	policy := c.RequestPolicy(false) // 非流式:沿用整请求上限
	if policy.Total != 250*time.Millisecond {
		t.Fatalf("non-stream policy Total=%v want 250ms", policy.Total)
	}
	start := time.Now()
	_, _, _, _, err := c.ChatStreamWithPolicy(context.Background(),
		&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{}`), policy)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("non-stream request was not capped")
	}
	if elapsed > 3*time.Second {
		t.Errorf("non-stream cap fired after %v, want ~250ms", elapsed.Round(time.Millisecond))
	}
}

// 非流式的**响应体**阶段也必须受上限约束。改造后非流式不再靠 Client.Timeout，
// 而是由 timeoutBody 的 Total 看门狗兜住；若这条路径漏了，上游发了响应头再挂住就会
// 永久占住租约——旧实现里 Client.Timeout 本来能拦住它。
func TestRealNetworkNonStreamBodyPhaseStillCapped(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.(http.Flusher).Flush() // 响应头已到，但正文永远不发
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)

	c := New()
	c.ChatBaseCN = srv.URL
	c.HTTP.Timeout = 300 * time.Millisecond

	rc, status, _, _, err := c.ChatStreamWithPolicy(context.Background(),
		&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{}`), c.RequestPolicy(false))
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v (headers should have arrived)", status, err)
	}
	defer rc.Close()

	start := time.Now()
	_, rerr := io.ReadAll(rc)
	elapsed := time.Since(start)
	if rerr == nil {
		t.Fatal("non-stream body read was not capped; it would hang forever")
	}
	// 这里不锁定具体是哪个哨兵：Total 同时由 ctx 与看门狗表达，两者几乎同时到点，
	// 谁先触发取决于调度，所以拿到的可能是 ErrStreamTotalTimeout，也可能是 ctx 自己
	// 的 "context deadline exceeded"。真正要保证的是两点——有界，且能被 sse.go 的
	// 超时判定认出（它要求 errors.Is(err, context.DeadlineExceeded)）。
	if !errors.Is(rerr, context.DeadlineExceeded) {
		t.Fatalf("err=%v must wrap context.DeadlineExceeded for sse.go classification", rerr)
	}
	if elapsed > 3*time.Second {
		t.Errorf("body-phase cap fired after %v, want ~300ms", elapsed.Round(time.Millisecond))
	}
}
