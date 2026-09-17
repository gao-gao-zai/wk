package main

// 剖面驱动器：在 pprof 端点常驻的前提下对服务打合成负载，供外部抓剖面。
//
// 与 load_test.go 的区别：load_test 结尾有严格的"metrics==attempts"断言，
// SQLite 单连接写 25 万条在这个机器上根本追不平（负载结束后立刻读快照
// 必然是 0）——那是压测要的断言，不是剖面要的。剖面驱动器只关心：
// 一直有真实流量、进程活着、pprof 可达。
//
// 用法：
//
//	$env:WK_PROFILE='1'
//	go test ./cmd/server/ -run TestProxyProfile$ -v -timeout 15m
//
// 另一个终端抓剖面（负载运行中）：
//
//	go tool pprof -seconds 30 http://127.0.0.1:6061/debug/pprof/profile      # CPU
//	go tool pprof -sample_index=alloc_objects http://127.0.0.1:6061/debug/pprof/heap
//	go tool pprof http://127.0.0.1:6061/debug/pprof/mutex
//	go tool pprof http://127.0.0.1:6061/debug/pprof/goroutine?debug=1
//
// 端口固定 6061（避开 6060，万一真实服务也在跑）。
// 压测时长 WK_PROFILE_DURATION（默认 60s）、并发 WK_PROFILE_CONCURRENCY（默认 100）。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	_ "net/http/pprof" // 注册 /debug/pprof/* 到 DefaultServeMux
	"os"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

func TestProxyProfile(t *testing.T) {
	if os.Getenv("WK_PROFILE") != "1" {
		t.Skip("set WK_PROFILE=1 to run profiling driver")
	}

	duration := 60 * time.Second
	if raw := os.Getenv("WK_PROFILE_DURATION"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			t.Fatal("invalid WK_PROFILE_DURATION")
		}
		duration = d
	}
	concurrency := 100
	if raw := os.Getenv("WK_PROFILE_CONCURRENCY"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			t.Fatal("WK_PROFILE_CONCURRENCY must be 1..1000")
		}
		concurrency = n
	}

	addr := "127.0.0.1:6061"
	pprofSrv := &http.Server{Addr: addr, Handler: nil} // nil = DefaultServeMux
	go func() { _ = pprofSrv.ListenAndServe() }()
	defer pprofSrv.Close()
	time.Sleep(100 * time.Millisecond)
	t.Logf("pprof ready on http://%s/debug/pprof/", addr)

	// —— 合成上游：4 帧 SSE + usage，每帧间隔 25ms ——
	delay := 25 * time.Millisecond
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 4; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(delay):
			}
			fmt.Fprint(w, "data: {\"id\":\"profile-id\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: {\"id\":\"profile-id\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4,\"total_tokens\":14}}\n\ndata: [DONE]\n\n")
	}))
	defer mock.Close()

	// —— 被测服务：与 load_test 相同的组装，但 metrics 走内存（不落 SQLite，
	// 25 万条/分钟的 SQLite 单连接写入会把剖面淹没在 db 写里，且这不是本驱动要测的）。
	up := upstream.New()
	up.ChatBaseCN = mock.URL
	defer up.HTTP.CloseIdleConnections()
	p := pool.New("")
	p.SetMaxInFlight(3)
	for i := 0; i < 100; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprint(i), ExpiresAt: time.Now().Add(time.Hour).Unix()})
	}
	cfg := server.Config{Pool: p, Upstream: up, APIKey: "profile-key",
		Session: session.New(session.Config{Available: p.AvailableUIDs}),
		// MetricsStore/RequestLogStore 留 nil：chatStat.done 的持久化分支
		// 对 nil store 有显式跳过，日志照打。
	}
	proxy := httptest.NewServer(server.NewHandler(cfg))
	defer proxy.Close()

	transport := &http.Transport{MaxIdleConns: concurrency, MaxIdleConnsPerHost: concurrency}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second}
	body, _ := json.Marshal(map[string]any{"model": "mock-model", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("x", 1024)}}})

	var ok, fail atomic.Int64
	deadline := time.Now().Add(duration)
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-gate
			for time.Now().Before(deadline) {
				req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions", strings.NewReader(string(body)))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer profile-key")
				resp, err := client.Do(req)
				if err != nil {
					fail.Add(1)
					continue
				}
				data, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode == 200 && strings.Contains(string(data), "[DONE]") {
					ok.Add(1)
				} else {
					fail.Add(1)
				}
			}
		}(i)
	}
	close(gate)
	wg.Wait()

	t.Logf("profile load done: ok=%d fail=%d (%.0f rps)", ok.Load(), fail.Load(), float64(ok.Load())/duration.Seconds())

	// 快照 heap + mutex 到 testdata/pprof/ 供离线分析。
	outDir := filepath.Join("testdata", "pprof")
	if err := os.MkdirAll(outDir, 0o755); err == nil {
		ts := time.Now().Format("20060102-150405")
		if f, err := os.Create(filepath.Join(outDir, fmt.Sprintf("heap-%s.pprof", ts))); err == nil {
			_ = pprof.Lookup("heap").WriteTo(f, 0)
			f.Close()
			t.Logf("heap profile saved to %s", outDir)
		}
		if f, err := os.Create(filepath.Join(outDir, fmt.Sprintf("mutex-%s.pprof", ts))); err == nil {
			_ = pprof.Lookup("mutex").WriteTo(f, 0)
			f.Close()
			t.Logf("mutex profile saved to %s", outDir)
		}
	}
}
