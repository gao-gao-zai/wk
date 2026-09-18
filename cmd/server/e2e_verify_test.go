package main

// 端到端验证：真实 HTTP 客户端 → handler → httptest mock 上游 → SQLite
// 持久化。这是 TestProxyLoad 的微缩版（并发 8、几秒内完成），用来在改动
// 请求解码/持久化路径后快速确认整条链路没断，不必跑完整的 opt-in 负载测试。
// 覆盖点：单次解码共享（#1）、typed SSE 统计（#2）、synchronous=NORMAL
// 下的 RecordCompletion 落库（#3）。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/metricsstore"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

const verifySSE = "data: {\"id\":\"chatcmpl-v\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-v\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4,\"total_tokens\":14,\"credit\":0.01}}\n\n" +
	"data: [DONE]\n\n"

func TestEndToEndChatPersistsMetrics(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "load-id-unchanged")
		fmt.Fprint(w, verifySSE)
	}))
	defer mock.Close()

	up := upstream.New()
	up.ChatBaseCN = mock.URL
	up.ChatBaseGlobal = mock.URL
	defer up.HTTP.CloseIdleConnections()

	p := pool.New("")
	p.SetMaxInFlight(3)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	p.Add(&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	p.Add(&auth.Auth{UID: "u4", AccessToken: "at4", ExpiresAt: time.Now().Add(time.Hour).Unix()})

	db, err := metricsstore.Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	adapter := metricsAdapter{store: db}
	cfg := server.Config{
		Pool: p, Upstream: up, MetricsStore: adapter, RequestLogStore: adapter,
		CompletionStore: adapter,
		Session:      session.New(session.Config{Available: p.AvailableUIDs}),
		APIKey:        "k-verify",
		CreditPolicy: server.CreditPolicy{InputPer1K: 1, OutputPer1K: 1},
	}
	proxy := httptest.NewServer(server.NewHandler(cfg))
	defer proxy.Close()

	const concurrency = 8
	const rounds = 5
	transport := &http.Transport{MaxIdleConns: concurrency, MaxIdleConnsPerHost: concurrency}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second}

	body, _ := json.Marshal(map[string]any{
		"model":         "verify-model",
		"stream":        true,
		"metadata":      map[string]any{"conversation_id": "verify-conv"},
		"max_tokens":    128,
		"messages":      []any{map[string]any{"role": "user", "content": "hi"}},
	})

	var successes atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions", strings.NewReader(string(body)))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer k-verify")
				resp, err := client.Do(req)
				if err != nil {
					t.Errorf("request failed: %v", err)
					return
				}
				data, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK && strings.Contains(string(data), "[DONE]") {
					successes.Add(1)
				} else {
					t.Errorf("status=%d body=%s", resp.StatusCode, truncateFor(t, string(data)))
				}
			}
		}()
	}
	wg.Wait()

	want := int64(concurrency * rounds)
	if got := successes.Load(); got != want {
		t.Fatalf("successes=%d want=%d", got, want)
	}
	// 每个 200 请求必须已在 SQLite 里累计（含 token 与 credit 字段）。
	snap, err := db.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Requests != want || snap.Successes != want {
		t.Fatalf("persisted metrics=%+v want requests/successes=%d", snap, want)
	}
	if snap.InputTokens != want*10 || snap.OutputTokens != want*4 {
		t.Fatalf("token accounting broken: input=%d output=%d", snap.InputTokens, snap.OutputTokens)
	}
	// usage.credit = 0.01/请求：typed struct 的 *float64 路径必须照常采信。
	if snap.CreditsConsumed < float64(want)*0.01-1e-9 {
		t.Fatalf("credits=%v want >= %v", snap.CreditsConsumed, float64(want)*0.01)
	}
}

func truncateFor(t *testing.T, s string) string {
	t.Helper()
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
