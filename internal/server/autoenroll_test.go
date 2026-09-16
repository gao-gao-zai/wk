package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/haozhuma"
	"workbuddy2api/internal/smslogin"
)

// fakeHZM 假豪猪服务器：可编程的响应序列，用于测试终止逻辑。
type fakeHZM struct {
	t         *testing.T
	srv       *httptest.Server
	getPhone  atomic.Int32 // getPhone 调用次数
	getMsg    atomic.Int32 // getMessage 调用次数（验证轮询次数用）
	blacklist atomic.Int32 // addBlacklist 成功次数
	releaseOK atomic.Int32 // cancelRecv 成功次数
	summary   atomic.Value // string: 余额响应
	phoneResp atomic.Value // string: getPhone 响应
	tokenResp atomic.Value // string: login 响应
	msgResp   atomic.Value // string: getMessage 响应（默认"等待短信"）
	// releaseFail 剩余多少次 cancelRecv 要失败（模拟释放请求失败）。
	releaseFail atomic.Int32
	// releaseGone 为 true 时豪猪对释放返回"手机号不存在"（号已不在它手里）。
	releaseGone atomic.Bool
	// seq 全局事件序号，配合 firstPhoneSeq / firstReleaseSeq 断言**顺序**：
	// "补释放必须发生在第一次取号之前"是额度能否恢复的关键，只断言"最终释放了"
	// 无法区分顺序（两条路径都会释放）。
	seq             atomic.Int64
	firstPhoneSeq   atomic.Int64
	firstReleaseSeq atomic.Int64
}

// nextSeq 取一个递增序号，用于记录"某类事件第一次发生的时刻"。
func (f *fakeHZM) nextSeq() int64 { return f.seq.Add(1) }

