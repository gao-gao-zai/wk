// 节点解析：share-link（vmess/vless/trojan/ss/wireguard）+ socks5/http + Base64 列表。
//
// 解析产物是 NodeSpec——已归一化的节点描述，kernel 层据此生成 Xray outbound。
// 地区（Region tag）按节点名识别（机场命名惯例），手动节点可用 RegionTag 覆盖。
package reqproxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// NodeSpec 解析后的节点规格（kernel 无关）。
type NodeSpec struct {
	ID       string            `json:"id"`       // 稳定 ID（来源前缀 + 名称哈希/导入ID）
	Name     string            `json:"name"`
	Protocol string            `json:"protocol"` // vmess|vless|trojan|ss|wireguard|socks|http
	Source   string            `json:"source"`  // 订阅ID 或 "manual"
	Region   string            `json:"region"`   // HK|JP|SG|US|...|other（按名称识别）
	Spec     map[string]any    `json:"spec"`    // 协议参数（kernel 层消费）
	Raw      string            `json:"-"`        // 原始链接（定义变更检测用）
}

// ParseText 解析多行文本：每行一个 share-link 或 host:port:user:pass，或整体 Base64。
// 返回成功解析的节点和逐行错误（错误行跳过，不中断）。
// 协议不支持的行静默跳过（不进 errs）——调用方（订阅层）统计跳过数自行告警。
func ParseText(text, source string) ([]NodeSpec, []error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	// 整体 Base64 节点列表（v2rayN 导出）？
	if !strings.Contains(text, "://") {
		if decoded, ok := tryBase64Decode(text); ok && strings.Contains(decoded, "://") {
			text = decoded
		}
	}
	var nodes []NodeSpec
	var errs []error
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		spec, err := ParseLink(line)
		if err != nil {
			if _, unsupported := err.(*ErrUnsupportedProtocol); unsupported {
				continue // 静默跳过，调用方统计
			}
			errs = append(errs, fmt.Errorf("第 %d 行: %w", i+1, err))
			continue
		}
		if spec != nil {
			spec.Source = source
			nodes = append(nodes, *spec)
		}
	}
	return nodes, errs
}

// ErrUnsupportedProtocol 表示该链接协议不受支持（调用方据此统计「协议不支持跳过」数）。
type ErrUnsupportedProtocol struct{ Proto string }

func (e *ErrUnsupportedProtocol) Error() string {
	return fmt.Sprintf("协议不支持: %s", e.Proto) }

// ParseLink 解析单条链接。返回 (nil, nil) 表示空行/注释。
func ParseLink(line string) (*NodeSpec, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, nil
	}
	// host:port:user:pass（兼容旧格式）
	if !strings.Contains(line, "://") {
		parts := strings.Split(line, ":")
		if len(parts) != 4 {
			return nil, fmt.Errorf("无法解析 %q，期望 share-link 或 host:port:user:pass", line)
		}
		port := toInt(parts[1], 0)
		if port <= 0 || port > 65535 {
			return nil, fmt.Errorf("无法解析 %q，端口 %q 不是 1-65535 的数字", line, parts[1])
		}
		return &NodeSpec{
			Name:     parts[0],
			Protocol: "http",
			Region:   DetectRegion(parts[0]),
			Spec: map[string]any{
				"host": parts[0], "port": port,
				"username": parts[2], "password": parts[3],
			},
			Raw: line,
		}, nil
	}
	u, err := url.Parse(line)
	if err != nil {
		return nil, fmt.Errorf("链接不合法: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "vmess":
		return parseVmess(line)
	case "vless":
		return parseVless(u)
	case "trojan":
		return parseTrojan(u)
	case "ss":
		return parseSS(u)
	case "socks", "socks5":
		return parseHTTPLike(u, "socks")
	case "http", "https":
		return parseHTTPLike(u, "http")
	case "wireguard":
		return parseWireguard(u)
	case "hysteria2", "hy2", "tuic", "ssr", "snell":
		return nil, &ErrUnsupportedProtocol{Proto: u.Scheme}
	default:
		return nil, &ErrUnsupportedProtocol{Proto: u.Scheme}
	}
}

