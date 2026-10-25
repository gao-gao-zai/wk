package haozhuma

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newEcodeTestServer 搭一个 E码云风格的纯文本 API fake。
// 各动作的响应文本可定制；默认按 action 返回常见值。
func newEcodeTestServer(t *testing.T, respond func(action string, q url.Values) (int, string)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Query().Get("code")
		status, body := respond(action, r.URL.Query())
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newEcodeClientFor(srv *httptest.Server, token string) *Client {
	c := NewEcode(srv.URL, token)
	c.Timeout = 3 * time.Second
	return c
}

func TestEcodeSummaryLeftAmount(t *testing.T) {
	srv := newEcodeTestServer(t, func(action string, q url.Values) (int, string) {
		if action != "leftAmount" {
			return 200, "ERROR:未知动作"
		}
		if q.Get("token") != "tok-1" {
			return 403, "forbidden"
		}
		return 200, "12.50"
	})
	c := newEcodeClientFor(srv, "tok-1")
	s, err := c.Summary(context.Background())
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if s.Balance != 12.50 {
		t.Errorf("Balance=%v want 12.50", s.Balance)
	}
	if s.Occupied != -1 {
		t.Errorf("Occupied=%v want -1（E码云无占用数）", s.Occupied)
	}
}

func TestEcodeSummaryBadToken(t *testing.T) {
	srv := newEcodeTestServer(t, func(action string, q url.Values) (int, string) {
		return 403, "forbidden"
	})
	c := newEcodeClientFor(srv, "bad")
	if _, err := c.Summary(context.Background()); err == nil {
		t.Fatal("坏 token 应报错")
	} else {
		if !strings.Contains(err.Error(), "token 失效") && !strings.Contains(err.Error(), "403") {
			t.Errorf("错误应提示 token 失效/403，got: %v", err)
		}
	}
}

func TestEcodeGetPhoneKeyword(t *testing.T) {
	var gotKeyword, gotCardType string
	srv := newEcodeTestServer(t, func(action string, q url.Values) (int, string) {
		if action == "getPhone" {
			gotKeyword = q.Get("keyWord")
			gotCardType = q.Get("cardType")
			return 200, "16512345678"
		}
		return 200, "ERROR:未知动作"
	})
	c := newEcodeClientFor(srv, "tok-1")
	phone, err := c.GetPhone(context.Background(), "腾讯科技")
	if err != nil {
		t.Fatalf("GetPhone: %v", err)
	}
	if phone != "16512345678" {
		t.Errorf("phone=%q want 16512345678", phone)
	}
	if gotKeyword != "腾讯科技" {
		t.Errorf("keyWord=%q want 腾讯科技（sid 语义映射）", gotKeyword)
	}
	if gotCardType != "全部" {
		t.Errorf("cardType=%q want 全部（默认）", gotCardType)
	}
}

func TestEcodeGetPhoneCardType(t *testing.T) {
	var gotCardType string
	srv := newEcodeTestServer(t, func(action string, q url.Values) (int, string) {
		if action == "getPhone" {
			gotCardType = q.Get("cardType")
			return 200, "15512345678"
		}
		return 200, "ERROR:x"
	})
	c := newEcodeClientFor(srv, "tok-1")
	c.CardType = "实卡"
	if _, err := c.GetPhone(context.Background(), "毛竹"); err != nil {
		t.Fatalf("GetPhone: %v", err)
	}
	if gotCardType != "实卡" {
		t.Errorf("cardType=%q want 实卡", gotCardType)
	}
}

func TestEcodeGetPhoneError(t *testing.T) {
	srv := newEcodeTestServer(t, func(action string, q url.Values) (int, string) {
		return 200, "ERROR:没有可用号码"
	})
	c := newEcodeClientFor(srv, "tok-1")
	_, err := c.GetPhone(context.Background(), "腾讯科技")
	if err == nil {
		t.Fatal("ERROR: 响应应报错")
	}
	if !strings.Contains(err.Error(), "没有可用号码") {
		t.Errorf("错误应包含上游文本，got: %v", err)
	}
}

func TestEcodeGetMessageWaiting(t *testing.T) {
	srv := newEcodeTestServer(t, func(action string, q url.Values) (int, string) {
		if action == "getMsg" {
			return 200, "[尚未收到]请稍后再试"
		}
		return 200, "ERROR:x"
	})
	c := newEcodeClientFor(srv, "tok-1")
	// 等待态 = 空 sms 不报错（对齐豪猪 code=-1 等待语义）。
	sms, err := c.GetMessage(context.Background(), "腾讯科技", "16512345678")
	if err != nil {
		t.Fatalf("等待态不应报错: %v", err)
	}
	if strings.TrimSpace(sms) != "" {
		t.Errorf("等待态应返回空 sms，got: %q", sms)
	}
}

func TestEcodeGetMessageSMS(t *testing.T) {
	var gotPhone, gotKeyword string
	srv := newEcodeTestServer(t, func(action string, q url.Values) (int, string) {
		if action == "getMsg" {
			gotPhone = q.Get("phone")
			gotKeyword = q.Get("keyWord")
			return 200, "【腾讯科技】您的验证码是 482913，5 分钟内有效"
		}
		return 200, "ERROR:x"
	})
	c := newEcodeClientFor(srv, "tok-1")
	sms, err := c.GetMessage(context.Background(), "腾讯科技", "16512345678")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if !strings.Contains(sms, "482913") {
		t.Errorf("sms 缺验证码: %q", sms)
	}
	if gotPhone != "16512345678" || gotKeyword != "腾讯科技" {
		t.Errorf("参数 phone=%q keyWord=%q", gotPhone, gotKeyword)
	}
	// 提取码复用豪猪 ExtractCode。
	if code := ExtractCode(sms); code != "482913" {
		t.Errorf("ExtractCode=%q want 482913", code)
	}
}

func TestEcodeNoopLifecycle(t *testing.T) {
	// release/blacklist/releaseAll/relogin 在 ecode 下应是 no-op。
	called := map[string]bool{}
	srv := newEcodeTestServer(t, func(action string, q url.Values) (int, string) {
		called[action] = true
		return 200, "should-not-be-called"
	})
	c := newEcodeClientFor(srv, "tok-1")
	ctx := context.Background()
	if err := c.Release(ctx, "腾讯科技", "16512345678"); err != nil {
		t.Errorf("Release 应 no-op: %v", err)
	}
	if err := c.Blacklist(ctx, "腾讯科技", "16512345678"); err != nil {
		t.Errorf("Blacklist 应 no-op: %v", err)
	}
	if err := c.ReleaseAll(ctx); err != nil {
		t.Errorf("ReleaseAll 应 no-op: %v", err)
	}
	if err := c.Relogin(); err != nil {
		t.Errorf("Relogin 应 no-op 成功: %v", err)
	}
	for action := range called {
		t.Errorf("ecode no-op 方法不应发起请求，但收到 action=%s", action)
	}
}

func TestEcodeFatalClassification(t *testing.T) {
	// pollCode 靠 APIError.Fatal()/Waiting() 分流；ERROR: 文本要能正确
	// 映射到这些判定（与豪猪共用同一套关键词表）。
	cases := []struct {
		text string
		want bool
	}{
		{"余额不足，请充值", true},
		{"没有可用号码", true},
		{"账号已被禁用", true},
		{"参数错误", false},
	}
	for _, tc := range cases {
		ae := &APIError{API: "getPhone", Code: "-1", Msg: tc.text}
		if got := ae.Fatal(); got != tc.want {
			t.Errorf("Fatal(%q)=%v want %v", tc.text, got, tc.want)
		}
	}
	// 等待判定：ecode 不走 APIError.Waiting（豪猪的 -1+等待 语义），
	// 走 ecodeWaiting 文本判定。
	if !ecodeWaiting("[尚未收到]xxx") {
		t.Error("ecodeWaiting 应识别 [尚未收到]")
	}
	if ecodeWaiting("【腾讯科技】验证码 1234") {
		t.Error("ecodeWaiting 不应把正常短信当等待")
	}
}

func TestEcodeGetPhoneRetryOnFatalLikeError(t *testing.T) {
	// 取号 ERROR 直接上抛（无 ISP 降级轮次——E码云没有 isp 参数）。
	calls := 0
	srv := newEcodeTestServer(t, func(action string, q url.Values) (int, string) {
		calls++
		return 200, "ERROR:余额不足，请充值"
	})
	c := newEcodeClientFor(srv, "tok-1")
	_, err := c.GetPhone(context.Background(), "毛竹")
	if err == nil {
		t.Fatal("应报错")
	}
	if calls != 1 {
		t.Errorf("ecode 取号失败应只调一次（无 ISP 轮次），calls=%d", calls)
	}
}

// —— 双平台共存（豪猪主用 + E码云兜底）——

// newDualTest 搭豪猪 fake（可控制挂/不挂）+ E码云 fake。
func newDualTest(t *testing.T, haozhumaUp bool, haozhumaBody string) (*httptest.Server, *httptest.Server) {
	t.Helper()
	hz := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !haozhumaUp {
			w.WriteHeader(466)
			_, _ = w.Write([]byte("maintenance"))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(haozhumaBody))
	}))
	ec := newEcodeTestServer(t, func(action string, q url.Values) (int, string) {
		switch action {
		case "getPhone":
			return 200, "17123456789"
		case "getMsg":
			if q.Get("phone") == "17123456789" {
				return 200, "【腾讯科技】验证码 666777"
			}
			return 200, "[尚未收到]xxx"
		case "leftAmount":
			return 200, "29.00"
		}
		return 200, "ERROR:未知动作"
	})
	t.Cleanup(hz.Close)
	return hz, ec
}

