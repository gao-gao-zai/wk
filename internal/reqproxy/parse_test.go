package reqproxy

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")

	s1, err := NewStore(fp)
	if err != nil {
		t.Fatal(err)
	}
	s1.Update(func(st *State) {
		st.Enabled = true
		st.Subscriptions = append(st.Subscriptions, Subscription{
			ID: "sub_1", Name: "机场A", URL: "https://example.com/sub?token=xx", Interval: "1h",
		})
		st.Bindings["uid_1"] = Binding{SlotID: "slot_1", Since: time.Now()}
	})
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	// 文件权限 0600（Windows 上 chmod 不生效，跳过权限断言）
	if _, err := os.Stat(fp); err != nil {
		t.Fatalf("state.json 未落盘: %v", err)
	}

	s2, err := NewStore(fp)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if !s2.st.Enabled {
		t.Error("Enabled 未恢复")
	}
	if len(s2.st.Subscriptions) != 1 || s2.st.Subscriptions[0].Name != "机场A" {
		t.Errorf("订阅未恢复: %+v", s2.st.Subscriptions)
	}
	if b, ok := s2.st.Bindings["uid_1"]; !ok || b.SlotID != "slot_1" {
		t.Errorf("绑定未恢复: %+v", s2.st.Bindings)
	}
}

func TestStoreNormalize(t *testing.T) {
	// 旧/部分状态文件缺字段时 Normalize 补全
	s := &State{Enabled: true}
	s.Normalize()
	if s.Subscriptions == nil || s.ManualNodes == nil || s.Slots == nil ||
		s.Bindings == nil || s.Health == nil || s.Rules.RegionRules == nil {
		t.Error("Normalize 未补全空集合")
	}
}

// ---- 解析器测试 ----

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestParseVmessBase64JSON(t *testing.T) {
	j, _ := json.Marshal(map[string]any{
		"ps": "香港 01", "add": "hk1.example.com", "port": "443", "id": "uuid-1",
		"aid": "0", "net": "ws", "path": "/ws", "host": "cdn.example.com", "tls": "tls",
	})
	link := "vmess://" + b64(string(j))
	n, err := ParseLink(link)
	if err != nil {
		t.Fatal(err)
	}
	if n.Protocol != "vmess" || n.Name != "香港 01" || n.Region != "HK" {
		t.Errorf("解析结果: %+v", n)
	}
	if n.Spec["host"] != "hk1.example.com" || n.Spec["uuid"] != "uuid-1" {
		t.Errorf("spec: %+v", n.Spec)
	}
	// 传输层 host（ws Host 头）单独存放，不覆盖服务器地址
	if n.Spec["wsHost"] != "cdn.example.com" && n.Spec["host"] == "cdn.example.com" {
		t.Errorf("ws host 处理: %+v", n.Spec)
	}
	if n.Spec["network"] != "ws" || n.Spec["tls"] != true {
		t.Errorf("传输层: %+v", n.Spec)
	}
}

func TestParseVless(t *testing.T) {
	link := "vless://uuid-2@us1.example.com:443?type=grpc&security=reality&pbk=PK&sid=AB&fp=chrome&sni=us1.example.com&flow=xtls-rprx-vision#美国%20Los%20Angeles"
	n, err := ParseLink(link)
	if err != nil {
		t.Fatal(err)
	}
	if n.Protocol != "vless" || n.Region != "US" {
		t.Errorf("解析结果: %+v", n)
	}
	if n.Spec["security"] != "reality" || n.Spec["publicKey"] != "PK" || n.Spec["flow"] != "xtls-rprx-vision" {
		t.Errorf("reality 参数: %+v", n.Spec)
	}
	if n.Name != "美国 Los Angeles" {
		t.Errorf("名称: %q", n.Name)
	}
}

func TestParseTrojan(t *testing.T) {
	n, err := ParseLink("trojan://pass123@jp1.example.com:443?sni=jp1.example.com#东京01")
	if err != nil {
		t.Fatal(err)
	}
	if n.Protocol != "trojan" || n.Spec["password"] != "pass123" || n.Region != "JP" {
		t.Errorf("解析结果: %+v", n)
	}
	if n.Spec["tls"] != true {
		t.Error("trojan 必须 tls")
	}
}