// parseVmess vmess://Base64(JSON) 或 vmess://uuid@host:port?params#name
func parseVmess(raw string) (*NodeSpec, error) {
	payload := strings.TrimPrefix(strings.TrimSpace(raw), "vmess://")
	// Base64 JSON 形态
	if decoded, ok := tryBase64Decode(payload); ok && strings.HasPrefix(strings.TrimSpace(decoded), "{") {
		var v struct {
			V    string `json:"v"`
			Ps   string `json:"ps"`
			Add  string `json:"add"`
			Port any    `json:"port"`
			ID   string `json:"id"`
			Aid  any    `json:"aid"`
			Net  string `json:"net"`
		Type string `json:"type"`
		Host string `json:"host"`
		Path string `json:"path"`
		TLS  string `json:"tls"`
		Sni  string `json:"sni"`
		Alpn string `json:"alpn"`
		Fp   string `json:"fp"`
		}
		if err := json.Unmarshal([]byte(decoded), &v); err != nil {
			return nil, fmt.Errorf("vmess Base64 JSON 不合法: %w", err)
		}
		if v.Add == "" || v.ID == "" {
			return nil, fmt.Errorf("vmess JSON 缺少 add/id")
		}
		spec := map[string]any{
			"host": v.Add, "port": toInt(v.Port, 443),
			"uuid": v.ID, "alterId": toInt(v.Aid, 0),
			"security": "auto",
		}
		applyTransport(spec, v.Net, v.Type, v.Host, v.Path, v.TLS, v.Sni, v.Alpn, v.Fp)
		name := v.Ps
		if name == "" {
			name = v.Add
		}
		return &NodeSpec{Name: name, Protocol: "vmess", Region: DetectRegion(name), Spec: spec, Raw: raw}, nil
	}
	// v2rayN 参数形态 vmess://uuid@host:port?...
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("vmess 链接不合法: %w", err)
	}
	uuid := u.User.Username()
	host := u.Hostname()
	port := u.Port()
	if uuid == "" || host == "" {
		return nil, fmt.Errorf("vmess 链接缺少 uuid/host")
	}
	q := u.Query()
	spec := map[string]any{
		"host": host, "port": toInt(port, 443), "uuid": uuid,
		"alterId": toInt(q.Get("alterId"), 0), "security": "auto",
	}
	network := q.Get("net")
	if network == "" {
		network = q.Get("network")
	}
	applyTransport(spec, network, q.Get("type"), q.Get("host"), q.Get("path"),
		q.Get("security"), q.Get("sni"), q.Get("alpn"), q.Get("fp"))
	name := fragment(u)
	return &NodeSpec{Name: name, Protocol: "vmess", Region: DetectRegion(name), Spec: spec, Raw: raw}, nil
}

// applyTransport 把传输层参数并入 spec（ws/grpc/tcp+h2 等）。
func applyTransport(spec map[string]any, network, headerType, host, path, tls, sni, alpn, fp string) {
	network = strings.ToLower(strings.TrimSpace(network))
	if network == "" {
		network = "tcp"
	}
	spec["network"] = network
	if headerType != "" && headerType != "none" {
		spec["headerType"] = headerType
	}
	if host != "" {
		// 传输层 Host（ws/grpc 的 Host 头），与服务器地址分开存放
		spec["wsHost"] = host
	}
	if path != "" {
		spec["path"] = path
	}
	tlsOn := strings.EqualFold(tls, "tls") || strings.EqualFold(tls, "reality")
	spec["tls"] = tlsOn
	if tlsOn {
		if sni != "" {
			spec["serverName"] = sni
		}
		if alpn != "" {
			spec["alpn"] = strings.Split(alpn, ",")
		}
		if fp != "" {
			spec["fingerprint"] = fp
		}
	}
}

func parseVless(u *url.URL) (*NodeSpec, error) {
	uuid := u.User.Username()
	host := u.Hostname()
	port := u.Port()
	if uuid == "" || host == "" {
		return nil, fmt.Errorf("vless 链接缺少 uuid/host")
	}
	q := u.Query()
	spec := map[string]any{
		"host": host, "port": toInt(port, 443), "uuid": uuid,
	}
	network := q.Get("type")
	if network == "" {
		network = "tcp"
	}
	spec["network"] = network
	if h := q.Get("host"); h != "" {
		spec["host"] = h
	}
	if p := q.Get("path"); p != "" {
		spec["path"] = p
	}
	// TLS / Reality
	security := q.Get("security")
	if security == "tls" || security == "reality" {
		spec["tls"] = true
		spec["security"] = security
		if sni := q.Get("sni"); sni != "" {
			spec["serverName"] = sni
		}
		if alpn := q.Get("alpn"); alpn != "" {
			spec["alpn"] = strings.Split(alpn, ",")
		}
		if fp := q.Get("fp"); fp != "" {
			spec["fingerprint"] = fp
		}
		if security == "reality" {
			spec["publicKey"] = q.Get("pbk")
			spec["shortId"] = q.Get("sid")
		}
	}
	if flow := q.Get("flow"); flow != "" {
		spec["flow"] = flow
	}
	if svc := q.Get("serviceName"); svc != "" {
		spec["serviceName"] = svc
	}
	name := fragment(u)
	return &NodeSpec{Name: name, Protocol: "vless", Region: DetectRegion(name), Spec: spec, Raw: u.String()}, nil
}