func newFakeHZM(t *testing.T) *fakeHZM {
	f := &fakeHZM{t: t}
	f.summary.Store(`{"code":"0","msg":"ok","money":"10.00"}`)
	f.phoneResp.Store(`{"code":"0","msg":"成功","phone":"17000000001"}`)
	f.tokenResp.Store(`{"code":"0","msg":"ok","token":"tok-1"}`)
	f.msgResp.Store(`{"code":"-1","msg":"等待短信"}`)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api := r.URL.Query().Get("api")
		switch api {
		case "login":
			_, _ = w.Write([]byte(f.tokenResp.Load().(string)))
		case "getSummary":
			_, _ = w.Write([]byte(f.summary.Load().(string)))
		case "getPhone":
			// 记下第一次取号的时刻（compare-and-swap：只记首次）。
			f.firstPhoneSeq.CompareAndSwap(0, f.nextSeq())
			f.getPhone.Add(1)
			_, _ = w.Write([]byte(f.phoneResp.Load().(string)))
		case "getMessage":
			f.getMsg.Add(1)
			// 默认永远等待（模拟收不到码）。
			_, _ = w.Write([]byte(f.msgResp.Load().(string)))
		case "addBlacklist":
			f.blacklist.Add(1)
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok"}`))
		case "cancelRecv":
			// 记下第一次释放的时刻，用于断言"补释放早于取号"。
			f.firstReleaseSeq.CompareAndSwap(0, f.nextSeq())
			// 号已不在豪猪手里（已释放/过期）：实测文案"很抱歉,手机号不存在"。
			if f.releaseGone.Load() {
				_, _ = w.Write([]byte(`{"code":"-1","msg":"很抱歉,手机号不存在"}`))
				return
			}
			// 按注入的次数失败，用来验证收尾兜底会重试。
			// 注意文案：不能用"释放失败"——那已被 goneUpstream 认作
			// "号已不在豪猪"（一键释放/过期后的实测文案），会被算归还
			// 而不是留在账本。这里要模拟的是平台持续故障（重试有意义）。
			if f.releaseFail.Load() > 0 {
				f.releaseFail.Add(-1)
				_, _ = w.Write([]byte(`{"code":"-1","msg":"系统繁忙，请稍后再试（模拟）"}`))
				return
			}
			f.releaseOK.Add(1)
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok"}`))
		default:
			_, _ = w.Write([]byte(`{"code":"404","msg":"unknown"}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHZM) client() *haozhuma.Client {
	u, _ := url.Parse(f.srv.URL)
	c := haozhuma.New("tok-1")
	c.Base = u.String() + "/"
	return c
}

// successSMSManager 一个能走通完整短信直登链的 Manager（复用 sms_fake_test 的假上游）。
func successSMSManager(t *testing.T) *smslogin.Manager {
	t.Helper()
	up := newFakeSMSUpstream(t)
	t.Cleanup(up.Close)
	return smslogin.NewManager(up.Endpoints(), time.Minute)
}

// noSMS 一个永远发不出码的 SMSLogin 管理器（发码直接失败）。
func noSMSManager() *smslogin.Manager {
	// 用一个不存在的端点：Send 会因网络错误失败，但很快（连 localhost）。
	m := smslogin.NewManager(smslogin.Endpoints{
		Console: "http://127.0.0.1:1",
		CLI:     "http://127.0.0.1:1",
		OneID:   "http://127.0.0.1:1",
		Realm:   "copilot",
	}, time.Minute)
	return m
}

// TestAutoEnrollStopsOnFatalError 余额不足/无号可取等致命错误必须立即终止，
// 不能继续烧钱。
func TestAutoEnrollStopsOnFatalError(t *testing.T) {
	f := newFakeHZM(t)
	// 取号返回余额不足。
	f.phoneResp.Store(`{"code":"201","msg":"余额不足，请充值"}`)
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)

	if err := en.AutoRun(5, 1); err != nil {
		t.Fatal(err)
	}
	waitDone(t, en)
	st := en.Status()
	if st.Running {
		t.Fatal("task should be done")
	}
	if !strings.Contains(st.StopReason, "余额") {
		t.Fatalf("stop_reason=%q want 余额不足", st.StopReason)
	}
	// 致命错误只调一次 getPhone 就停。
	if n := f.getPhone.Load(); n > 2 {
		t.Fatalf("getPhone called %d times after fatal error, want <=2", n)
	}
}

// TestAutoEnrollStopsOnLowBalance 余额不足以支付一次成功取码时不该启动
// （开跑前先查余额，直接报错而不是跑起来才发现取不到号）。
func TestAutoEnrollStopsOnLowBalance(t *testing.T) {
	f := newFakeHZM(t)
	// 余额明显低于 minBalance 默认值（2.2，对应 52283 单价）。
	f.summary.Store(`{"code":"0","msg":"ok","money":"1.20"}`)
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.retryDelay = time.Millisecond

	err := en.AutoRun(3, 1)
	if err == nil {
		waitDone(t, en)
		t.Fatalf("low balance should refuse to start, status=%+v", en.Status())
	}
	if !strings.Contains(err.Error(), "余额不足") {
		t.Fatalf("err=%v want 余额不足", err)
	}
	// 拒绝启动时不该取号。
	if n := f.getPhone.Load(); n != 0 {
		t.Fatalf("getPhone called %d times, want 0", n)
	}
	// 也不该进入 running 状态。
	if en.Status().Running {
		t.Fatal("must not be marked running after refusing to start")
	}
}

// TestAutoEnrollCircuitBreaker 连续失败达到阈值必须熔断。
func TestAutoEnrollCircuitBreaker(t *testing.T) {
	f := newFakeHZM(t)
	// 取号正常，但 SMSLogin 用坏端点：每个号发码都失败 -> 连续失败。
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.retryDelay = time.Millisecond // 测试里别真等 5s

	if err := en.AutoRun(50, 1); err != nil {
		t.Fatal(err)
	}
	waitDone(t, en)
	st := en.Status()
	if !strings.Contains(st.StopReason, "连续") && !strings.Contains(st.StopReason, "熔断") {
		t.Fatalf("stop_reason=%q want 熔断", st.StopReason)
	}
	// 熔断阈值 consecutiveFails=8，远小于 50*3 的上限。
	if st.Attempts > consecutiveFails+2 {
		t.Fatalf("attempts=%d exceeds circuit breaker expectation", st.Attempts)
	}
	// 熔断必须真的省下取号：不该跑满 150 次。
	if n := f.getPhone.Load(); int(n) > consecutiveFails+2 {
		t.Fatalf("getPhone called %d times, circuit breaker did not stop it", n)
	}
}

// TestAutoEnrollReloginOnTokenInvalid token 失效时自动重登并继续。
func TestAutoEnrollReloginOnTokenInvalid(t *testing.T) {
	var phase atomic.Int32 // 0=token失效, 1=已重登
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("api") {
		case "login":
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","token":"tok-2"}`))
		case "getSummary":
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","money":"10.00"}`))
		case "getPhone":
			if phase.Load() == 0 {
				// 第一次：token 失效 -> 触发 Relogin。
				_, _ = w.Write([]byte(`{"code":"101","msg":"token失效请重新登录"}`))
				phase.Store(1)
				return
			}
			// 重登后：余额不足（致命错误，快速结束测试）。
			_, _ = w.Write([]byte(`{"code":"201","msg":"余额不足"}`))
		default:
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok"}`))
		}
	}))
	defer srv.Close()

	// 客户端必须记住账密，Relogin 才有凭据可用。
	u, _ := url.Parse(srv.URL)
	c := haozhuma.NewWithCredentials("tok-1", "user", "pass")
	c.Base = u.String() + "/"

	en := NewAutoEnroller(noSMSManager(), c, "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.retryDelay = time.Millisecond

	if err := en.AutoRun(2, 1); err != nil {
		t.Fatal(err)
	}
	waitDone(t, en)
	st := en.Status()
	// 重登后应继续到第二个致命错误（余额不足），而不是停在 token 失效。
	if !strings.Contains(st.StopReason, "余额") {
		t.Fatalf("stop_reason=%q want relogin then 余额不足", st.StopReason)
	}
	if c.Token != "tok-2" {
		t.Fatalf("token=%q want tok-2 (relogin)", c.Token)
	}
}

// TestReloginWithoutCredentials token 直建（无账密）时 Relogin 必须报错而不是静默失败。
func TestReloginWithoutCredentials(t *testing.T) {
	c := haozhuma.New("tok-only")
	if err := c.Relogin(); err == nil {
		t.Fatal("Relogin without credentials must fail")
	}
}

// TestAutoEnrollFatalErrorClassification 直接验证 APIError.Fatal 分类。
func TestAutoEnrollFatalErrorClassification(t *testing.T) {
	fatal := []string{
		`{"code":"201","msg":"余额不足，请先充值"}`,
		`{"code":"999","msg":"当前无号可取"}`,
		`{"code":"999","msg":"项目已被禁用"}`,
		// 实测量到的真实措辞：错 sid 返回这个。
		`{"code":"-1","msg":"没有找到项目ID"}`,
		`{"code":"999","msg":"号码库存不足"}`,
	}
	for _, raw := range fatal {
		// 用假服务器构造 APIError。
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(raw))
		}))
		u, _ := url.Parse(srv.URL)
		c := haozhuma.New("t")
		c.Base = u.String() + "/"
		_, err := c.GetPhone(context.Background(), "1")
		var ae *haozhuma.APIError
		if !errors.As(err, &ae) || !ae.Fatal() {
			t.Errorf("raw=%s should be fatal, got err=%v", raw, err)
		}
		srv.Close()
	}
	// 成功响应不是 fatal。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":"0","msg":"ok","phone":"1"}`)
	}))
	u, _ := url.Parse(srv.URL)
	c := haozhuma.New("t")
	c.Base = u.String() + "/"
	if _, err := c.GetPhone(context.Background(), "1"); err != nil {
		var ae *haozhuma.APIError
		if errors.As(err, &ae) && ae.Fatal() {
			t.Error("success response must not be fatal")
		}
	}
	srv.Close()
}