func TestFallbackGetPhoneWhenHaozhumaDown(t *testing.T) {
	// 豪猪 466 维护 → 自动切 E码云取号。
	hzSrv, ecSrv := newDualTest(t, false, "")
	main := New("hz-token")
	main.Base = hzSrv.URL + "/"
	main.Timeout = 2 * time.Second
	fb := NewEcode(ecSrv.URL, "ec-token")
	fb.Timeout = 2 * time.Second
	main.Fallback = fb

	phone, err := main.GetPhone(context.Background(), "腾讯科技")
	if err != nil {
		t.Fatalf("豪猪挂时兜底取号应成功: %v", err)
	}
	if phone != "17123456789" {
		t.Errorf("phone=%q want 17123456789", phone)
	}
	if main.sourceOf(phone) != "ecode" {
		t.Errorf("来源记账失败：sourceOf(%s)=%q", phone, main.sourceOf(phone))
	}
}

func TestFallbackGetMessageRouted(t *testing.T) {
	// 兜底取的号收码走 E码云（拿到短信），释放/拉黑 no-op 不碰豪猪。
	hzSrv, ecSrv := newDualTest(t, false, "")
	main := New("hz-token")
	main.Base = hzSrv.URL + "/"
	main.Timeout = 2 * time.Second
	fb := NewEcode(ecSrv.URL, "ec-token")
	fb.Timeout = 2 * time.Second
	main.Fallback = fb

	phone, err := main.GetPhone(context.Background(), "腾讯科技")
	if err != nil {
		t.Fatalf("取号: %v", err)
	}
	sms, err := main.GetMessage(context.Background(), "腾讯科技", phone)
	if err != nil {
		t.Fatalf("收码: %v", err)
	}
	if !strings.Contains(sms, "666777") {
		t.Errorf("sms=%q 应含验证码（走 E码云路由）", sms)
	}
	if code := ExtractCode(sms); code != "666777" {
		t.Errorf("ExtractCode=%q", code)
	}
	// 释放/拉黑：ecode 来源 no-op 成功。
	if err := main.Release(context.Background(), "腾讯科技", phone); err != nil {
		t.Errorf("Release ecode 来源应 no-op: %v", err)
	}
	if err := main.Blacklist(context.Background(), "腾讯科技", phone); err != nil {
		t.Errorf("Blacklist ecode 来源应 no-op: %v", err)
	}
}

