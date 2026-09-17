// streambench 流式速度基准工具：以不同并发度打 /v1/chat/completions，
// 逐流测量 TTFB 与 token 生成速率，用于定位"高并发下 token 变慢"的层级。
//
// 用法示例：
//
//	go run ./cmd/streambench -base http://127.0.0.1:7863 -key sk-xxx \
//	  -model kimi-k3 -c 1 -per 1              # 单流基线
//	go run ./cmd/streambench -base http://127.0.0.1:7863 -key sk-xxx \
//	  -model kimi-k3 -c 3 -per 1 -conversation bench-1   # 同账号 3 并发（粘性钉住）
//	go run ./cmd/streambench -base http://127.0.0.1:7863 -key sk-xxx \
//	  -model kimi-k3 -c 3 -per 1 -conversation-split      # 每流独立会话键（分散）
//
// 判读（对照 README/排查文档）：
//   - TTFB 随并发上升、tok/s 不变 → 瓶颈在请求建立段（选号锁 / refresh 风暴）
//   - tok/s 随并发下降（单流）但总吞吐不变 → 上游总量限速（连接/账号池整体）
//   - 同账号并发 tok/s 掉、分散账号不掉 → 上游按账号并发生成降速（max_in_flight 应调小）
//   - 全部模式都掉且总吞吐也掉 → 上游按 IP / HTTP2 单连接限速
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

type result struct {
	worker  int
	ttfb    time.Duration
	dur     time.Duration
	tokens  int64
	chunks  int64
	err     string
	status  int
	uidHint string
}

func main() {
	var (
		base     = flag.String("base", "http://127.0.0.1:7863", "服务地址")
		key      = flag.String("key", os.Getenv("WB2A_API_KEY"), "API key（缺省读 WB2A_API_KEY）")
		model    = flag.String("model", "kimi-k3", "模型名")
		c        = flag.Int("c", 1, "并发流数")
		per      = flag.Int("per", 1, "每个 worker 连续发送的请求数")
		maxTok   = flag.Int("max-tokens", 512, "每个请求的 max_tokens")
		conv     = flag.String("conversation", "", "会话粘性键（metadata.conversation_id）；留空=不粘")
		convEach = flag.Bool("conversation-each", false, "每个 worker 用独立会话键（<conversation>-<i>）")
		prompt   = flag.String("prompt", "请从 1 开始逐个报数，尽可能长地列下去，不要解释。", "生成提示词")
	)
	flag.Parse()
	if *key == "" {
		fmt.Fprintln(os.Stderr, "必须提供 -key 或设置 WB2A_API_KEY")
		os.Exit(1)
	}

	fmt.Printf("base=%s model=%s 并发=%d 每流请求数=%d max_tokens=%d 粘性=%q\n\n",
		*base, *model, *c, *per, *maxTok, *conv)

	client := &http.Client{} // 流式不设整体超时
	results := make([]result, 0, *c**per)
	var mu sync.Mutex
	var wg sync.WaitGroup

	startAll := time.Now()
	for w := 0; w < *c; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < *per; i++ {
				r := oneRequest(client, *base, *key, *model, *prompt, *maxTok, conversationFor(*conv, *convEach, w))
				r.worker = w
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
				if r.err != "" {
					fmt.Printf("[w%02d] ERROR status=%d %s\n", w, r.status, r.err)
				}
			}
		}(w)
	}
	wg.Wait()
	wall := time.Since(startAll)

	printReport(results, wall, *c)
}

func conversationFor(conv string, each bool, worker int) string {
	if conv == "" {
		return ""
	}
	if each {
		return fmt.Sprintf("%s-w%02d", conv, worker)
	}
	return conv
}