// TestTokenInvalidOnHTTP403 实测 token 失效时豪猪回 HTTP 403 且 body 非 JSON，
// 必须被识别成 token 失效（触发重登）而不是普通的解析错误。
func TestTokenInvalidOnHTTP403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "<html>403 Forbidden</html>")
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := haozhuma.New("bad-token")
	c.Base = u.String() + "/"

	_, err := c.GetPhone(context.Background(), "52283")
	var ae *haozhuma.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("HTTP 403 must yield APIError, got %v", err)
	}
	if !ae.TokenInvalid() {
		t.Fatalf("HTTP 403 must be TokenInvalid, got code=%s msg=%s", ae.Code, ae.Msg)
	}
	if ae.Fatal() {
		t.Error("token invalid should not be classified fatal (relogin can fix it)")
	}
}

// TestBadSIDIsFatal 错项目 ID 实测返回 "没有找到项目ID"，必须终止任务。
func TestBadSIDIsFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":"-1","data":null,"msg":"没有找到项目ID"}`)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := haozhuma.New("t")
	c.Base = u.String() + "/"

	_, err := c.GetPhone(context.Background(), "999999")
	var ae *haozhuma.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("bad sid must yield APIError, got %v", err)
	}
	if !ae.Fatal() {
		t.Fatalf("bad sid must be fatal, got code=%s msg=%s", ae.Code, ae.Msg)
	}
	if ae.TokenInvalid() {
		t.Error("bad sid must not be classified as token invalid (would retry forever via relogin)")
	}
}

// TestBalanceParsing 余额解析（真实响应 money 是字符串）。
func TestBalanceParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"money":"21.000","num":"300"}`)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := haozhuma.New("t")
	c.Base = u.String() + "/"

	bal, err := c.Balance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if bal != 21.0 {
		t.Fatalf("balance=%v want 21.0", bal)
	}
}

// TestAutoEnrollConcurrentRespectsTarget 并发跑时必须精确停在目标成功数，
// 不能超发（超发会多消耗号）。
func TestAutoEnrollConcurrentRespectsTarget(t *testing.T) {
	f := newFakeHZM(t)
	var seq atomic.Int64
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("api") {
		case "login":
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","token":"tok-1"}`))
		case "getSummary":
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","money":"100.00"}`))
		case "getPhone":
			n := seq.Add(1)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"code":"0","msg":"成功","phone":"1700000%04d"}`, n)))
		case "getMessage":
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","sms":"验证码123456"}`))
		default:
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok"}`))
		}
	})
	en := NewAutoEnroller(successSMSManager(t), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		func(string) (string, bool) { return "", false },
	)
	en.retryDelay = time.Millisecond
	en.pollInterval = time.Millisecond

	const want = 6
	if err := en.AutoRun(want, 3); err != nil {
		t.Fatal(err)
	}
	waitDone(t, en)
	st := en.Status()
	if st.OK != want {
		t.Fatalf("ok=%d want exactly %d (overshoot wastes numbers)", st.OK, want)
	}
	if st.Workers != 3 {
		t.Fatalf("workers=%d want 3", st.Workers)
	}
}

// TestAutoEnrollBlacklistPolicy 拉黑策略（2026-09 与豪猪官方 SDK next_code
// 语义对齐后）：**失败号**（收不到码/验码失败）必须拉黑+释放；
// **成功号**（接码成功、账号落盘）只释放不拉黑——拉黑已成功的号会让
// 号主以后在本项目收不到码。
func TestAutoEnrollBlacklistPolicy(t *testing.T) {
	f := newFakeHZM(t)
	var mu sync.Mutex
	blacklistCalls := map[string]bool{}
	releaseCalls := map[string]bool{}
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch q.Get("api") {
		case "login":
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","token":"tok-1"}`))
		case "getSummary":
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","money":"100.00"}`))
		case "getPhone":
			n := f.getPhone.Add(1)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"code":"0","msg":"成功","phone":"1700000%04d"}`, n)))
		case "getMessage":
			// 尾号偶数收不到码（失败），奇数给码（成功）。
			p := q.Get("phone")
			if (p[len(p)-1]-'0')%2 == 0 {
				_, _ = w.Write([]byte(`{"code":"-1","msg":"等待短信"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","sms":"验证码123456"}`))
		case "addBlacklist":
			mu.Lock()
			blacklistCalls[q.Get("phone")] = true
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok"}`))
		case "cancelRecv":
			mu.Lock()
			releaseCalls[q.Get("phone")] = true
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok"}`))
		default:
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok"}`))
		}
	})
	en := NewAutoEnroller(successSMSManager(t), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		func(string) (string, bool) { return "", false },
	)
	en.retryDelay = time.Millisecond
	en.pollInterval = time.Millisecond
	en.pollCount = 1 // 失败号只轮一次就超时，测试快速推进

	if err := en.AutoRun(3, 1); err != nil {
		t.Fatal(err)
	}
	waitDone(t, en)

	mu.Lock()
	defer mu.Unlock()
	// 每个取到过的号都要被释放（成功失败都一样：额度必须归还）。
	if n := int(f.getPhone.Load()); len(releaseCalls) != n {
		t.Fatalf("released=%d but took %d numbers, every taken number must be released", len(releaseCalls), n)
	}
	for phone := range releaseCalls {
		last := phone[len(phone)-1]
		if last == '0' || last == '2' || last == '4' || last == '6' || last == '8' {
			// 偶数 = 失败号：必须拉黑。
			if !blacklistCalls[phone] {
				t.Errorf("failed number %s was not blacklisted", phone)
			}
		}
	}
	// 成功号（奇数）不允许出现在黑名单里。
	for phone := range blacklistCalls {
		last := phone[len(phone)-1]
		if last == '1' || last == '3' || last == '5' || last == '7' || last == '9' {
			t.Errorf("successful number %s must NOT be blacklisted (release only)", phone)
		}
	}
}