func TestFallbackSummaryMergedWhenHaozhumaDown(t *testing.T) {
	// 豪猪挂 + E码云活：Summary 返回 E码云余额 + 附带说明的错误。
	hzSrv, ecSrv := newDualTest(t, false, "")
	main := New("hz-token")
	main.Base = hzSrv.URL + "/"
	main.Timeout = 2 * time.Second
	fb := NewEcode(ecSrv.URL, "ec-token")
	fb.Timeout = 2 * time.Second
	main.Fallback = fb

	s, err := main.Summary(context.Background())
	if s.Balance != 29.00 {
		t.Errorf("Balance=%v want 29.00（E码云兜底）", s.Balance)
	}
	if err == nil || !strings.Contains(err.Error(), "E码云兜底") {
		t.Errorf("err 应附带豪猪不可用说明, got: %v", err)
	}
}

func TestFallbackNotTriggeredWhenHaozhumaOK(t *testing.T) {
	// 豪猪正常（JSON 响应）→ 不走兜底，号码来源为空（主平台语义）。
	hzSrv, ecSrv := newDualTest(t, true, `{"code":"200","msg":"success","phone":"13911112222"}`)
	main := New("hz-token")
	main.Base = hzSrv.URL + "/"
	main.Timeout = 2 * time.Second
	fb := NewEcode(ecSrv.URL, "ec-token")
	fb.Timeout = 2 * time.Second
	main.Fallback = fb

	phone, err := main.GetPhone(context.Background(), "52283")
	if err != nil {
		t.Fatalf("豪猪正常时取号: %v", err)
	}
	if phone != "13911112222" {
		t.Errorf("phone=%q want 豪猪侧号码", phone)
	}
	if main.sourceOf(phone) != "" {
		t.Errorf("豪猪号码不应记 ecode 来源")
	}
}

