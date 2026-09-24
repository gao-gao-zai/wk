package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newWebuiTestHandler 组装带 AutoEnroller + 回调的测试 handler，豪猪
// 端点指到 fakeHZM——覆盖"验证→落盘→热替换"全链。
// 返回的 authedHandler 自带测试鉴权头，直接 ServeHTTP 即可。
func newWebuiTestHandler(t *testing.T, seedConfig string) (authedHandler, *fakeHZM, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if seedConfig != "" {
		if err := os.WriteFile(cfgPath, []byte(seedConfig), 0600); err != nil {
			t.Fatal(err)
		}
	}
	f := newFakeHZM(t)
	// login 响应里带 token，鉴权验证链路要走 login。
	f.tokenResp.Store(`{"code":"0","msg":"ok","token":"tok-1"}`)
	f.summary.Store(`{"code":"0","msg":"ok","money":"58.20","num":2}`)
	// buildHaozhumaClient 的 Base 指到本 fake（生产是真实豪猪域名）。
	u, _ := url.Parse(f.srv.URL)
	hzBase := u.String() + "/"
	oldBase := haozhumaBase
	haozhumaBase = hzBase
	t.Cleanup(func() { haozhumaBase = oldBase })
	ah := newTestHandler(t, Config{ConfigPath: cfgPath})
	h := ah.Handler
	c := f.client()
	h.cfg.AutoEnroll = NewAutoEnroller(nil, c, "52283", nil, nil, nil)
	h.cfg.SetHaozhumaSid = h.cfg.AutoEnroll.SetSid
	h.cfg.SetHaozhumaFetch = h.cfg.AutoEnroll.UpdateFetchOptions
	h.cfg.ReplaceHaozhumaClient = h.cfg.AutoEnroll.ReplaceClient
	h.cfg.SetAutoEnrollLimits = h.cfg.AutoEnroll.SetLimits
	h.cfg.SetAutoEnrollRetry = func(sec int) {
		h.cfg.AutoEnroll.SetRetryDelay(time.Duration(sec) * time.Second)
	}
	return ah, f, cfgPath
}