// TestAutoEnrollConcurrentCircuitBreaker 并发下熔断阈值仍然生效。
func TestAutoEnrollConcurrentCircuitBreaker(t *testing.T) {
	f := newFakeHZM(t)
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.retryDelay = time.Millisecond

	if err := en.AutoRun(50, 4); err != nil {
		t.Fatal(err)
	}
	waitDone(t, en)
	st := en.Status()
	if st.StopReason == "" {
		t.Fatalf("concurrent run should stop with a reason, status=%+v", st)
	}
	// 并发不会让熔断失效：尝试次数被限制住，不会跑满 50*12。
	if st.Attempts > consecutiveFails+4*2 {
		t.Fatalf("attempts=%d too high for circuit breaker", st.Attempts)
	}
}

// TestQuotaExhaustedIsNotFatal 豪猪的「您的余额不足,请释放拉黑后再取号」
// 是"占用号数到上限"的意思，不是账户没钱——绝不能终止整个任务。
func TestQuotaExhaustedIsNotFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":"-1","msg":"您的余额不足,请释放拉黑后再取号"}`)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := haozhuma.New("t")
	c.Base = u.String() + "/"

	_, err := c.GetPhone(context.Background(), "52283")
	var ae *haozhuma.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("want APIError, got %v", err)
	}
	if !ae.QuotaExhausted() {
		t.Fatalf("should be QuotaExhausted, msg=%q", ae.Msg)
	}
	if ae.Fatal() {
		t.Fatalf("quota exhaustion must NOT be fatal (task should wait and retry), msg=%q", ae.Msg)
	}
}

