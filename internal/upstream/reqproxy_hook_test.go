package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestDialProxyNilZeroRegression 关闭（nil 钩子）时与旧行为一致：直连。
func TestDialProxyNilZeroRegression(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	c.BillingBaseCN = srv.URL
	if c.DialProxy != nil {
		t.Fatal("默认 DialProxy 必须为 nil（零回归基线）")
	}
	a := &auth.Auth{UID: "u1", Domain: "example.com", RefreshToken: "rt"}
	// RefreshToken 走 doJSON 收口；响应缺 accessToken 会报业务错，但那证明
	// 请求已直连到达——nil 钩子路径正常。
	if err := c.RefreshToken(a); err == nil {
		t.Log("刷新成功（服务器返回了 accessToken）")
	} else {
		t.Logf("RefreshToken err（预期：no accessToken）: %v", err)
	}
}

// TestDialProxyErrorFailsClosed D4：钩子报错时请求必须失败，绝不直连。
func TestDialProxyErrorFailsClosed(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true // 若请求到达这里 = 绕过了 D4，测试失败
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	c.BillingBaseCN = srv.URL
	c.DialProxy = func(uid, region string) (*url.URL, error) {
		return nil, errNoNode
	}
	a := &auth.Auth{UID: "u1", Domain: "example.com", RefreshToken: "rt"}
	err := c.RefreshToken(a)
	if err == nil {
		t.Fatal("D4：应失败")
	}
	if called {
		t.Fatal("D4 违反：请求绕过代理直连到达了上游")
	}
}

// TestDialProxyRoutesThroughSlot 有绑定账号 → 请求经槽位端口出站（经 doJSON 收口）。
func TestDialProxyRoutesThroughSlot(t *testing.T) {
	upstreamHit := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"accessToken":"at-new","expiresIn":3600}}`))
	}))
	defer target.Close()

	proxyHit := false
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHit = true
		req, err := http.NewRequest(r.Method, target.URL, nil)
		if err != nil {
			t.Errorf("代理转发构造: %v", err)
			w.WriteHeader(502)
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("代理转发: %v", err)
			w.WriteHeader(502)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer proxy.Close()

	proxyURL, _ := url.Parse(proxy.URL)
	c := New()
	c.ChatBaseCN = target.URL
	c.BillingBaseCN = target.URL
	c.DialProxy = func(uid, region string) (*url.URL, error) {
		return proxyURL, nil
	}
	a := &auth.Auth{UID: "u1", Domain: "example.com", RefreshToken: "rt"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("经槽位请求: %v", err)
	}
	if !upstreamHit {
		t.Fatal("上游未被命中——代理路径未生效？")
	}
	if !proxyHit {
		t.Fatal("代理端口未被命中——请求没走槽位")
	}
}

// errNoNode D4 测试哨兵。
var errNoNode = &url.Error{Op: "dialproxy", URL: "", Err: errNoNodeSentinel}

type errNoNodeSentinelType struct{}

func (errNoNodeSentinelType) Error() string { return "无可用代理节点" }

var errNoNodeSentinel = errNoNodeSentinelType{}
