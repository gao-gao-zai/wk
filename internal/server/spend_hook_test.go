package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TestSpendHookFiresOnSuccess chatStat 的影子扣减回调必须只在**成功请求
// 且费用已知**时触发；失败请求（上游没扣费）不能扣。
func TestSpendHookFiresOnSuccess(t *testing.T) {
	// 直接驱动 chatStat：finalize 逻辑是纯函数式的（不依赖 handler 内部）。
	st := newChatStatWithOptions(time.Now(), []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), false, nil, nil, CreditPolicy{InputPer1K: 0.01, OutputPer1K: 0.05}, false, "/v1/chat/completions")
	var spent atomic.Int64
	var spentUID atomic.Pointer[string]
	st.spendHook = func(uid string, amount float64) {
		spentUID.Store(&uid)
		spent.Add(int64(amount * 1000))
	}

	// 失败请求（500）：不触发。
	st.uid = "u1"
	st.status = 500
	st.done()
	if spent.Load() != 0 {
		t.Fatalf("failed request must not spend, got %d", spent.Load())
	}

	// 成功请求（200）且上游费用已知：触发，金额 = 上游 usage.credit。
	st2 := newChatStatWithOptions(time.Now(), []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), false, nil, nil, CreditPolicy{InputPer1K: 0.01, OutputPer1K: 0.05}, false, "/v1/chat/completions")
	st2.spendHook = st.spendHook
	st2.uid = "u2"
	st2.status = 200
	st2.creditsConsumed = 0.37
	st2.creditSource = "upstream"
	st2.done()
	if spent.Load() != 370 {
		t.Fatalf("successful request must spend 0.37, got %.3f", float64(spent.Load())/1000)
	}
	if got := *spentUID.Load(); got != "u2" {
		t.Fatalf("spend uid = %s, want u2", got)
	}
}

// TestSpendHookSkipsUnknownCredits 费用未知（unknown）的成功请求不扣：
// 宁可不均衡也不错扣。
func TestSpendHookSkipsUnknownCredits(t *testing.T) {
	st := newChatStatWithOptions(time.Now(), []byte(`{}`), false, nil, nil, CreditPolicy{}, false, "/v1/chat/completions")
	var fired atomic.Bool
	st.spendHook = func(string, float64) { fired.Store(true) }
	st.uid = "u"
	st.status = 200
	// 没有上游 credit、费率零值 Estimate 返回 !ok → creditSource=unknown。
	st.done()
	if fired.Load() {
		t.Fatal("unknown-credit success must not spend")
	}
}

// TestSpendWiringInChatCompletions handler 层接线：完整请求生命周期中
// spendHook 路径不 panic。上游指向 127.0.0.1 黑洞端口（不触网）→
// 轮换耗尽 → 503。
//
// 注：upstream.New() 默认指向真实上游域名，测试必须覆盖为黑洞地址，
// 否则会打到真网（曾有版本真的收到了 openresty 401）。
func TestSpendWiringInChatCompletions(t *testing.T) {
	up := upstream.New()
	up.ChatBaseCN = "http://127.0.0.1:1"
	up.ChatBaseGlobal = "http://127.0.0.1:1"
	up.BillingBaseCN = "http://127.0.0.1:1"
	up.BillingBaseGlob = "http://127.0.0.1:1"
	h := newTestHandler(t, Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		APIKey:   testAPIKey,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code < 500 {
		t.Fatalf("expected upstream-failure 5xx, got %d: %s", rec.Code, rec.Body.String())
	}
}