// TestRealBalanceInsufficientIsFatal 账户真没钱时必须终止。
func TestRealBalanceInsufficientIsFatal(t *testing.T) {
	for _, msg := range []string{"余额不足，请充值", "您的余额不足，请充值后使用"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"code":"201","msg":%q}`, msg)
		}))
		u, _ := url.Parse(srv.URL)
		c := haozhuma.New("t")
		c.Base = u.String() + "/"
		_, err := c.GetPhone(context.Background(), "52283")
		var ae *haozhuma.APIError
		if !errors.As(err, &ae) || !ae.Fatal() {
			t.Errorf("msg=%q should be fatal, got %v", msg, err)
		}
		if ae != nil && ae.QuotaExhausted() {
			t.Errorf("msg=%q must not be QuotaExhausted", msg)
		}
		srv.Close()
	}
}

// TestAutoEnrollStop 运行中必须能被停掉（否则只能重启容器）。
// 刻意用"取号成功但永远收不到码"：任务会一直轮询，只有 Stop 能结束它。
func TestAutoEnrollStop(t *testing.T) {
	f := newFakeHZM(t)
	// getMessage 永远返回"等待"，轮询次数设得很大，任务因此一直挂着。
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.retryDelay = 10 * time.Millisecond
	en.pollInterval = 20 * time.Millisecond
	en.pollCount = maxPollCount // 故意很大：证明是 Stop 生效而不是等超时

	// noSMSManager 发码会立刻失败，任务瞬间跑完；这里换成一个"发码成功但
	// 收不到码"的假上游，让任务停在收码轮询上。
	en.sms = successSMSManager(t)

	if err := en.AutoRun(10, 2); err != nil {
		t.Fatal(err)
	}
	// 等它真的进入收码轮询。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !en.Status().Running {
		time.Sleep(10 * time.Millisecond)
	}
	if !en.Status().Running {
		t.Fatal("task should be running")
	}

	if !en.Stop("测试停止") {
		t.Fatal("Stop should report true while running")
	}
	// 必须在很短时间内结束（远小于 36 次 × 20ms 的轮询总量，证明是取消起作用）。
	done := make(chan struct{})
	go func() { waitDone(t, en); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not finish the task in time")
	}
	if en.Status().Running {
		t.Fatal("should not be running after Stop")
	}
	// 没有正在跑的任务时 Stop 返回 false，而不是报错。
	if en.Stop("") {
		t.Fatal("Stop on idle task should report false")
	}
}

// TestAutoEnrollStopKeepsReason Stop 之后轮询仍能读到终止原因。
func TestAutoEnrollStopKeepsReason(t *testing.T) {
	f := newFakeHZM(t)
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.retryDelay = 10 * time.Millisecond
	en.pollInterval = 10 * time.Millisecond
	en.pollCount = maxPollCount // 轮询够久，Stop 一定发生在任务中途

	if err := en.AutoRun(10, 1); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !en.Status().Running {
		time.Sleep(10 * time.Millisecond)
	}
	en.Stop("用户手动停止")
	waitDone(t, en)
	if got := en.Status().StopReason; !strings.Contains(got, "停止") {
		t.Fatalf("stop_reason=%q want 手动停止", got)
	}
}

// TestAutoEnrollStopCountsConsumedNumbers 中途停止时，已经取走的号必须
// 计入消耗——否则"取了 3 个号后停止"会显示成 0，看不出号码去了哪。
func TestAutoEnrollStopCountsConsumedNumbers(t *testing.T) {
	f := newFakeHZM(t)
	en := NewAutoEnroller(successSMSManager(t), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.retryDelay = 10 * time.Millisecond
	en.pollInterval = 20 * time.Millisecond
	en.pollCount = maxPollCount // 卡在收码轮询，等 Stop

	if err := en.AutoRun(10, 2); err != nil {
		t.Fatal(err)
	}
	// 等两个 worker 都取到号（getPhone 被调用 >= 2 次）。
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if f.getPhone.Load() >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if f.getPhone.Load() < 2 {
		t.Fatalf("workers should have taken numbers, getPhone=%d", f.getPhone.Load())
	}

	en.Stop("测试停止")
	waitDone(t, en)

	st := en.Status()
	taken := int(f.getPhone.Load())
	if st.Consumed == 0 {
		t.Fatalf("consumed=0 but %d numbers were taken — consumption must be reported", taken)
	}
	// 每个取到的号都应被计入（允许任务在 Stop 前多取，所以用 >=）。
	if st.Consumed < 2 {
		t.Fatalf("consumed=%d want >=2 (getPhone called %d times)", st.Consumed, taken)
	}
	// 被取消的尝试不该被算成失败。
	if st.Fail != 0 {
		t.Fatalf("fail=%d, cancelled attempts must not count as failures", st.Fail)
	}
}

// TestPollCountControlsPollingRounds 轮询次数必须精确控制实际查询次数。
// 这是用户能调的核心参数：次数少了收不到晚到的码，次数多了白等。
func TestPollCountControlsPollingRounds(t *testing.T) {
	for _, want := range []int{1, 3, 7} {
		f := newFakeHZM(t)
		en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
			func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
			nil)
		en.pollInterval = time.Millisecond
		en.pollCount = want

		code, err := en.pollCode(context.Background(), "17000000001", want)
		if code != "" {
			t.Fatalf("count=%d: unexpected code %q", want, code)
		}
		if err == nil {
			t.Fatalf("count=%d: want timeout error", want)
		}
		// 精确等于 want：多一次是白等，少一次是漏查。
		if got := int(f.getMsg.Load()); got != want {
			t.Fatalf("count=%d: getMessage called %d times, want exactly %d", want, got, want)
		}
	}
}

// TestPollCountClampedToMax 超过上限的次数必须被夹住，否则一个荒唐的值
// （比如 100000）会让任务挂着不动，而号码一直被豪猪占着。
func TestPollCountClampedToMax(t *testing.T) {
	f := newFakeHZM(t)
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.pollInterval = time.Millisecond

	if err := en.AutoRunWith(AutoRunOptions{Want: 1, Workers: 1, PollCount: 100000}); err != nil {
		t.Fatal(err)
	}
	// 生效值必须是夹住后的上限，而不是用户填的值。
	if got := en.Status().PollCount; got != maxPollCount {
		t.Fatalf("poll_count=%d, want clamped to %d", got, maxPollCount)
	}
	en.Stop("测试结束")
	waitDone(t, en)
}

// TestPollCountZeroUsesTimeoutDerivedDefault 不指定次数时回到"时长 ÷ 间隔"，
// 保证老调用点（只设 pollTimeout）行为不变。
func TestPollCountZeroUsesTimeoutDerivedDefault(t *testing.T) {
	f := newFakeHZM(t)
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.pollInterval = time.Millisecond
	en.pollTimeout = 5 * time.Millisecond // 5ms / 1ms = 5 次

	_, _ = en.pollCode(context.Background(), "17000000001", 0)
	if got := int(f.getMsg.Load()); got != 5 {
		t.Fatalf("getMessage called %d times, want 5 (derived from pollTimeout/pollInterval)", got)
	}
}

// TestPollCodeReturnsEarlyOnCode 拿到码就立刻返回，不用把次数轮完。
func TestPollCodeReturnsEarlyOnCode(t *testing.T) {
	f := newFakeHZM(t)
	// 第 2 次查询才给码：前一次"等待"，之后正常返回。
	f.msgResp.Store(`{"code":"0","msg":"ok","sms":"您的验证码是 654321"}`)
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.pollInterval = time.Millisecond

	code, err := en.pollCode(context.Background(), "17000000001", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != "654321" {
		t.Fatalf("code=%q want 654321", code)
	}
	if got := int(f.getMsg.Load()); got != 1 {
		t.Fatalf("getMessage called %d times, want 1 (must stop as soon as the code arrives)", got)
	}
}

// TestMaxAttemptsOverride 用户可以把总尝试次数放宽——对接商质量差时
// 需要试很多次才成一个，写死 want*12 会提前放弃。
//
// 注意要同时放宽熔断阈值：全部尝试都失败时，连续失败熔断（默认 15）会先于
// 尝试上限生效。这是有意的（通道真坏了就别再抽号），所以本测试显式抬高它
// 来单独验证 max_attempts。
func TestMaxAttemptsOverride(t *testing.T) {
	oldFails := consecutiveFails
	consecutiveFails = 1000
	t.Cleanup(func() { consecutiveFails = oldFails })

	f := newFakeHZM(t)
	// 取号一直失败（可重试，非致命），任务会一直重试到上限。
	f.phoneResp.Store(`{"code":"-1","msg":"没有取到号码，请重新尝试"}`)
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.retryDelay = time.Millisecond
	// 目标 1 个，默认上限是 max(20, 1*12) = 20；放宽到 35 应该真的跑 35 次。
	if err := en.AutoRunWith(AutoRunOptions{Want: 1, Workers: 1, MaxAttempts: 35}); err != nil {
		t.Fatal(err)
	}
	if got := en.Status().MaxAttempts; got != 35 {
		t.Fatalf("max_attempts=%d want 35", got)
	}
	waitDone(t, en)
	if got := en.Status().Attempts; got != 35 {
		t.Fatalf("attempts=%d want 35 (the raised limit must actually be used)", got)
	}
}

// TestCircuitBreakerStillCapsRaisedAttempts 抬高上限不能绕过熔断：
// 对接商整条通道坏掉时（连续失败），即使把尝试上限调到很大也必须停下，
// 否则会无意义地把号池抽干。
func TestCircuitBreakerStillCapsRaisedAttempts(t *testing.T) {
	f := newFakeHZM(t)
	f.phoneResp.Store(`{"code":"-1","msg":"没有取到号码，请重新尝试"}`)
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.retryDelay = time.Millisecond
	if err := en.AutoRunWith(AutoRunOptions{Want: 1, Workers: 1, MaxAttempts: 500}); err != nil {
		t.Fatal(err)
	}
	waitDone(t, en)
	st := en.Status()
	if st.Attempts > consecutiveFails+2 {
		t.Fatalf("attempts=%d — circuit breaker must cap a raised max_attempts (%d)",
			st.Attempts, consecutiveFails)
	}
	if st.StopReason == "" {
		t.Fatal("circuit breaker should record a stop reason")
	}
}

// TestCountersResetBetweenRuns 每次启动都必须清零计数，尤其是 consumed。
// 漏掉 consumed 会残留上次任务的号码数——界面显示 1 而实际已取走 5 个号，
// 而号码是花钱的资产，这个数错了会让人以为没消耗。
func TestCountersResetBetweenRuns(t *testing.T) {
	f := newFakeHZM(t)
	en := NewAutoEnroller(successSMSManager(t), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.retryDelay = time.Millisecond
	en.pollInterval = time.Millisecond
	en.pollCount = 1 // 收不到码，快速失败

	// 第一轮：取号必然发生，consumed 应该 > 0。
	if err := en.AutoRun(1, 1); err != nil {
		t.Fatal(err)
	}
	waitDone(t, en)
	first := en.Status()
	if first.Consumed == 0 {
		t.Fatal("first run should have consumed at least one number")
	}
	if first.Attempts == 0 {
		t.Fatal("first run should have at least one attempt")
	}

	// 第二轮：所有计数必须从 0 重新开始，而不是叠加或残留。
	if err := en.AutoRun(1, 1); err != nil {
		t.Fatal(err)
	}
	// 立刻读：此时第二轮刚起步，consumed 还不该是上一轮的值。
	mid := en.Status()
	if mid.Consumed == first.Consumed && mid.Attempts == first.Attempts {
		t.Fatalf("counters look stale at second start: consumed=%d attempts=%d (same as first run)",
			mid.Consumed, mid.Attempts)
	}
	waitDone(t, en)
	second := en.Status()
	if second.Consumed > first.Consumed+2 {
		t.Fatalf("consumed=%d after second run — counters must reset, not accumulate (first=%d)",
			second.Consumed, first.Consumed)
	}
}

// TestTaskEndReleasesAllNumbers 任务结束时必须把所有号码还给豪猪。
//
// 号没还回去会占住豪猪的并发额度，额度满了后续取号一律返回
// 「您的余额不足,请释放拉黑后再取号」——看起来像没钱，实际是号没还。
func TestTaskEndReleasesAllNumbers(t *testing.T) {
	f := newFakeHZM(t)
	en := NewAutoEnroller(successSMSManager(t), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.retryDelay = time.Millisecond
	en.pollInterval = time.Millisecond
	en.pollCount = 1

	if err := en.AutoRun(1, 1); err != nil {
		t.Fatal(err)
	}
	waitDone(t, en)

	st := en.Status()
	if st.Held != 0 {
		t.Fatalf("held=%d after the task finished — every number must be returned", st.Held)
	}
	// 取到的号个数 == 释放的号个数。
	if want := int(f.getPhone.Load()); st.Released != want {
		t.Fatalf("released=%d but %d numbers were taken; all must be returned", st.Released, want)
	}
	// 本测试的号全部收不到码（fake 默认"等待短信"）→ 失败路径必须拉黑。
	if got := int(f.blacklist.Load()); got != int(f.getPhone.Load()) {
		t.Fatalf("blacklisted=%d but took %d numbers (all failed numbers must be blacklisted)", got, f.getPhone.Load())
	}
}

// TestFailedReleaseIsRetriedAtTaskEnd 单次释放请求失败时，收尾兜底必须再试一次。
// 这正是"任务结束后额度仍被占着"的实际成因：某次 HTTP 释放没成功就漏掉了。
//
// 关键是把任务限制成**只跑一次尝试**：否则后续尝试的 finish() 会碰巧把号
// 释放掉，收尾兜底就永远用不上，测试会变成空转（这个坑踩过一次）。
func TestFailedReleaseIsRetriedAtTaskEnd(t *testing.T) {
	f := newFakeHZM(t)
	// 第一次 cancelRecv 失败（模拟网络抖动），之后的成功——所以只有收尾兜底
	// 能救回这个号。
	f.releaseFail.Store(1)
	en := NewAutoEnroller(successSMSManager(t), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.retryDelay = time.Millisecond
	en.pollInterval = time.Millisecond
	en.pollCount = 1

	// max_attempts=1：整个任务只有一次尝试，它的释放失败了，任务随即结束。
	if err := en.AutoRunWith(AutoRunOptions{Want: 1, Workers: 1, MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	waitDone(t, en)

	if n := int(f.getPhone.Load()); n != 1 {
		t.Fatalf("getPhone=%d, this test needs exactly one attempt", n)
	}
	st := en.Status()
	if st.Held != 0 {
		t.Fatalf("held=%d — the failed release must be retried during teardown, not leaked", st.Held)
	}
	if st.Released != 1 {
		t.Fatalf("released=%d want 1 (the one number taken must be returned)", st.Released)
	}
}

// TestReclaimOrphansOnStartup 进程被 SIGKILL（容器重启）后，账本里遗留的号
// 必须在下一次启动时补释放——否则它们会一直占着豪猪的取号额度，后续每次
// 取号都报「余额不足,请释放拉黑后再取号」，看着像账户没钱。
func TestReclaimOrphansOnStartup(t *testing.T) {
	f := newFakeHZM(t)
	dir := t.TempDir()
	ledger := filepath.Join(dir, "autoenroll-held.json")

	// 模拟上次进程留下的账本：两个号取走了但没释放。
	if err := os.WriteFile(ledger, []byte(`["17000000001","17000000002"]`), 0o600); err != nil {
		t.Fatal(err)
	}

	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.SetLedgerPath(ledger)

	if n := en.ReclaimOrphans(); n != 2 {
		t.Fatalf("reclaimed=%d want 2", n)
	}
	if got := f.releaseOK.Load(); got != 2 {
		t.Fatalf("releaseOK=%d want 2 — every orphan must be released", got)
	}
	// 遗留号不拉黑：账本可能混有接码已成功的号（成功路径释放失败也会留下），
	// 拉黑它会让号主以后在本项目收不到码。真正收不到码的号，下次被取到时
	// 走 tryOne 失败路径自然会拉黑。
	if got := f.blacklist.Load(); got != 0 {
		t.Fatalf("blacklist=%d want 0 — reclaim must not blacklist (ledger may contain successful numbers)", got)
	}
	// 账本必须被清空，否则每次启动都重复释放同一批号。
	if b, err := os.ReadFile(ledger); err != nil {
		t.Fatalf("read ledger: %v", err)
	} else if strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("ledger not cleared after reclaim: %s", b)
	}
}

// TestLedgerPersistsHeldNumbers 取号后账本必须立刻落盘：进程随时可能被
// SIGKILL，攒着批量写会丢掉"这个号还没还"的记录。
func TestLedgerPersistsHeldNumbers(t *testing.T) {
	f := newFakeHZM(t)
	ledger := filepath.Join(t.TempDir(), "autoenroll-held.json")
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.SetLedgerPath(ledger)

	en.trackHeld("17000000009")
	raw, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatalf("ledger must be written immediately on track: %v", err)
	}
	if !strings.Contains(string(raw), "17000000009") {
		t.Fatalf("ledger missing the held number: %s", raw)
	}

	// 释放后要从账本里消失（否则下次启动会去释放一个已经还掉的号）。
	en.markReleased("17000000009")
	raw, _ = os.ReadFile(ledger)
	if strings.Contains(string(raw), "17000000009") {
		t.Fatalf("released number still in ledger: %s", raw)
	}
}

// TestReclaimOrphansWithoutLedger 没配账本（或文件不存在）时必须安静返回 0，
// 不能报错也不能 panic——这是"从没跑过自动加号"的常见情况。
func TestReclaimOrphansWithoutLedger(t *testing.T) {
	f := newFakeHZM(t)
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	// 未设置 ledgerPath。
	if n := en.ReclaimOrphans(); n != 0 {
		t.Fatalf("reclaimed=%d want 0 when no ledger is configured", n)
	}
	// 设置了路径但文件不存在。
	en.SetLedgerPath(filepath.Join(t.TempDir(), "missing.json"))
	if n := en.ReclaimOrphans(); n != 0 {
		t.Fatalf("reclaimed=%d want 0 when the ledger file is absent", n)
	}
	// 损坏的账本不该让服务起不来。
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`{"not":"an array"}`), 0o600)
	en.SetLedgerPath(bad)
	if n := en.ReclaimOrphans(); n != 0 {
		t.Fatalf("reclaimed=%d want 0 for a corrupt ledger", n)
	}
}

// TestStuckNumberRetriedAtNextRunStart 上一轮释放失败的号，必须在本轮**取号之前**
// 先补释放。
//
// 顺序是重点：遗留号占着豪猪的取号额度，如果等到任务收尾才释放，本轮所有取号
// 都会失败并报「余额不足,请释放拉黑后再取号」——看着像没钱，实际是旧号没还。
// 所以断言的是"释放发生在第一次取号之前"，而不只是"最终被释放了"。
func TestStuckNumberRetriedAtNextRunStart(t *testing.T) {
	f := newFakeHZM(t)
	ledger := filepath.Join(t.TempDir(), "autoenroll-held.json")
	en := NewAutoEnroller(successSMSManager(t), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.SetLedgerPath(ledger)
	en.retryDelay = time.Millisecond
	en.pollInterval = time.Millisecond
	en.pollCount = 1

	// 模拟上一轮遗留：账本里有个号还没还。
	en.trackHeld("17000000001")

	if err := en.AutoRunWith(AutoRunOptions{Want: 1, Workers: 1, MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	waitDone(t, en)

	// 关键断言：补释放必须早于第一次取号。
	releaseAt := f.firstReleaseSeq.Load()
	phoneAt := f.firstPhoneSeq.Load()
	if releaseAt == 0 {
		t.Fatal("the stuck number was never released")
	}
	if phoneAt == 0 {
		t.Fatal("no number was ever requested; test setup is wrong")
	}
	if releaseAt > phoneAt {
		t.Fatalf("stuck number released at seq=%d but the first getPhone was at seq=%d — "+
			"it must be released BEFORE the run starts taking numbers, otherwise every "+
			"getPhone fails with a misleading 'insufficient balance' error", releaseAt, phoneAt)
	}
	// 且不能还留在账本里（它已确认归还）。
	if b, _ := os.ReadFile(ledger); strings.Contains(string(b), "17000000001") {
		t.Fatalf("stuck number still in ledger after being released: %s", b)
	}
}

// TestStuckNumberKeptWhenReleaseKeepsFailing 如果补释放一直失败，账本必须
// 保留该号——清空等于永久遗忘一个仍占着额度的号。
func TestStuckNumberKeptWhenReleaseKeepsFailing(t *testing.T) {
	f := newFakeHZM(t)
	ledger := filepath.Join(t.TempDir(), "autoenroll-held.json")
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.SetLedgerPath(ledger)
	// 让释放永远失败（模拟豪猪持续报错）。
	f.releaseFail.Store(9999)

	if err := os.WriteFile(ledger, []byte(`["17000000007"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := en.ReclaimOrphans(); n != 0 {
		t.Fatalf("reclaimed=%d want 0 (release kept failing)", n)
	}
	// 关键：账本必须**留着**这个号，否则下次再也不会重试。
	raw, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "17000000007") {
		t.Fatalf("ledger must keep a number whose release failed: %s", raw)
	}
}

