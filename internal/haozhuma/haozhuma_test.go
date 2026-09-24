package haozhuma

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestExtractCode(t *testing.T) {
	cases := map[string]string{
		"【腾讯科技】验证码502612，用于手机登录，5分钟内有效": "502612",
		"您的验证码是 8432，请勿泄露":              "8432",
		"【CodeBuddy】你的验证码：998877":       "998877",
		"no code here": "",
		"":             "",
	}
	for sms, want := range cases {
		if got := ExtractCode(sms); got != want {
			t.Errorf("ExtractCode(%q)=%q want %q", sms, got, want)
		}
	}
}

// TestClientFlow 用假服务器验证 login/getPhone/getMessage/release 的请求形状
// 与"等待"状态的轮询语义。
func TestClientFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("api") {
		case "login":
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","token":"tok-1"}`))
		case "getPhone":
			if r.URL.Query().Get("token") != "tok-1" || r.URL.Query().Get("sid") != "52283" {
				_, _ = w.Write([]byte(`{"code":"101","msg":"参数错误"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","phone":"17012345678"}`))
		case "getMessage":
			if r.URL.Query().Get("phone") != "17012345678" {
				_, _ = w.Write([]byte(`{"code":"102","msg":"号码不匹配"}`))
				return
			}
			// 不带 poll 参数时返回等待状态。
			if r.URL.Query().Get("poll") == "" {
				_, _ = w.Write([]byte(`{"code":"-1","msg":"等待短信"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","sms":"【腾讯科技】验证码502612，用于手机登录"}`))
		case "cancelRecv":
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok"}`))
		default:
			_, _ = w.Write([]byte(`{"code":"404","msg":"unknown api"}`))
		}
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL + "/", Timeout: 5 * time.Second, HTTP: srv.Client()}

	// login
	if err := c.loginFlow(); err != nil {
		t.Fatalf("login: %v", err)
	}
	if c.Token != "tok-1" {
		t.Fatalf("token=%q", c.Token)
	}

	// getPhone
	phone, err := c.GetPhone(context.Background(), "52283")
	if err != nil || phone != "17012345678" {
		t.Fatalf("GetPhone: phone=%q err=%v", phone, err)
	}

	// getMessage：带 poll=2 的请求（服务器用它区分两次轮询）。
	q := url.Values{"token": {"tok-1"}, "sid": {"52283"}, "phone": {phone}, "poll": {"2"}}
	raw, err := c.call(context.Background(), "getMessage", q)
	if err != nil {
		t.Fatalf("second GetMessage: %v", err)
	}
	sms, _ := raw["sms"].(string)
	if code := ExtractCode(sms); code != "502612" {
		t.Fatalf("code from sms=%q", code)
	}

	// release
	if err := c.Release(context.Background(), "52283", phone); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// TestGetPhoneSendsAuthorAndISP 「[限对接]」项目必须带 author，运营商过滤
// 必须带 isp —— 实测缺 author 拿不到号，不限 isp 只会放广电号（收不到腾讯短信）。
func TestGetPhoneSendsAuthorAndISP(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_, _ = w.Write([]byte(`{"code":"0","msg":"成功","phone":"19572980371","uid":"52283-ABC"}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := New("tok")
	c.Base = u.String() + "/"
	c.Author = "adminzfz"
	c.ISP = "1"

	if _, err := c.GetPhone(context.Background(), "52283"); err != nil {
		t.Fatal(err)
	}
	if got.Get("author") != "adminzfz" {
		t.Errorf("author=%q want adminzfz", got.Get("author"))
	}
	if got.Get("isp") != "1" {
		t.Errorf("isp=%q want 1", got.Get("isp"))
	}
	// uid 从取号响应里学到，供后续收码用。
	if c.UID() != "52283-ABC" {
		t.Errorf("uid=%q want 52283-ABC", c.UID())
	}
}

// TestGetPhoneOmitsEmptyFilters 普通项目（不配 author/isp）不该下发空参数。
func TestGetPhoneOmitsEmptyFilters(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_, _ = w.Write([]byte(`{"code":"0","msg":"成功","phone":"13800138000"}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := New("tok")
	c.Base = u.String() + "/"

	if _, err := c.GetPhone(context.Background(), "52283"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"author", "isp", "uid"} {
		if got.Has(k) {
			t.Errorf("param %s should be omitted when unset", k)
		}
	}
}

// TestGetPhoneISPFallback ISP 是优先级列表：前一档没号要退到下一档，
// 最后退回不限；不该因为移动号没货就整个取号失败。
func TestGetPhoneISPFallback(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isp := r.URL.Query().Get("isp")
		seen = append(seen, isp)
		// isp=1（移动）没号，其余有号。
		if isp == "1" {
			_, _ = w.Write([]byte(`{"code":"-1","msg":"没有取到号码，请重新尝试"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":"0","msg":"成功","phone":"13900139000"}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := New("tok")
	c.Base = u.String() + "/"
	c.ISP = "1,2"

	phone, err := c.GetPhone(context.Background(), "52283")
	if err != nil {
		t.Fatalf("should fall back to next ISP, got %v", err)
	}
	if phone != "13900139000" {
		t.Fatalf("phone=%q", phone)
	}
	// 应该先试 1，再试 2 并成功（不该再多试）。
	if len(seen) < 2 || seen[0] != "1" || seen[1] != "2" {
		t.Fatalf("isp attempts=%v want [1 2]", seen)
	}
}

// TestGetPhoneISPListReachesUnrestricted 所有指定档都没号时退回"不限运营商"。
func TestGetPhoneISPListReachesUnrestricted(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isp := r.URL.Query().Get("isp")
		seen = append(seen, isp)
		if isp != "" {
			_, _ = w.Write([]byte(`{"code":"-1","msg":"没有取到号码，请重新尝试"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":"0","msg":"成功","phone":"19200192000"}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := New("tok")
	c.Base = u.String() + "/"
	c.ISP = "1,2,3"

	phone, err := c.GetPhone(context.Background(), "52283")
	if err != nil {
		t.Fatal(err)
	}
	if phone != "19200192000" {
		t.Fatalf("phone=%q", phone)
	}
	if len(seen) != 4 || seen[3] != "" {
		t.Fatalf("isp attempts=%v want 1,2,3 then unrestricted", seen)
	}
}

// TestGetPhoneFatalErrorStopsFallback 余额不足这类致命错误不该继续试下一档。
func TestGetPhoneFatalErrorStopsFallback(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"code":"201","msg":"余额不足，请充值"}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := New("tok")
	c.Base = u.String() + "/"
	c.ISP = "1,2,3"

	if _, err := c.GetPhone(context.Background(), "52283"); err == nil {
		t.Fatal("want fatal error")
	}
	if calls != 1 {
		t.Fatalf("fatal error should stop after 1 call, got %d", calls)
	}
}

// TestNetworkErrorDoesNotLeakToken 网络错误绝不能把 token 带进日志：
// *url.Error 的 Error() 会拼出完整 URL（含 token=...）。
func TestNetworkErrorDoesNotLeakToken(t *testing.T) {
	const secret = "SUPERSECRETTOKEN1234567890"
	// 指向一个必然连不上的地址，触发 *url.Error。
	c := New(secret)
	c.Base = "http://127.0.0.1:1/"
	c.Timeout = 2 * time.Second

	_, err := c.GetPhone(context.Background(), "52283")
	if err == nil {
		t.Fatal("want network error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("token leaked into error: %v", err)
	}
	// 也不该泄露其它 query 参数拼成的完整 URL。
	if strings.Contains(err.Error(), "token=") {
		t.Fatalf("query leaked into error: %v", err)
	}
}

// TestGetPhoneDropsStaleUID 钉死的对接码失效时必须自动退回平台自动分配。
//
// 真实场景：配置里写死 uid=52283-XXXX，上游把该对接码删了，之后每次取号都
// 报「没有这个[52283-XXXX]专属码」。修好之前整个任务会立刻失败退出。
func TestGetPhoneDropsStaleUID(t *testing.T) {
	var uidAttempts, autoAttempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("uid") != "" {
			uidAttempts++
			_, _ = w.Write([]byte(`{"code":"-1","msg":"没有这个[52283-STALE]专属码"}`))
			return
		}
		autoAttempts++
		_, _ = w.Write([]byte(`{"code":"0","msg":"成功","phone":"16725646683","uid":"52283-NEW"}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := New("tok")
	c.Base = u.String() + "/"
	c.SetUID("52283-STALE")

	phone, err := c.GetPhone(context.Background(), "52283")
	if err != nil {
		t.Fatalf("stale uid must fall back to auto-assign, got %v", err)
	}
	if phone != "16725646683" {
		t.Fatalf("phone=%q", phone)
	}
	if uidAttempts != 1 {
		t.Errorf("should try the pinned uid exactly once, got %d", uidAttempts)
	}
	if autoAttempts != 1 {
		t.Errorf("should retry without uid once, got %d", autoAttempts)
	}
	// 失效的对接码要从客户端里清掉，后面不再带着它取号。
	if got := c.UID(); got == "52283-STALE" {
		t.Errorf("stale uid must be cleared, still %q", got)
	}
}

// TestGetPhoneUnknownUIDIsNotFatal 「专属码不存在」不能算致命错误——
// 否则 AutoEnroller 会停掉整个任务而不是换一个对接码。
func TestGetPhoneUnknownUIDIsNotFatal(t *testing.T) {
	e := &APIError{API: "getPhone", Code: "-1", Msg: "没有这个[52283-X]专属码"}
	if !e.UnknownUID() {
		t.Fatal("should be detected as unknown uid")
	}
	if e.Fatal() {
		t.Fatal("unknown uid must not be fatal")
	}
	if e.TokenInvalid() {
		t.Fatal("unknown uid must not look like a token problem")
	}
}

// TestGetPhoneStaleUIDThenNoNumbers 退回自动分配后仍然没号时，
// 要如实报"没号"，不能把过期的对接码错误一直冒泡上去。
func TestGetPhoneStaleUIDThenNoNumbers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("uid") != "" {
			_, _ = w.Write([]byte(`{"code":"-1","msg":"没有这个[52283-STALE]专属码"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":"-1","msg":"没有取到号码，请重新尝试"}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := New("tok")
	c.Base = u.String() + "/"
	c.SetUID("52283-STALE")

	_, err := c.GetPhone(context.Background(), "52283")
	if err == nil {
		t.Fatal("want an error when no numbers are available")
	}
	if strings.Contains(err.Error(), "专属码") {
		t.Fatalf("error should reflect the real cause (no numbers), got %v", err)
	}
	if c.UID() != "" {
		t.Fatalf("stale uid should stay cleared, got %q", c.UID())
	}
}

// TestGetPhoneUIDPoolRotation 多对接码轮换池：round-robin 逐码取号；
// 失效码自动出池并进 drained（上层据此从豪猪账户移除）；池空后退回
// 平台自动分配。
func TestGetPhoneUIDPoolRotation(t *testing.T) {
	var autoAttempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := r.URL.Query().Get("uid")
		switch uid {
		case "52283-P1":
			_, _ = w.Write([]byte(`{"code":"-1","msg":"没有这个[52283-P1]专属码"}`))
		case "52283-P2":
			_, _ = w.Write([]byte(`{"code":"0","msg":"成功","phone":"16711112222"}`))
		case "":
			autoAttempts++
			_, _ = w.Write([]byte(`{"code":"0","msg":"成功","phone":"16733334444"}`))
		default:
			t.Errorf("unexpected uid %q", uid)
			_, _ = w.Write([]byte(`{"code":"-1","msg":"bad"}`))
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := New("tok")
	c.Base = u.String() + "/"
	c.SetUIDs([]string{"52283-P1", "52283-P2"})

	// 第一次：P1 失效出池 → P2 接上取到号。
	phone, err := c.GetPhone(context.Background(), "52283")
	if err != nil || phone != "16711112222" {
		t.Fatalf("first: phone=%q err=%v (want P2's number)", phone, err)
	}
	// P1 进 drained（等上层移出账户）。
	if d := c.TakeDrainedUIDs(); len(d) != 1 || d[0] != "52283-P1" {
		t.Fatalf("drained=%v want [52283-P1]", d)
	}
	// 池里只剩 P2。
	if got := c.UIDs(); len(got) != 1 || got[0] != "52283-P2" {
		t.Fatalf("pool after drop=%v", got)
	}

	// 第二次：P2 还在池里，round-robin 继续用它。
	phone, err = c.GetPhone(context.Background(), "52283")
	if err != nil || phone != "16711112222" {
		t.Fatalf("second: phone=%q err=%v", phone, err)
	}
	if autoAttempts != 0 {
		t.Fatalf("pool mode must not leak to auto-assign while pool has live codes, auto=%d", autoAttempts)
	}
}

// TestGetPhoneUIDPoolExhausted 池全部失效：退回平台自动分配而不是报错。
func TestGetPhoneUIDPoolExhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if uid := r.URL.Query().Get("uid"); uid != "" {
			_, _ = w.Write([]byte(fmt.Sprintf(`{"code":"-1","msg":"没有这个[%s]专属码"}`, uid)))
			return
		}
		_, _ = w.Write([]byte(`{"code":"0","msg":"成功","phone":"16755556666"}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := New("tok")
	c.Base = u.String() + "/"
	c.SetUIDs([]string{"52283-DEAD1", "52283-DEAD2"})

	phone, err := c.GetPhone(context.Background(), "52283")
	if err != nil || phone != "16755556666" {
		t.Fatalf("phone=%q err=%v (want auto-assign fallback)", phone, err)
	}
	if d := c.TakeDrainedUIDs(); len(d) != 2 {
		t.Fatalf("drained=%v want both dead codes", d)
	}
	if got := c.UIDs(); len(got) != 0 {
		t.Fatalf("pool should be empty, got %v", got)
	}
}

// TestGetPhoneUIDPoolNoNumbers 池里的码没失效但项目没号：如实报错
//（不逐码放大请求量）。
func TestGetPhoneUIDPoolNoNumbers(t *testing.T) {
	var uidCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("uid") != "" {
			uidCalls++
			_, _ = w.Write([]byte(`{"code":"-1","msg":"没有取到号码，请重新尝试"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":"-1","msg":"没有取到号码，请重新尝试"}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := New("tok")
	c.Base = u.String() + "/"
	c.SetUIDs([]string{"52283-A", "52283-B", "52283-C"})

	_, err := c.GetPhone(context.Background(), "52283")
	if err == nil {
		t.Fatal("want no-numbers error")
	}
	// 没号不是码失效：只试了一个码（+池耗尽后的自动分配一次），drained 为空。
	if uidCalls > 1 {
		t.Fatalf("no-numbers must not rotate through the whole pool, uid calls=%d", uidCalls)
	}
	if d := c.TakeDrainedUIDs(); len(d) != 0 {
		t.Fatalf("no-numbers must not drain codes, got %v", d)
	}
}

// TestSetUIDsDedup 池配置去重去空。
func TestSetUIDsDedup(t *testing.T) {
	c := New("tok")
	c.SetUIDs([]string{" 52283-A ", "", "52283-A", "52283-B"})
	if got := c.UIDs(); len(got) != 2 || got[0] != "52283-A" || got[1] != "52283-B" {
		t.Fatalf("pool=%v", got)
	}
}

// loginFlow 走 login 接口填 token（测试辅助）。
func (c *Client) loginFlow() error {
	raw, err := c.call(context.Background(), "login", url.Values{"user": {"u"}, "pass": {"p"}})
	if err != nil {
		return err
	}
	tok, _ := raw["token"].(string)
	c.Token = strings.TrimSpace(tok)
	return nil
}
