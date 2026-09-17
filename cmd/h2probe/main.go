// h2probe 直连上游的对照实验：绕过 wb2api 服务，用指定的 HTTP 协议版本
// 直接请求上游 chat 接口，逐帧打印到达时间间隔。
//
// 用途：当怀疑"服务转发层把流拖慢"时，用它测量不经服务的原始上游速度。
// 若直连（h2 或 h1）帧间隔与服务转发时一致 → 慢在上游本身；
// 若直连明显更快 → 慢在服务或其连接复用方式。
//
// 需要从服务容器外拿一个账号凭证运行：
//
//	h2probe -auth /opt/wk/auths/workbuddy-xxx.json -model kimi-k3 \
//	  -proto h2 -max-tokens 512
package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"time"
)

func main() {
	var (
		authFp   = flag.String("auth", "", "workbuddy 凭证 JSON 路径")
		model    = flag.String("model", "kimi-k3", "模型")
		proto    = flag.String("proto", "h2", "h2 或 http1.1（对照实验用）")
		maxTok   = flag.Int("max-tokens", 512, "max_tokens")
		prompt   = flag.String("prompt", "请从 1 开始逐个报数，尽可能长地列下去。", "提示词")
		base     = flag.String("base", "https://copilot.tencent.com", "上游地址")
	)
	flag.Parse()
	if *authFp == "" {
		fmt.Fprintln(os.Stderr, "必须提供 -auth（workbuddy 凭证文件）")
		os.Exit(1)
	}

	raw, err := os.ReadFile(*authFp)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var doc struct {
		Auth struct {
			AccessToken string `json:"accessToken"`
			Domain      string `json:"domain"`
		} `json:"auth"`
		Account struct {
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
		} `json:"account"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		fmt.Fprintln(os.Stderr, "凭证解析失败（需要嵌套形 workbuddy JSON）:", err)
		os.Exit(1)
	}

	tr := &http.Transport{
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 16,
		ForceAttemptHTTP2:   *proto == "h2",
	}
	if *proto == "http1.1" {
		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	client := &http.Client{Transport: tr}

	body, _ := json.Marshal(map[string]any{
		"model":      *model,
		"stream":     true,
		"max_tokens": *maxTok,
		"messages":   []any{map[string]any{"role": "user", "content": *prompt}},
	})
	req, err := http.NewRequest(http.MethodPost, *base+"/v2/chat/completions", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Authorization", "Bearer "+doc.Auth.AccessToken)
	req.Header.Set("X-User-Id", doc.Account.UID)
	if doc.Account.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", doc.Account.EnterpriseID)
	}
	req.Header.Set("User-Agent", "CLI/2.63.2 CodeBuddy/2.63.2 WorkBuddy")

	fmt.Printf("proto=%s base=%s model=%s max_tokens=%d\n", *proto, *base, *model, *maxTok)
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "请求失败:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	fmt.Printf("响应头: %s (%.0fms)\n", resp.Status, time.Since(start).Seconds()*1000)
	fmt.Printf("协商协议: %s\n", resp.Proto)
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		fmt.Printf("非 200: %s\n", string(raw))
		os.Exit(1)
	}

	br := bufio.NewReaderSize(resp.Body, 64<<10)
	var gaps []float64
	var tokens int64
	var reasoning, content int
	prev := time.Time{}
	ttfb := time.Duration(0)
	for {
		line, err := br.ReadString('\n')
		if len(line) > 6 && line[:6] == "data: " {
			payload := trim(line[6:])
			if payload == "[DONE]" {
				break
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content   string `json:"content"`
						Reasoning string `json:"reasoning_content"`
					} `json:"delta"`
				} `json:"choices"`
				Usage *struct {
					CompletionTokens int64 `json:"completion_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal([]byte(payload), &chunk) == nil {
				now := time.Now()
				if ttfb == 0 && len(chunk.Choices) > 0 &&
					(chunk.Choices[0].Delta.Content != "" || chunk.Choices[0].Delta.Reasoning != "") {
					ttfb = now.Sub(start)
				}
				if len(chunk.Choices) > 0 &&
					(chunk.Choices[0].Delta.Content != "" || chunk.Choices[0].Delta.Reasoning != "") {
					if !prev.IsZero() {
						gaps = append(gaps, now.Sub(prev).Seconds()*1000)
					}
					prev = now
					reasoning += len(chunk.Choices[0].Delta.Reasoning)
					content += len(chunk.Choices[0].Delta.Content)
				}
				if chunk.Usage != nil && chunk.Usage.CompletionTokens > 0 {
					tokens = chunk.Usage.CompletionTokens
				}
			}
		}
		if err != nil {
			break
		}
	}
	total := time.Since(start)
	sort.Float64s(gaps)
	fmt.Printf("TTFB=%.0fms 总时长=%.1fs tokens=%d reasoning=%dch content=%dch\n",
		ttfb.Seconds()*1000, total.Seconds(), tokens, reasoning, content)
	if len(gaps) > 0 {
		fmt.Printf("帧间隔(ms): p50=%.1f p90=%.1f max=%.1f n=%d\n",
			pct(gaps, 50), pct(gaps, 90), gaps[len(gaps)-1], len(gaps))
		fmt.Printf("吞吐 = %.1f tok/s（生成段）\n", float64(tokens)/(total.Seconds()-ttfb.Seconds()))
	}
}

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func pct(sorted []float64, p int) float64 {
	idx := len(sorted) * p / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