// TestGoneNumberCountsAsReclaimed 豪猪说"手机号不存在"时，号已不在它手里，
// 必须算归还并从账本移除——否则账本会永远留着一个不存在的号。
func TestGoneNumberCountsAsReclaimed(t *testing.T) {
	f := newFakeHZM(t)
	ledger := filepath.Join(t.TempDir(), "autoenroll-held.json")
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.SetLedgerPath(ledger)
	// 豪猪对已失效的号返回"很抱歉,手机号不存在"（实测文案）。
	f.releaseGone.Store(true)

	if err := os.WriteFile(ledger, []byte(`["17000000008"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := en.ReclaimOrphans(); n != 1 {
		t.Fatalf("reclaimed=%d want 1 (a nonexistent number counts as returned)", n)
	}
	if raw, _ := os.ReadFile(ledger); strings.Contains(string(raw), "17000000008") {
		t.Fatalf("gone number must be dropped from the ledger: %s", raw)
	}
}

// TestReleaseFailedCountsAsReclaimed 一键释放/占用过期后，豪猪对逐号
// cancelRecv 回 code=-1 "释放失败"（实测：51 个号 1 秒内全部同文案）。
// 这些号不在豪猪手里，必须算归还，否则账本永久滞留、每次任务开跑空转。
func TestReleaseFailedCountsAsReclaimed(t *testing.T) {
	f := newFakeHZM(t)
	// cancelRecv 一律回"释放失败"（模拟豪猪对无占用记录的通用拒绝）。
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("api") {
		case "login":
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","token":"tok-1"}`))
		case "cancelRecv":
			_, _ = w.Write([]byte(`{"code":"-1","msg":"释放失败"}`))
		default:
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok"}`))
		}
	})
	ledger := filepath.Join(t.TempDir(), "autoenroll-held.json")
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.SetLedgerPath(ledger)
	if err := os.WriteFile(ledger, []byte(`["17000000001","17000000002"]`), 0o600); err != nil {
		t.Fatal(err)
	}

	if n := en.ReclaimOrphans(); n != 2 {
		t.Fatalf("reclaimed=%d want 2 ('释放失败' means the number is gone upstream)", n)
	}
	st := en.Status()
	if st.Held != 0 {
		t.Fatalf("held=%d want 0 (ledger must be cleared)", st.Held)
	}
}

