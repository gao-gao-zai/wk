package reqproxy

import (
	"encoding/json"
	"testing"

	"github.com/xtls/xray-core/infra/conf"
	httpproxy "github.com/xtls/xray-core/proxy/http"
	"github.com/xtls/xray-core/proxy/socks"
)

// buildClientSettings Build 节点 outbound 并解出协议层 ClientConfig，
// 用于验证凭据没有在 JSON 形态转换中静默丢失。
func buildClientSettings(t *testing.T, n NodeSpec) (protoMsg any) {
	t.Helper()
	raw, err := buildOutboundJSON("test", n)
	if err != nil {
		t.Fatalf("buildOutboundJSON: %v", err)
	}
	var oc conf.OutboundDetourConfig
	if err := json.Unmarshal(raw, &oc); err != nil {
		t.Fatalf("unmarshal outbound: %v", err)
	}
	oh, err := oc.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if oh == nil || oh.ProxySettings == nil {
		t.Fatal("Build 产物缺 ProxySettings")
	}
	msg, err := oh.ProxySettings.GetInstance()
	if err != nil {
		t.Fatalf("ProxySettings 解包: %v", err)
	}
	return msg
}
// TestHTTPOutboundCarriesCredentials 回归：http outbound 的凭据必须到达
// 协议层 ClientConfig.Server.User。旧构造把 user/pass 平铺在 servers[] 里，
// Xray 的 HTTPClientConfig 只认 servers[].users 数组——凭据被静默丢弃，
// 出站无认证 → 407 Proxy Authentication Required → 节点全部测速失败。
func TestHTTPOutboundCarriesCredentials(t *testing.T) {
	n := NodeSpec{
		ID: "n1", Name: "n1", Protocol: "http",
		Spec: map[string]any{
			"host": "1.2.3.4", "port": 8080,
			"username": "u-ser", "password": "p-ss",
		},
	}
	msg := buildClientSettings(t, n)
	cfg, ok := msg.(*httpproxy.ClientConfig)
	if !ok {
		t.Fatalf("Build 产物类型 %T, want *http.ClientConfig", msg)
	}
	if cfg.Server == nil {
		t.Fatal("Server 为空")
	}
	u := cfg.Server.User
	if u == nil || u.Account == nil {
		t.Fatal("HTTP outbound 凭据丢失：Server.User/Account 为 nil（会导致 407）")
	}
	// Account 是 TypedMessage，GetInstance 反序列化回 http.Account 检查字段
	accAny, err := u.Account.GetInstance()
	if err != nil {
		t.Fatalf("Account 反序列化: %v", err)
	}
	acc, ok := accAny.(*httpproxy.Account)
	if !ok {
		t.Fatalf("Account 类型 %T, want *http.Account", accAny)
	}
	if acc.Username != "u-ser" || acc.Password != "p-ss" {
		t.Fatalf("凭据 = %q/%q, want u-ser/p-ss", acc.Username, acc.Password)
	}
}

// TestSocksOutboundCarriesCredentials 同上，socks outbound。
func TestSocksOutboundCarriesCredentials(t *testing.T) {
	n := NodeSpec{
		ID: "n2", Name: "n2", Protocol: "socks",
		Spec: map[string]any{
			"host": "1.2.3.4", "port": 1080,
			"username": "u-ser", "password": "p-ss",
		},
	}
	msg := buildClientSettings(t, n)
	cfg, ok := msg.(*socks.ClientConfig)
	if !ok {
		t.Fatalf("Build 产物类型 %T, want *socks.ClientConfig", msg)
	}
	if cfg.Server == nil {
		t.Fatal("Server 为空")
	}
	u := cfg.Server.User
	if u == nil || u.Account == nil {
		t.Fatal("socks outbound 凭据丢失：Server.User/Account 为 nil")
	}
	accAny, err := u.Account.GetInstance()
	if err != nil {
		t.Fatalf("Account 反序列化: %v", err)
	}
	acc, ok := accAny.(*socks.Account)
	if !ok {
		t.Fatalf("Account 类型 %T, want *socks.Account", accAny)
	}
	if acc.Username != "u-ser" || acc.Password != "p-ss" {
		t.Fatalf("凭据 = %q/%q, want u-ser/p-ss", acc.Username, acc.Password)
	}
}

// TestHTTPOutboundNoCredentials 无凭据节点不应构造 users（匿名代理）。
func TestHTTPOutboundNoCredentials(t *testing.T) {
	n := NodeSpec{
		ID: "n3", Name: "n3", Protocol: "http",
		Spec: map[string]any{"host": "1.2.3.4", "port": 8080},
	}
	msg := buildClientSettings(t, n)
	cfg, ok := msg.(*httpproxy.ClientConfig)
	if !ok {
		t.Fatalf("Build 产物类型 %T", msg)
	}
	if cfg.Server == nil {
		t.Fatal("Server 为空")
	}
	if cfg.Server.User != nil {
		t.Fatal("匿名节点不应带 User")
	}
}

// TestVmessOutboundUnaffected 现有协议（vmess）不受本次改动影响。
func TestVmessOutboundUnaffected(t *testing.T) {
	n := NodeSpec{
		ID: "n4", Name: "n4", Protocol: "vmess",
		Spec: map[string]any{
			"host": "v.example.com", "port": 443, "uuid": "uuid-x",
			"security": "auto", "tls": true,
		},
	}
	if _, _, err := outboundParts(n); err != nil {
		t.Fatalf("vmess outboundParts: %v", err)
	}
}
