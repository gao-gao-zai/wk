// 订阅拉取：v2rayN 风格 URL → 节点集。
//
// 关键约定（设计文档 3.3）：
//   - 强制 User-Agent: v2rayN/6.x——机场按 UA 决定返回 Base64 还是 clash YAML。
//   - 返回 clash YAML（proxies: 特征）→ 明确报错，不做半吊子解析。
//   - 解析 subscription-userinfo 响应头（upload=..; download=..; total=..; expire=..）。
package reqproxy

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrClashYAML 订阅源返回了 clash YAML（不支持）。
var ErrClashYAML = fmt.Errorf("该订阅源返回 clash YAML 格式（不支持）；请使用提供 Base64/share-link 的订阅地址")

// SubscriptionResult 一次订阅拉取的结果。
type SubscriptionResult struct {
	Nodes    []NodeSpec
	Userinfo *Userinfo
	Warnings []string // 协议不支持等非致命告警
}

var subHTTPClient = &http.Client{Timeout: 30 * time.Second}

// FetchSubscription 拉取并解析一个订阅 URL。
func FetchSubscription(rawURL string) (*SubscriptionResult, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("订阅地址不合法: %w", err)
	}
	// 关键：v2rayN UA，引导机场返回 share-link 列表而非 clash YAML
	req.Header.Set("User-Agent", "v2rayN/6.60")
	resp, err := subHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取订阅: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4MB 上限
	if err != nil {
		return nil, fmt.Errorf("读取订阅内容: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("订阅返回 HTTP %d", resp.StatusCode)
	}

	result := &SubscriptionResult{Userinfo: parseUserinfoHeader(resp.Header.Get("subscription-userinfo"))}

	text := strings.TrimSpace(string(body))
	if text == "" {
		return nil, fmt.Errorf("订阅内容为空")
	}
	// clash YAML 特征检测（设计文档：明确报错，不解析）
	if strings.HasPrefix(text, "proxies:") || strings.Contains(text, "\nproxies:") {
		return nil, ErrClashYAML
	}
	nodes, errs := ParseText(text, "")
	for _, e := range errs {
		return nil, fmt.Errorf("订阅内容解析失败: %w", e)
	}
	// 统计跳过的协议不支持节点（ParseText 静默跳过，这里按行数差值推算）
	parsedCount := len(nodes)
	totalLines := countNodeLines(text)
	if skipped := totalLines - parsedCount; skipped > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf("%d 个节点因协议不支持被跳过", skipped))
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("订阅内容未解析出任何节点")
	}
	result.Nodes = nodes
	return result, nil
}

// countNodeLines 统计文本里的有效节点行数（空行/注释除外）。
func countNodeLines(text string) int {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		n++
	}
	return n
}

// parseUserinfoHeader 解析 subscription-userinfo 头：
//
//	upload=1234; download=5678; total=107374182400; expire=1735689600
func parseUserinfoHeader(h string) *Userinfo {
	if strings.TrimSpace(h) == "" {
		return nil
	}
	u := &Userinfo{}
	for _, kv := range strings.Split(h, ";") {
		kv = strings.TrimSpace(kv)
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(parts[0])) {
		case "upload":
			u.UploadBytes = v
		case "download":
			u.DownloadBytes = v
		case "total":
			u.TotalBytes = v
		case "expire":
			t := time.Unix(v, 0)
			u.ExpireAt = &t
		}
	}
	if u.TotalBytes == 0 && u.ExpireAt == nil {
		return nil
	}
	return u
}