// TestReleaseAllHeldViaCancelAllRecv 一键释放走豪猪的 cancelAllRecv（平台侧
// 全量释放），并清空整个账本——包括账本文件。held 统计随之归零，released
// 补记账本遗留数。
func TestReleaseAllHeldViaCancelAllRecv(t *testing.T) {
	f := newFakeHZM(t)
	var cancelAll atomic.Int32
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("api") {
		case "login":
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","token":"tok-1"}`))
		case "cancelAllRecv":
			cancelAll.Add(1)
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok"}`))
		default:
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok"}`))
		}
	})
	ledger := filepath.Join(t.TempDir(), "autoenroll-held.json")
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.SetLedgerPath(ledger)
	// 模拟账本里有 3 个遗留号。
	for _, p := range []string{"17000000001", "17000000002", "17000000003"} {
		en.trackHeld(p)
	}

	done, heldBefore, err := en.ReleaseAllHeld(context.Background())
	if err != nil || !done {
		t.Fatalf("ReleaseAllHeld = (%v, %d, %v), want (true, 3, nil)", done, heldBefore, err)
	}
	if heldBefore != 3 {
		t.Fatalf("heldBefore=%d want 3", heldBefore)
	}
	if cancelAll.Load() != 1 {
		t.Fatalf("cancelAllRecv called %d times, want 1", cancelAll.Load())
	}
	st := en.Status()
	if st.Held != 0 {
		t.Fatalf("held=%d after release-all, want 0", st.Held)
	}
	if st.Released != 3 {
		t.Fatalf("released=%d want 3 (ledger backlog counted)", st.Released)
	}
	if raw, _ := os.ReadFile(ledger); strings.TrimSpace(string(raw)) != "[]" {
		t.Fatalf("ledger must be cleared after release-all: %s", raw)
	}
}