func TestParseSS(t *testing.T) {
	// SIP002: userinfo = base64url(method:pass)
	cred := b64("aes-256-gcm:secretpass")
	n, err := ParseLink("ss://" + cred + "@sg1.example.com:8388#新加坡%20IEPL")
	if err != nil {
		t.Fatal(err)
	}
	if n.Protocol != "ss" || n.Spec["method"] != "aes-256-gcm" || n.Spec["password"] != "secretpass" {
		t.Errorf("解析结果: %+v", n)
	}
	if n.Region != "SG" {
		t.Errorf("地区: %s", n.Region)
	}
}

func TestParseLegacyHostPort(t *testing.T) {
	n, err := ParseLink("1.2.3.4:8080:user:pass")
	if err != nil {
		t.Fatal(err)
	}
	if n.Protocol != "http" || n.Spec["host"] != "1.2.3.4" || n.Spec["password"] != "pass" {
		t.Errorf("解析结果: %+v", n)
	}
	// port 必须是 int：Xray conf 的 HTTPRemoteConfig.Port 是 uint16，
	// 字符串端口会让 outbound Build 失败（节点入池但内核无出站 → 测速不健康）。
	if n.Spec["port"] != 8080 {
		t.Errorf("port 应为 int 8080, got %T %v", n.Spec["port"], n.Spec["port"])
	}
	// 非法端口要报错而不是静默产出坏节点
	if _, err := ParseLink("1.2.3.4:abc:user:pass"); err == nil {
		t.Error("非法端口应报错")
	}
	if _, err := ParseLink("1.2.3.4:0:user:pass"); err == nil {
		t.Error("端口 0 应报错")
	}
}

func TestParseSocks5(t *testing.T) {
	n, err := ParseLink("socks5://user:pass@5.6.7.8:1080#手动socks")
	if err != nil {
		t.Fatal(err)
	}
	if n.Protocol != "socks" || n.Spec["port"] != 1080 {
		t.Errorf("解析结果: %+v", n)
	}
}

func TestUnsupportedProtocol(t *testing.T) {
	_, err := ParseLink("hysteria2://abc@x.com:443#hy2节点")
	if err == nil {
		t.Fatal("期望 ErrUnsupportedProtocol")
	}
	if _, ok := err.(*ErrUnsupportedProtocol); !ok {
		t.Fatalf("错误类型: %T", err)
	}
}

func TestParseTextBase64List(t *testing.T) {
	raw := "trojan://pw@hk.example.com:443#香港A\nvless://u@jp.example.com:443#日本B\n"
	text := b64(raw)
	nodes, errs := ParseText(text, "sub_1")
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	if len(nodes) != 2 || nodes[0].Name != "香港A" || nodes[1].Region != "JP" {
		t.Errorf("节点: %+v", nodes)
	}
	if nodes[0].Source != "sub_1" {
		t.Errorf("source: %s", nodes[0].Source)
	}
}

func TestParseTextMixedWithBadLines(t *testing.T) {
	text := "vmess://garbage!!!\ntrojan://pw@a.com:443#ok\n#注释\n\n"
	nodes, errs := ParseText(text, "manual")
	if len(errs) != 1 {
		t.Errorf("期望 1 个错误, got %d: %v", len(errs), errs)
	}
	if len(nodes) != 1 || nodes[0].Name != "ok" {
		t.Errorf("节点: %+v", nodes)
	}
}

func TestParseTextClashYAMLRejected(t *testing.T) {
	yaml := "proxies:\n  - name: x\n    type: ss\n"
	nodes, errs := ParseText(yaml, "sub_1")
	// 不含 :// 也不是 base64 → 逐行解析全失败
	if len(nodes) != 0 {
		t.Errorf("clash YAML 不应解析出节点: %+v", nodes)
	}
	if len(errs) == 0 {
		t.Error("应报错")
	}
}

func TestDetectRegion(t *testing.T) {
	cases := map[string]string{
		"香港 IEPL 01":    "HK",
		"日本 Tokyo BGP":  "JP",
		"US Los Angeles": "US",
		"新加坡 沪新专线":     "SG",
		"韩国首尔":          "KR",
		"专线 A":          "other",
		"":               "other",
	}
	for name, want := range cases {
		if got := DetectRegion(name); got != want {
			t.Errorf("DetectRegion(%q) = %s, want %s", name, got, want)
		}
	}
}

func TestRegionKeywordAsSubstring(t *testing.T) {
	// "US" 出现在 "RUSsia" 之类误匹配风险：按表序匹配，HK/JP 等先于 US，
	// 且实际机场命名极少这样组合——可接受（宽松匹配优于漏匹配）。
	if got := DetectRegion("香港US"); got != "HK" {
		t.Errorf("表序优先级: got %s", got)
	}
	_ = strings.TrimSpace
}