func parseTrojan(u *url.URL) (*NodeSpec, error) {
	password := u.User.Username()
	host := u.Hostname()
	port := u.Port()
	if password == "" || host == "" {
		return nil, fmt.Errorf("trojan 链接缺少 password/host")
	}
	q := u.Query()
	spec := map[string]any{
		"host": host, "port": toInt(port, 443), "password": password,
		"tls": true,
	}
	if sni := q.Get("sni"); sni != "" {
		spec["serverName"] = sni
	}
	if alpn := q.Get("alpn"); alpn != "" {
		spec["alpn"] = strings.Split(alpn, ",")
	}
	// 传输层（ws/grpc）
	network := q.Get("type")
	if network == "" {
		network = "tcp"
	}
	spec["network"] = network
	if p := q.Get("path"); p != "" {
		spec["path"] = p
	}
	if h := q.Get("host"); h != "" {
		spec["hostHeader"] = h
	}
	if svc := q.Get("serviceName"); svc != "" {
		spec["serviceName"] = svc
	}
	name := fragment(u)
	return &NodeSpec{Name: name, Protocol: "trojan", Region: DetectRegion(name), Spec: spec, Raw: u.String()}, nil
}

// parseSS ss://Base64(method:pass@host:port)#name 或 ss://method:pass@host:port（RFC 3986 userinfo）
func parseSS(u *url.URL) (*NodeSpec, error) {
	// 带 plugin 的 SIP002 形态暂不支持（少见）
	if u.Query().Get("plugin") != "" {
		return nil, &ErrUnsupportedProtocol{Proto: "ss+plugin"}
	}
	host := u.Hostname()
	port := u.Port()
	if host == "" || port == "" {
		return nil, fmt.Errorf("ss 链接缺少 host/port")
	}
	method, password := "", ""
	if u.User != nil {
		// SIP002: userinfo = Base64Url(method:password)
		raw := u.User.String()
		if dec, ok := tryBase64Decode(raw); ok && strings.Contains(dec, ":") {
			parts := strings.SplitN(dec, ":", 2)
			method, password = parts[0], parts[1]
		} else if strings.Contains(raw, ":") {
			parts := strings.SplitN(raw, ":", 2)
			method, password = parts[0], parts[1]
		}
	}
	if method == "" {
		// 旧形态: ss://Base64(method:pass@host:port)#name
		payload := strings.TrimPrefix(u.String(), "ss://")
		if idx := strings.Index(payload, "#"); idx >= 0 {
			payload = payload[:idx]
		}
		if dec, ok := tryBase64Decode(payload); ok {
			// method:pass@host:port
			at := strings.LastIndex(dec, "@")
			if at > 0 {
				cred := dec[:at]
				server := dec[at+1:]
				if c := strings.Index(cred, ":"); c > 0 {
					method, password = cred[:c], cred[c+1:]
				}
				if sp := strings.LastIndex(server, ":"); sp > 0 {
					host, port = server[:sp], server[sp+1:]
				}
			}
		}
	}
	if method == "" || password == "" {
		return nil, fmt.Errorf("ss 链接缺少 method/password")
	}
	spec := map[string]any{
		"host": host, "port": toInt(port, 443),
		"method": method, "password": password,
	}
	name := fragment(u)
	return &NodeSpec{Name: name, Protocol: "ss", Region: DetectRegion(name), Spec: spec, Raw: u.String()}, nil
}

func parseHTTPLike(u *url.URL, proto string) (*NodeSpec, error) {
	host := u.Hostname()
	port := u.Port()
	if host == "" {
		return nil, fmt.Errorf("%s 链接缺少 host", proto)
	}
	spec := map[string]any{"host": host, "port": toInt(port, defaultPort(u.Scheme))}
	if u.User != nil {
		spec["username"] = u.User.Username()
		if p, ok := u.User.Password(); ok {
			spec["password"] = p
		}
	}
	name := fragment(u)
	return &NodeSpec{Name: name, Protocol: proto, Region: DetectRegion(name), Spec: spec, Raw: u.String()}, nil
}