// TestReleaseAllHeldRejectedWhileRunning 任务运行中拒绝一键释放：
// cancelAllRecv 会把正在收码的在途号码一起放掉，所有 worker 白等。
func TestReleaseAllHeldRejectedWhileRunning(t *testing.T) {
	f := newFakeHZM(t)
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.retryDelay = time.Millisecond

	if err := en.AutoRun(50, 1); err != nil {
		t.Fatal(err)
	}
	defer waitDone(t, en)

	_, _, err := en.ReleaseAllHeld(context.Background())
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("ReleaseAllHeld during a run = %v, want ErrBusy", err)
	}
}

// TestReleaseAllHeldUpstreamError 豪猪侧失败时账本必须原样保留
// （清空等于遗忘仍占额度的号）。
func TestReleaseAllHeldUpstreamError(t *testing.T) {
	f := newFakeHZM(t)
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("api") {
		case "login":
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","token":"tok-1"}`))
		case "cancelAllRecv":
			_, _ = w.Write([]byte(`{"code":"-1","msg":"释放失败（模拟）"}`))
		default:
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok"}`))
		}
	})
	ledger := filepath.Join(t.TempDir(), "autoenroll-held.json")
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.SetLedgerPath(ledger)
	en.trackHeld("17000000001")

	done, _, err := en.ReleaseAllHeld(context.Background())
	if err == nil || done {
		t.Fatalf("ReleaseAllHeld = (%v, _, %v), want (false, _, error)", done, err)
	}
	if st := en.Status(); st.Held != 1 {
		t.Fatalf("held=%d after a failed release-all, want 1 (ledger preserved)", st.Held)
	}
}

func waitDone(t *testing.T, en *AutoEnroller) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if !en.Status().Running {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("auto-enroll did not finish in 30s")
}
