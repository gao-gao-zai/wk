package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// 端到端验证用户可见契约:流式请求在上游卡住时,客户端必须收到
// upstream_timeout 错误帧 + 恰好一个 [DONE],而不是被静默截断。
//
// 这里走真实 TCP(httptest.Server)而不是假 RoundTripper,因为本次改造的时钟与连接
// 关闭行为在真实 net.Conn 上路径不同——假 body 不会经历 transport 层的关闭。
func TestChatStreamIdleTimeoutEmitsUpstreamTimeoutFrameOverRealTCP(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		// 先给一帧正文,再彻底静默(不关连接),模拟上游卡在半途。
		io.WriteString(w, "data: {\"id\":\"r1\",\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"partial\"}}]}\n\n")
		fl.Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	up := upstream.New()
	up.ChatBaseCN = srv.URL
	// 流式:不限总时长,空闲窗口压到 250ms 让测试快速收敛。
	up.Stream = upstream.StreamPolicy{Total: 0, Idle: 250 * time.Millisecond}

	h := newTestHandler(t, Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)

	start := time.Now()
	h.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	body := rec.Body.String()
	// 空闲超时必须及时触发,而不是干等到某个整请求超时。
	if elapsed > 5*time.Second {
		t.Errorf("idle timeout took %v, want ~250ms", elapsed.Round(time.Millisecond))
	}
	// 已有正文必须照常转发给客户端(不能因为超时丢掉已收到的内容)。
	if !strings.Contains(body, "partial") {
		t.Errorf("already-streamed content was dropped: %s", body)
	}
	// sse.go 的既有分类必须仍然生效:错误帧要报 upstream_timeout。
	if !strings.Contains(body, "upstream_timeout") {
		t.Errorf("missing upstream_timeout frame: %s", body)
	}
	// 收尾必须恰好一个 [DONE],客户端才能正常结束读取。
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want 1: %s", n, body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type=%q want text/event-stream", ct)
	}
}

// 空闲超时中断流之后,在途租约必须被归还——否则每个卡住的上游都会永久占用一个
// 并发名额,把号池慢慢耗干(比超时本身更严重)。
func TestChatStreamIdleTimeoutReleasesInFlightLease(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.Stream = upstream.StreamPolicy{Total: 0, Idle: 200 * time.Millisecond}

	p := testPoolWith(&auth.Auth{UID: "u1", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1) // 唯一名额:若泄漏,后面再请求就会 503
	h := newTestHandler(t, Config{Pool: p, Upstream: up})

	call := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}],"stream":true}`))
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		h.ServeHTTP(rec, req)
		return rec
	}

	first := call()
	if !strings.Contains(first.Body.String(), "upstream_timeout") {
		t.Fatalf("first call did not hit the idle timeout: %s", first.Body.String())
	}
	// 名额已归还 → 第二次请求必须仍能拿到账号,而不是 "all accounts unavailable"。
	second := call()
	if second.Code == http.StatusServiceUnavailable {
		t.Fatalf("in-flight lease leaked: second call got %d %s", second.Code, second.Body.String())
	}
}

// 长回答不再被"整请求超时"掐断:即使 up.HTTP.Timeout 远小于流的总时长,
// 只要上游持续产出,流式请求就必须完整跑完。这是本次改造的核心回归点。
func TestChatStreamLongAnswerNotCappedByRequestTimeout(t *testing.T) {
	const frames = 8
	const gap = 60 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < frames; i++ {
			io.WriteString(w, "data: {\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"tok\"}}]}\n\n")
			fl.Flush()
			time.Sleep(gap)
		}
		io.WriteString(w, "data: {\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":8,\"total_tokens\":9}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	up := upstream.New()
	up.ChatBaseCN = srv.URL
	// 旧实现里这个值会把流拦腰砍断;现在它只约束非流式。
	up.HTTP.Timeout = 100 * time.Millisecond
	up.Stream = upstream.StreamPolicy{Total: 0, Idle: 2 * time.Second}

	h := newTestHandler(t, Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)

	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	if strings.Contains(body, "upstream_timeout") {
		t.Fatalf("long stream was killed by the whole-request timeout: %s", body)
	}
	if n := strings.Count(body, "\"content\":\"tok\""); n != frames {
		t.Fatalf("got %d content frames, want %d: %s", n, frames, body)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Fatalf("[DONE] count=%d want 1: %s", n, body)
	}
}