func TestFallbackBothFailReportsBoth(t *testing.T) {
	// 两边都挂：错误信息包含两边失败原因。
	hzSrv, _ := newDualTest(t, false, "")
	ecDown := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("ecode down"))
	}))
	t.Cleanup(ecDown.Close)
	main := New("hz-token")
	main.Base = hzSrv.URL + "/"
	main.Timeout = 2 * time.Second
	fb := NewEcode(ecDown.URL, "ec-token")
	fb.Timeout = 2 * time.Second
	main.Fallback = fb

	_, err := main.GetPhone(context.Background(), "腾讯科技")
	if err == nil {
		t.Fatal("两边都挂应报错")
	}
	if !strings.Contains(err.Error(), "豪猪取号失败") || !strings.Contains(err.Error(), "兜底") {
		t.Errorf("错误应包含两边信息, got: %v", err)
	}
}

// —— 平台选择模式（Mode 热切换）——

// modeFixture 搭可编程双平台 fake：豪猪/E码云 各自返回独立号码，计数。
func modeFixture(t *testing.T, hzPhone, ecPhone string) (*Client, *int, *int) {
	t.Helper()
	hzCalls, ecCalls := 0, 0
	hzSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hzCalls++
		if hzPhone == "" {
			w.WriteHeader(466)
			_, _ = w.Write([]byte("maintenance"))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":"200","msg":"ok","phone":"` + hzPhone + `"}`))
	}))
	ecSrv := newEcodeTestServer(t, func(action string, q url.Values) (int, string) {
		if action == "getPhone" {
			ecCalls++
			if ecPhone == "" {
				return 200, "ERROR:没有可用号码"
			}
			return 200, ecPhone
		}
		if action == "leftAmount" {
			return 200, "10.00"
		}
		return 200, "ERROR:x"
	})
	main := New("hz-token")
	main.Base = hzSrv.URL + "/"
	main.Timeout = 2 * time.Second
	fb := NewEcode(ecSrv.URL, "ec-token")
	fb.Timeout = 2 * time.Second
	main.Fallback = fb
	return main, &hzCalls, &ecCalls
}