// parseWireguard wireguard://privatekey@host:port?peers=...（少见，宽松处理：记录 host/port/key）
func parseWireguard(u *url.URL) (*NodeSpec, error) {
	host := u.Hostname()
	port := u.Port()
	if host == "" {
		return nil, fmt.Errorf("wireguard 链接缺少 host")
	}
	spec := map[string]any{"host": host, "port": toInt(port, 51820)}
	if u.User != nil {
		spec["privateKey"] = u.User.Username()
	}
	if pk := u.Query().Get("publickey"); pk != "" {
		spec["peerPublicKey"] = pk
	}
	if psk := u.Query().Get("presharedkey"); psk != "" {
		spec["preSharedKey"] = psk
	}
	if a := u.Query().Get("address"); a != "" {
		spec["localAddress"] = strings.Split(a, ",")
	}
	name := fragment(u)
	return &NodeSpec{Name: name, Protocol: "wireguard", Region: DetectRegion(name), Spec: spec, Raw: u.String()}, nil
}

// ---- 工具函数 ----

// defaultPort 按协议给默认端口。
func defaultPort(scheme string) int {
	switch strings.ToLower(scheme) {
	case "https":
		return 443
	case "socks", "socks5":
		return 1080
	default:
		return 8080
	}
}

// fragment 取 # 后的节点名（URL decode），无则用 host。
func fragment(u *url.URL) string {
	if u.Fragment != "" {
		dec, err := url.QueryUnescape(u.Fragment)
		if err == nil {
			return dec
		}
		return u.Fragment
	}
	return u.Hostname()
}

// tryBase64Decode 尝试 Std/URLSafe（带/不带 padding）解码，失败返回 ok=false。
func tryBase64Decode(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	trimmed := strings.TrimRight(s, "=")
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return string(b), true
		}
		// 去 padding 重试
		if b, err := enc.DecodeString(trimmed + strings.Repeat("=", (4-len(trimmed)%4)%4)); err == nil {
			return string(b), true
		}
	}
	return "", false
}

// toInt 宽松转 int（vmess JSON 的 port 可能是字符串）。
func toInt(v any, def int) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		var out int
		if _, err := fmt.Sscanf(n, "%d", &out); err == nil && out > 0 {
			return out
		}
	}
	return def
}

// regionPatterns 地区识别的匹配表（机场命名惯例：中英简称）。
var regionPatterns = []struct {
	Region  string
	Pattern []string
}{
	{"HK", []string{"香港", "HK", "Hong Kong", "HongKong", "Hongkong"}},
	{"TW", []string{"台湾", "TW", "Taiwan", "新北", "台北"}},
	{"JP", []string{"日本", "JP", "Japan", "东京", "大阪", "Tokyo", "Osaka"}},
	{"KR", []string{"韩国", "KR", "Korea", "首尔", "Seoul"}},
	{"SG", []string{"新加坡", "SG", "Singapore", "狮城", "沪新"}},
	{"MY", []string{"马来西亚", "MY", "Malaysia", "大马"}},
	{"TH", []string{"泰国", "TH", "Thailand", "曼谷"}},
	{"VN", []string{"越南", "VN", "Vietnam"}},
	{"PH", []string{"菲律宾", "PH", "Philippines"}},
	{"US", []string{"美国", "US", "United States", "Los Angeles", "San Jose", "Seattle", "纽约", "洛杉矶", "圣何塞", "西雅图"}},
	{"CA", []string{"加拿大", "CA", "Canada"}},
	{"GB", []string{"英国", "UK", "GB", "United Kingdom", "London", "伦敦"}},
	{"DE", []string{"德国", "DE", "Germany", "Frankfurt", "法兰克福"}},
	{"FR", []string{"法国", "FR", "France", "Paris", "巴黎"}},
	{"NL", []string{"荷兰", "NL", "Netherlands", "Amsterdam", "阿姆斯特丹"}},
	{"RU", []string{"俄罗斯", "RU", "Russia", "Moscow", "莫斯科"}},
	{"TR", []string{"土耳其", "TR", "Turkey", "Istanbul"}},
	{"AU", []string{"澳大利亚", "澳洲", "AU", "Australia", "Sydney", "悉尼"}},
	{"IN", []string{"印度", "IN", "India"}},
	{"BR", []string{"巴西", "BR", "Brazil", "圣保罗"}},
	{"AR", []string{"阿根廷", "AR", "Argentina"}},
}

// DetectRegion 按节点名识别地区；识别不出返回 "other"。
// 匹配规则：子串命中（词边界不强制——机场命名不规范，宽松匹配优于漏匹配）。
func DetectRegion(name string) string {
	if name == "" {
		return "other"
	}
	for _, p := range regionPatterns {
		for _, kw := range p.Pattern {
			if strings.Contains(name, kw) {
				return p.Region
			}
		}
	}
	return "other"
}