func oneRequest(client *http.Client, base, key, model, prompt string, maxTok int, conversation string) result {
	body := map[string]any{
		"model":      model,
		"stream":     true,
		"max_tokens": maxTok,
		"messages":   []any{map[string]any{"role": "user", "content": prompt}},
	}
	if conversation != "" {
		body["metadata"] = map[string]any{"conversation_id": conversation}
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return result{err: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return result{err: err.Error()}
	}
	defer resp.Body.Close()
	r := result{status: resp.StatusCode}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		r.err = string(raw)
		return r
	}

	br := bufio.NewReaderSize(resp.Body, 64<<10)
	var ttfb time.Duration
	var lastData time.Time
	var tokens, chunks int64
	done := false
	for !done {
		line, err := br.ReadString('\n')
		if line != "" {
			trimmed := trimEOL(line)
			if len(trimmed) > 6 && trimmed[:6] == "data: " {
				payload := trimmed[6:]
				if payload == "[DONE]" {
					lastData = time.Now()
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
						OutputTokens     int64 `json:"output_tokens"`
					} `json:"usage"`
				}
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					// 首 token = 首个 content 或首个 reasoning_content：
					// 推理模型的 reasoning 段也是逐 token 流出的，用户感知的
					// "首字慢" 包含它。只认 content 会把整个 reasoning 段
					// 错算进 TTFB。
					hasReasoning := len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Reasoning != ""
					if ttfb == 0 && (len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" || hasReasoning || chunk.Usage != nil) {
						ttfb = time.Since(start)
					}
					if len(chunk.Choices) > 0 && (chunk.Choices[0].Delta.Content != "" || hasReasoning) {
						chunks++
					}
					if chunk.Usage != nil {
						if chunk.Usage.CompletionTokens > 0 {
							tokens = chunk.Usage.CompletionTokens
						}
						if chunk.Usage.OutputTokens > 0 && tokens == 0 {
							tokens = chunk.Usage.OutputTokens
						}
					}
					lastData = time.Now()
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				lastData = time.Now()
			} else {
				r.err = err.Error()
			}
			break
		}
	}
	r.ttfb = ttfb
	r.dur = lastData.Sub(start)
	r.tokens = tokens
	r.chunks = chunks
	if r.tokens == 0 && r.chunks > 0 {
		// 上游没给 usage：用 chunk 数近似（1 chunk ≈ 1 token 量级，标注近似值）。
		r.tokens = r.chunks
		r.uidHint = "~chunks"
	}
	return r
}

func trimEOL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func printReport(results []result, wall time.Duration, concurrency int) {
	ok := 0
	var tokPerSec []float64
	var ttfbs []float64
	var totalTokens int64
	for _, r := range results {
		if r.err != "" || r.ttfb <= 0 || r.dur <= 0 {
			continue
		}
		ok++
		gen := r.dur - r.ttfb // 生成段 = 首 token 之后到流结束
		var rate float64
		// 生成段极短（上游一次性倾泻）时，rate 会爆成天文数字：把生成段
		// 下限钳到 1ms，避免 512tok/2ms 这种失真的“速率”。
		if gen < time.Millisecond {
			gen = time.Millisecond
		}
		rate = float64(r.tokens) / gen.Seconds()
		tokPerSec = append(tokPerSec, rate)
		ttfbs = append(ttfbs, r.ttfb.Seconds()*1000)
		totalTokens += r.tokens
	}
	fmt.Printf("完成 %d/%d 个流（墙钟 %s）\n\n", ok, len(results), wall.Round(time.Millisecond))
	if ok == 0 {
		return
	}
	sort.Float64s(tokPerSec)
	sort.Float64s(ttfbs)

	fmt.Printf("  每流 TTFB   : p50=%.0fms  min=%.0fms  max=%.0fms\n",
		pct(ttfbs, 50), ttfbs[0], ttfbs[len(ttfbs)-1])
	fmt.Printf("  每流 tok/s  : p50=%.1f  min=%.1f  max=%.1f%s\n",
		pct(tokPerSec, 50), tokPerSec[0], tokPerSec[len(tokPerSec)-1],
		approxNote(results))
	fmt.Printf("  总吞吐      : %.1f tok/s（%d tokens / %s）\n\n",
		float64(totalTokens)/wall.Seconds(), totalTokens, wall.Round(time.Millisecond))

	fmt.Println("判读：")
	fmt.Println("  * TTFB 涨、tok/s 不变      → 瓶颈在建立段（选号/token刷新），生成段健康")
	fmt.Println("  * 同账号并发 tok/s 掉       → 上游按账号并发生成降速 → 调小 max_in_flight")
	fmt.Println("  * 分散账号也掉、总量也掉    → 上游按 IP/连接限速 → 查 HTTP2 单连接复用")
	fmt.Println("  * 压测期间另开终端跑 watch-status.sh 观察 healthy/cooling/in_flight")
}

func approxNote(results []result) string {
	for _, r := range results {
		if r.uidHint == "~chunks" {
			return "（部分流无 usage，用 chunk 数近似）"
		}
	}
	return ""
}

func pct(sorted []float64, p int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := len(sorted) * p / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