func TestModeBothAlternates(t *testing.T) {
	// both 模式：两次取号应交替走两侧（豪猪、E码云各一次）。
	main, hzCalls, ecCalls := modeFixture(t, "13911110001", "17111110002")
	main.Mode = "both"
	ctx := context.Background()
	p1, err := main.GetPhone(ctx, "腾讯科技")
	if err != nil {
		t.Fatalf("第1次取号: %v", err)
	}
	p2, err := main.GetPhone(ctx, "腾讯科技")
	if err != nil {
		t.Fatalf("第2次取号: %v", err)
	}
	// 两次应来自不同平台。
	if p1 == p2 {
		t.Errorf("both 模式两次应取到不同平台的号: p1=%s p2=%s", p1, p2)
	}
	if *hzCalls != 1 || *ecCalls != 1 {
		t.Errorf("both 应交替消耗两侧: hz=%d ec=%d, want 1/1", *hzCalls, *ecCalls)
	}
}

func TestModeBothFailsOverToSurvivor(t *testing.T) {
	// both 模式：豪猪挂 → 换边成功，继续出号。
	main, _, ecCalls := modeFixture(t, "", "17111110003")
	main.Mode = "both"
	for i := 0; i < 2; i++ {
		phone, err := main.GetPhone(context.Background(), "腾讯科技")
		if err != nil {
			t.Fatalf("both 豪猪挂时第%d次: %v", i+1, err)
		}
		if phone != "17111110003" {
			t.Errorf("应从 E码云取号, got %s", phone)
		}
	}
	if *ecCalls < 2 {
		t.Errorf("E码云应承担全部取号, ec=%d", *ecCalls)
	}
}

func TestModeEcodeOnlySkipsHaozhuma(t *testing.T) {
	// ecode 模式：豪猪活着也不碰。
	main, hzCalls, _ := modeFixture(t, "13911110004", "17111110005")
	main.Mode = "ecode"
	phone, err := main.GetPhone(context.Background(), "腾讯科技")
	if err != nil {
		t.Fatalf("ecode 模式取号: %v", err)
	}
	if phone != "17111110005" {
		t.Errorf("应从 E码云取号, got %s", phone)
	}
	if *hzCalls != 0 {
		t.Errorf("ecode 模式不应请求豪猪, hz=%d", *hzCalls)
	}
	// Summary 也只查 E码云。
	s, err := main.Summary(context.Background())
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if s.Balance != 10.00 {
		t.Errorf("Balance=%v want 10.00（仅 E码云）", s.Balance)
	}
}

func TestModeHaozhumaOnlyNoFallback(t *testing.T) {
	// haozhuma 模式：豪猪挂了也不上兜底，直接失败。
	main, _, ecCalls := modeFixture(t, "", "17111110006")
	main.Mode = "haozhuma"
	_, err := main.GetPhone(context.Background(), "腾讯科技")
	if err == nil {
		t.Fatal("haozhuma 模式豪猪挂时应失败（不兜底）")
	}
	if *ecCalls != 0 {
		t.Errorf("haozhuma 模式不应请求 E码云, ec=%d", *ecCalls)
	}
}

func TestModeAutoIsDefault(t *testing.T) {
	// Mode 空 = auto：豪猪优先失败切 E码云（原兜底语义）。
	main, hzCalls, _ := modeFixture(t, "", "17111110007")
	phone, err := main.GetPhone(context.Background(), "腾讯科技")
	if err != nil {
		t.Fatalf("auto 模式: %v", err)
	}
	if phone != "17111110007" {
		t.Errorf("应兜底到 E码云, got %s", phone)
	}
	if *hzCalls == 0 {
		t.Error("auto 模式应先试豪猪")
	}
}