// TestAdminConfigHaozhumaAuthPatch 鉴权字段的 WebUI 闭环：
// POST user/pass → 验证（login）→ 落盘（保留其他字段）→ ReplaceClient。
func TestAdminConfigHaozhumaAuthPatch(t *testing.T) {
	seed := `{
  "sms": {
    "haozhuma": {
      "user": "old", "pass": "oldpass", "sid": "52283",
      "uid": "keep-me", "isp": "1,2"
    }
  }
}`
	h, _, cfgPath := newWebuiTestHandler(t, seed)

	body, _ := json.Marshal(map[string]any{
		"checkin_hours":   []int{3},
		"keepalive_hours": []int{9},
		"sms":             map[string]any{"haozhuma": map[string]any{"user": "newuser", "pass": "newpass"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/config", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("POST status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Updated map[string]any `json:"updated"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Updated["haozhuma_auth"] != "reconnected" {
		t.Fatalf("updated=%v, want haozhuma_auth=reconnected", resp.Updated)
	}
	// 落盘：user/pass 更新，token 被清空（账密模式 token 不落盘），其余保留。
	raw, _ := os.ReadFile(cfgPath)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	hz := doc["sms"].(map[string]any)["haozhuma"].(map[string]any)
	if hz["user"] != "newuser" || hz["pass"] != "newpass" {
		t.Fatalf("persisted auth not updated: %v", hz)
	}
	if tok, _ := hz["token"].(string); tok != "" {
		t.Fatalf("token should be cleared in userpass mode, got %q", tok)
	}
	if hz["uid"] != "keep-me" || hz["sid"] != "52283" || hz["isp"] != "1,2" {
		t.Fatalf("persisted haozhuma lost fields: %v", hz)
	}
}

// TestAdminConfigHaozhumaAuthBadCredentials 坏凭据必须被拒：不落盘、
// 不换客户端（回滚语义——旧客户端继续服务）。
func TestAdminConfigHaozhumaAuthBadCredentials(t *testing.T) {
	seed := `{
  "sms": {
    "haozhuma": {
      "user": "old", "pass": "oldpass", "sid": "52283"
    }
  }
}`
	h, f, cfgPath := newWebuiTestHandler(t, seed)
	// login 与 summary 全部失败（旧客户端此刻也查不了余额是预期内的——
	// fake 是全局响应；reject 之后恢复 summary 再验旧客户端）。
	f.tokenResp.Store(`{"code":"-1","msg":"账号或密码错误"}`)
	f.summary.Store(`{"code":"-1","msg":"token 无效"}`)

	body, _ := json.Marshal(map[string]any{
		"checkin_hours":   []int{3},
		"keepalive_hours": []int{9},
		"sms":             map[string]any{"haozhuma": map[string]any{"user": "bad", "pass": "bad"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/config", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("want 400 for bad credentials, got %d: %s", rec.Code, rec.Body.String())
	}
	// 配置文件未被改写。
	raw, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(raw), `"bad"`) {
		t.Fatalf("bad credentials must not be persisted: %s", raw)
	}
	// 运行时客户端未换：恢复 summary 后旧客户端还能用（token 还是 tok-1）。
	f.summary.Store(`{"code":"0","msg":"ok","money":"58.20","num":2}`)
	if _, err := h.cfg.AutoEnroll.Balance(context.Background()); err != nil {
		t.Fatalf("old client must still work after rejected patch: %v", err)
	}
}

// TestAdminConfigHaozhumaAuthRejectedWhileRunning 任务运行中拒绝鉴权 patch（409）。
func TestAdminConfigHaozhumaAuthRejectedWhileRunning(t *testing.T) {
	seed := `{
  "sms": {
    "haozhuma": {
      "user": "old", "pass": "oldpass", "sid": "52283"
    }
  }
}`
	h, f, _ := newWebuiTestHandler(t, seed)
	// 造一个挂着的运行任务：取号永远可重试地失败。
	f.phoneResp.Store(`{"code":"-1","msg":"没有取到号码，请重新尝试"}`)
	h.cfg.AutoEnroll.consecutiveFails = 100000
	h.cfg.AutoEnroll.retryDelay = 1
	if err := h.cfg.AutoEnroll.AutoRunWith(AutoRunOptions{Want: 5, Workers: 1}); err != nil {
		t.Fatal(err)
	}
	defer h.cfg.AutoEnroll.Stop("test")
	defer waitDone(t, h.cfg.AutoEnroll)

	body, _ := json.Marshal(map[string]any{
		"checkin_hours":   []int{3},
		"keepalive_hours": []int{9},
		"sms":             map[string]any{"haozhuma": map[string]any{"pass": "changed"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/config", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 409 {
		t.Fatalf("want 409 while running, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAdminConfigHaozhumaFetchPatch 取号字段（author/uid/isp）的校验、
// 落盘与热改闭环。
func TestAdminConfigHaozhumaFetchPatch(t *testing.T) {
	seed := `{
  "sms": {
    "haozhuma": {
      "user": "u", "pass": "p", "sid": "52283",
      "uid": "52283-OLD", "isp": "1,2", "author": "old"
    }
  }
}`
	h, _, cfgPath := newWebuiTestHandler(t, seed)

	// 非法 uid / isp 先拒。
	for _, bad := range []map[string]any{
		{"uid": "not-a-uid"},
		{"uid": "52283_underscore"},
		{"isp": "4,1"},
		{"isp": "1,1"},
		{"isp": "x"},
	} {
		body, _ := json.Marshal(map[string]any{
			"checkin_hours":   []int{3},
			"keepalive_hours": []int{9},
			"sms":             map[string]any{"haozhuma": bad},
		})
		req := httptest.NewRequest(http.MethodPost, "/admin/config", strings.NewReader(string(body)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Fatalf("bad patch %v: want 400, got %d", bad, rec.Code)
		}
	}

	// 合法 patch：改 author/uid/isp，落盘 + UpdateFetchOptions 生效。
	body, _ := json.Marshal(map[string]any{
		"checkin_hours":   []int{3},
		"keepalive_hours": []int{9},
		"sms": map[string]any{"haozhuma": map[string]any{
			"author": "", "uid": "52283-NEW", "isp": "2,1",
		}},
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/config", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("POST status %d: %s", rec.Code, rec.Body.String())
	}
	raw, _ := os.ReadFile(cfgPath)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	hz := doc["sms"].(map[string]any)["haozhuma"].(map[string]any)
	if hz["uid"] != "52283-NEW" || hz["isp"] != "2,1" {
		t.Fatalf("persisted fetch opts not updated: %v", hz)
	}
	// 运行时热改生效。
	s, err := h.cfg.AutoEnroll.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.UID != "52283-NEW" || s.ISP != "2,1" || s.Author != "" {
		t.Fatalf("runtime fetch opts = uid %q isp %q author %q, want NEW/2,1/empty", s.UID, s.ISP, s.Author)
	}
}

// TestAdminConfigAutoEnrollPatch 任务控制参数的校验 + 落盘 + 热改闭环。
func TestAdminConfigAutoEnrollPatch(t *testing.T) {
	h, _, cfgPath := newWebuiTestHandler(t, `{}`)

	// 越界值拒绝。
	for _, bad := range []map[string]any{
		{"min_balance": -1},
		{"consecutive_fails": 0},
		{"consecutive_fails": 101},
		{"retry_delay_seconds": 0},
		{"retry_delay_seconds": 61},
	} {
		body, _ := json.Marshal(map[string]any{
			"checkin_hours":   []int{3},
			"keepalive_hours": []int{9},
			"autoenroll":      bad,
		})
		req := httptest.NewRequest(http.MethodPost, "/admin/config", strings.NewReader(string(body)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Fatalf("bad autoenroll %v: want 400, got %d", bad, rec.Code)
		}
	}

	// 合法值：落盘 + SetLimits/SetRetryDelay 即时生效。
	body, _ := json.Marshal(map[string]any{
		"checkin_hours":   []int{3},
		"keepalive_hours": []int{9},
		"autoenroll":      map[string]any{"min_balance": 5.5, "consecutive_fails": 40, "retry_delay_seconds": 9},
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/config", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("POST status %d: %s", rec.Code, rec.Body.String())
	}
	mb, cf := h.cfg.AutoEnroll.Limits()
	if mb != 5.5 || cf != 40 {
		t.Fatalf("runtime limits = (%v,%d), want (5.5,40)", mb, cf)
	}
	if d := h.cfg.AutoEnroll.RetryDelay(); d != 9*1e9 {
		t.Fatalf("runtime retryDelay = %v, want 9s", d)
	}
	raw, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(raw), `"min_balance": 5.5`) {
		t.Fatalf("autoenroll node not persisted: %s", raw)
	}
}

// TestAdminConfigHaozhumaEcho GET 回显：凭据脱敏（user 前后2位、pass/token
// 只回有无），sid/author/uid/isp 明文，autoenroll 节带生效值。
func TestAdminConfigHaozhumaEcho(t *testing.T) {
	seed := `{
  "sms": {
    "haozhuma": {
      "user": "myuser", "pass": "secret", "token": "", "sid": "52283",
      "uid": "52283-AB", "isp": "1", "author": "a1"
    }
  }
}`
	h, _, _ := newWebuiTestHandler(t, seed)
	req := httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET status %d", rec.Code)
	}
	body := rec.Body.String()
	// 凭据值绝不能出现在回显里。
	if strings.Contains(body, "secret") {
		t.Fatalf("pass leaked in echo: %s", body)
	}
	var got struct {
		SMS struct {
			Haozhuma struct {
				Sid    string `json:"sid"`
				Author string `json:"author"`
				UID    string `json:"uid"`
				ISP    string `json:"isp"`
				Auth   struct {
					Mode     string `json:"mode"`
					User     string `json:"user"`
					HasPass  bool   `json:"has_pass"`
					HasToken bool   `json:"has_token"`
				} `json:"auth"`
			} `json:"haozhuma"`
		} `json:"sms"`
		AutoEnroll struct {
			MinBalance       float64 `json:"min_balance"`
			ConsecutiveFails int     `json:"consecutive_fails"`
			RetryDelaySecs   int     `json:"retry_delay_seconds"`
		} `json:"autoenroll"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	hz := got.SMS.Haozhuma
	if hz.Sid != "52283" || hz.Author != "a1" || hz.UID != "52283-AB" || hz.ISP != "1" {
		t.Fatalf("echo fetch opts = %+v", hz)
	}
	if hz.Auth.Mode != "userpass" || hz.Auth.User != "my***er" || !hz.Auth.HasPass || hz.Auth.HasToken {
		t.Fatalf("echo auth = %+v, want userpass/my***er/has_pass", hz.Auth)
	}
	// autoenroll 回显生效值（fakeHZM 的 AutoEnroller 用默认值）。
	if got.AutoEnroll.MinBalance != 2.2 || got.AutoEnroll.ConsecutiveFails != 15 || got.AutoEnroll.RetryDelaySecs != 5 {
		t.Fatalf("echo autoenroll = %+v, want defaults 2.2/15/5", got.AutoEnroll)
	}
}

// TestHaozhumaVerifyEndpoint 验证端点：好凭据回余额；坏凭据 400 带原文；
// 空 body 拒绝。
func TestHaozhumaVerifyEndpoint(t *testing.T) {
	h, f, _ := newWebuiTestHandler(t, `{}`)

	// 好凭据（账密模式，login 成功）。
	body, _ := json.Marshal(map[string]any{"mode": "userpass", "user": "u", "pass": "p"})
	req := httptest.NewRequest(http.MethodPost, "/admin/account/sms/haozhuma/verify", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("verify good creds: status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK      bool    `json:"ok"`
		Balance float64 `json:"balance"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.OK || resp.Balance != 58.20 {
		t.Fatalf("verify resp = %+v, want ok/58.20", resp)
	}

	// 坏凭据：login 失败且无 token → 400。
	f.tokenResp.Store(`{"code":"-1","msg":"账号或密码错误"}`)
	body, _ = json.Marshal(map[string]any{"mode": "userpass", "user": "u", "pass": "wrong"})
	req = httptest.NewRequest(http.MethodPost, "/admin/account/sms/haozhuma/verify", strings.NewReader(string(body)))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("verify bad creds: want 400, got %d", rec.Code)
	}

	// 什么都没填：400。
	req = httptest.NewRequest(http.MethodPost, "/admin/account/sms/haozhuma/verify", strings.NewReader(`{}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("verify empty: want 400, got %d", rec.Code)
	}
}

// TestHaozhumaSummaryEndpoint 概览端点：余额/占用/本地账本一体返回。
func TestHaozhumaSummaryEndpoint(t *testing.T) {
	h, _, _ := newWebuiTestHandler(t, `{}`)
	req := httptest.NewRequest(http.MethodGet, "/admin/account/sms/haozhuma/summary", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var s EnrollAccountSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if s.Balance != 58.20 || s.Occupied != 2 || s.Sid != "52283" {
		t.Fatalf("summary = %+v, want 58.20/2/52283", s)
	}
}

// TestHaozhumaVerifyNoTokenLeak 验证失败的响应体不得包含 token 值
// （redactNetErr 链路防 URL 泄露）：把 Base 指到一个已关闭的端口，
// 网络错误路径必须只报主机名不报完整 URL。
func TestHaozhumaVerifyNoTokenLeak(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	u, _ := url.Parse(dead.URL)
	dead.Close() // 端口立刻下线，后续连接全部失败

	h, _, _ := newWebuiTestHandler(t, `{}`)
	// handler 组装完再把 Base 指到死端口（组装时会指到 fakeHZM）。
	oldBase := haozhumaBase
	haozhumaBase = u.String() + "/"
	t.Cleanup(func() { haozhumaBase = oldBase })

	body, _ := json.Marshal(map[string]any{"mode": "token", "token": "SECRET-TOK-XYZ"})
	req := httptest.NewRequest(http.MethodPost, "/admin/account/sms/haozhuma/verify", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Fatal("verify with unreachable server should fail")
	}
	if strings.Contains(rec.Body.String(), "SECRET-TOK-XYZ") {
		t.Fatalf("token leaked in verify error: %s", rec.Body.String())
	}
}
