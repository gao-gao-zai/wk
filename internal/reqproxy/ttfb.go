package reqproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TTFBResult 一个节点的真实首字测速结果。
//
// 与 Health 的连通性测速不同：这里经该节点发一次最小的真实对话请求，
// 量的是「响应头 + 第一帧内容」到达的时间，和线上请求的首字是同一段链路。
type TTFBResult struct {
	NodeID    string `json:"node_id"`
	Name      string `json:"name"`
	UID       string `json:"uid,omitempty"`
	TTFBMs    int64  `json:"ttfb_ms"` // -1 = 失败
	HeaderMs  int64  `json:"header_ms"`
	Status    int    `json:"status,omitempty"`
	Error     string `json:"error,omitempty"`
	CheckedAt string `json:"checked_at"`
	// Skipped true = 账号侧原因没测成（额度/刷新/风控），节点没有被定罪，
	// 不计失败、不改 FailStreak。
	Skipped bool `json:"skipped,omitempty"`
}

// TTFBAccount 一个可用来打真实对话的账号。
type TTFBAccount struct {
	Auth   *auth.Auth
	Region string
}

// TTFBAccountSource 按分组提供测速账号（server 注入；避免 reqproxy 依赖 groups/pool）。
type TTFBAccountSource interface {
	// AccountsForGroup 该分组里可用于测速的账号。空 group = 不限分组。
	AccountsForGroup(group string) []TTFBAccount
}

// TokenRefresher 按需刷新账号 token（server 注入 upstream.Client 的包装）。
type TokenRefresher interface {
	// EnsureFresh 刷新 access token。返回账号侧错误（刷新失败等）时
	// 调用方应换号重试而不是判节点死。
	EnsureFresh(a *auth.Auth) error
}

// ttfbProbeBody 最小对话：只要上游吐出第一个字即可。
// max_tokens 设小是为了少烧额度，首字时间与生成长度无关。
var ttfbProbeBody = []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":16}`)

// TTFBProber 真实首字测速。
type TTFBProber struct {
	mu      sync.Mutex
	src     TTFBAccountSource
	refresh TokenRefresher
	pool    *Pool
	temp    TempPortDialer
	results map[string]TTFBResult
	running bool
}

// NewTTFBProber 构建。src/temp/refresh 可为 nil（未注入时相应入口返回明确错误）。
func NewTTFBProber(pool *Pool, temp TempPortDialer) *TTFBProber {
	return &TTFBProber{
		pool:    pool,
		temp:    temp,
		results: map[string]TTFBResult{},
	}
}

// SetAccountSource 注入测速账号来源。
func (p *TTFBProber) SetAccountSource(src TTFBAccountSource) {
	p.mu.Lock()
	p.src = src
	p.mu.Unlock()
}

// SetTokenRefresher 注入账号刷新（探测前按需刷新，避免整批号全 401）。
func (p *TTFBProber) SetTokenRefresher(r TokenRefresher) {
	p.mu.Lock()
	p.refresh = r
	p.mu.Unlock()
}

// Running 是否有一轮测速在跑（自动扫描循环让路用）。
func (p *TTFBProber) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// accounts 取分组账号（自动扫描循环用；src 未注入时返回空）。
func (p *TTFBProber) accounts(group string) []TTFBAccount {
	p.mu.Lock()
	src := p.src
	p.mu.Unlock()
	if src == nil {
		return nil
	}
	return src.AccountsForGroup(group)
}

// Results 最近一轮的结果（按节点 ID）。
func (p *TTFBProber) Results() map[string]TTFBResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]TTFBResult, len(p.results))
	for k, v := range p.results {
		out[k] = v
	}
	return out
}

// Run 对池内全部节点测一轮真实首字（手动按钮入口：一次性测完）。
//
// group 指定用哪个分组的账号；同一节点的多次尝试轮换账号，避免一个账号
// 被打满。concurrency 限制同时在飞的探测（每个都是一次真实对话）。
// timeout 是单节点的总上限，含等响应头和等第一帧。
//
// 返回 (测出首字的节点数, 失败数)。
func (p *TTFBProber) Run(group, model string, concurrency int, timeout time.Duration) (ok, fail int, err error) {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return 0, 0, fmt.Errorf("首字测速已在进行中")
	}
	if p.src == nil {
		p.mu.Unlock()
		return 0, 0, fmt.Errorf("首字测速未配置账号来源")
	}
	accounts := p.src.AccountsForGroup(group)
	if len(accounts) == 0 {
		p.mu.Unlock()
		if group == "" {
			return 0, 0, fmt.Errorf("没有可用账号")
		}
		return 0, 0, fmt.Errorf("分组 %q 里没有可用账号", group)
	}
	p.running = true
	p.results = map[string]TTFBResult{}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
	}()

	if concurrency <= 0 {
		concurrency = 4
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	body := probeBodyFor(model)

	nodes := p.pool.NodesSnapshot()
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i, n := range nodes {
		acct := accounts[i%len(accounts)]
		wg.Add(1)
		sem <- struct{}{}
		go func(n Node, acct TTFBAccount) {
			defer wg.Done()
			defer func() { <-sem }()
			res := p.probeWithRetry(n, acct, accounts, body, timeout)
			p.recordResult(n.ID, res)
			mu.Lock()
			if res.Error == "" {
				ok++
			} else {
				fail++
			}
			mu.Unlock()
		}(n, acct)
	}
	wg.Wait()
	return ok, fail, nil
}

// ProbeBatch 自动扫描循环的一拍：探测 nodes 这一批（并发执行），结果直接
// 落健康记录。账号从 accounts 轮换取号（调用方负责分组与轮转游标）。
// 返回每个节点的结果（顺序与入参一致）。
//
// 与 Run 的差异：不持有 running 全程锁（自动循环自身保证互斥）、不重置
// results 快照（节点表数据来自健康记录，快照仅手动轮展示用）。
func (p *TTFBProber) ProbeBatch(nodes []Node, accounts []TTFBAccount, model string, timeout time.Duration) []TTFBResult {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if len(nodes) == 0 || len(accounts) == 0 {
		return nil
	}
	body := probeBodyFor(model)
	out := make([]TTFBResult, len(nodes))
	var wg sync.WaitGroup
	for i := range nodes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i] = p.probeWithRetry(nodes[i], accounts[i%len(accounts)], accounts, body, timeout)
			p.recordResult(nodes[i].ID, out[i])
		}(i)
	}
	wg.Wait()
	return out
}

// recordResult 统一落库 + 快照更新。
func (p *TTFBProber) recordResult(nodeID string, res TTFBResult) {
	checked, _ := time.Parse(time.RFC3339, res.CheckedAt)
	if res.Skipped {
		// 账号侧原因：只更新 TTFB 展示字段，不动 FailStreak/Unhealthy。
		p.pool.MarkTTFBSkipped(nodeID, res.TTFBMs, res.Error, checked)
	} else {
		p.pool.MarkTTFB(nodeID, res.TTFBMs, res.Error, checked)
	}
	p.mu.Lock()
	p.results[nodeID] = res
	p.mu.Unlock()
}

// probeWithRetry 账号侧失败时换下一个账号重试，最多再试 2 个号（防雪崩式
// 烧额度）。candidates 是本次轮次的分组账号序列（调用方传入），从
// primary 的下一个位置开始取。全部尝试都是账号侧失败 → Skipped 结果
// （节点不定罪）。节点侧失败直接返回，不换号（换号也会失败，白烧额度）。
func (p *TTFBProber) probeWithRetry(n Node, primary TTFBAccount, candidates []TTFBAccount, body []byte, timeout time.Duration) TTFBResult {
	res := p.probeNode(n, primary, body, timeout)
	if res.Error == "" || !res.Skipped {
		return res // 成功 / 节点侧失败：都不换号
	}
	if len(candidates) == 0 {
		return res
	}
	// 找 primary 在候选里的位置，从下一个开始换号（找不到就从 0 开始）。
	startIdx := 0
	for i, c := range candidates {
		if c.Auth != nil && primary.Auth != nil && c.Auth.UID == primary.Auth.UID {
			startIdx = i + 1
			break
		}
	}
	last := res
	for k := 0; k < 2 && k < len(candidates); k++ {
		next := candidates[(startIdx+k)%len(candidates)]
		if next.Auth == nil || (primary.Auth != nil && next.Auth.UID == primary.Auth.UID) {
			continue
		}
		retry := p.probeNode(n, next, body, timeout)
		if retry.Error == "" {
			return retry // 换号成功：就是账号的问题
		}
		last = retry
		if !retry.Skipped {
			return retry // 节点侧失败：定性了，不换号
		}
	}
	return last
}

// probeNode 经节点的本地端口发一次流式对话，量到第一帧的时间。
// 结果分三类（Skipped 字段）：
//   - 成功：TTFBMs >= 0，Error 空
//   - 节点侧失败：连接错误/超时/上游 5xx/其他非账号 4xx——节点的问题
//   - 账号侧失败（Skipped=true）：刷新失败、401/403 账号类、额度类——
//     调用方应换号重试而不是定罪节点
func (p *TTFBProber) probeNode(n Node, acct TTFBAccount, body []byte, timeout time.Duration) TTFBResult {
	res := TTFBResult{
		NodeID:    n.ID,
		Name:      n.Name,
		TTFBMs:    -1,
		CheckedAt: time.Now().Format(time.RFC3339),
	}
	if acct.Auth == nil {
		res.Error = "无可用账号"
		res.Skipped = true
		return res
	}
	res.UID = acct.Auth.UID
	// 探测前按需刷新：token 过期的号打出去只会得到一排 401，
	// 全被记成节点失败就冤枉节点了。刷新失败按账号侧失败处理。
	if p.refresh != nil {
		if err := p.refresh.EnsureFresh(acct.Auth); err != nil {
			res.Error = "账号刷新失败: " + err.Error()
			res.Skipped = true
			return res
		}
	}
	if p.temp == nil {
		res.Error = "内核未启用"
		return res
	}
	region := acct.Region
	if region == "" {
		region = "cn"
	}
	port, release, err := p.temp.TempPort(n.NodeSpec, region)
	if err != nil {
		res.Error = "无测速通道: " + err.Error()
		return res
	}
	if release != nil {
		defer release()
	}

	base := "https://copilot.tencent.com"
	if region == "global" {
		base = "https://www.workbuddy.ai"
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v2/chat/completions", bytes.NewReader(body))
	if err != nil {
		res.Error = err.Error()
		return res
	}
	upstream.ChatHeaders(req, acct.Auth)

	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(mustParseURL(fmt.Sprintf("http://127.0.0.1:%d", port))),
			DisableKeepAlives: true,
		},
	}
	start := time.Now()
	resp, err := client.Do(req)
	res.HeaderMs = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		res.Error = fmt.Sprintf("上游 %d: %s", resp.StatusCode, truncateStr(string(raw), 160))
		// 账号侧错误分类（复用线上请求的同一套判定）：换号重试。
		if probeErrIsAccountSide(resp.StatusCode, string(raw)) {
			res.Skipped = true
		}
		return res
	}
	ttfb, err := firstFrame(resp.Body, start)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.TTFBMs = ttfb
	return res
}

// probeErrIsAccountSide 上游 4xx 是否属于「这个账号的问题」（额度/软限流/
// 会话死/风控）。账号侧失败换号重试，不定罪节点；其余（5xx、请求形态
// 4xx）视为节点/链路侧失败。
func probeErrIsAccountSide(status int, body string) bool {
	switch upstream.Classify(status, body) {
	case upstream.ErrHardCredit, upstream.ErrSoftRate,
		upstream.ErrSessionDead, upstream.ErrRiskFlag:
		return true
	}
	return false
}

// probeBodyFor 按模型名生成探测体；空/默认名直接用预置模板。
func probeBodyFor(model string) []byte {
	if model == "" || model == "glm-5.3" {
		return ttfbProbeBody
	}
	var doc map[string]any
	if err := json.Unmarshal(ttfbProbeBody, &doc); err != nil {
		return ttfbProbeBody
	}
	doc["model"] = model
	b, err := json.Marshal(doc)
	if err != nil {
		return ttfbProbeBody
	}
	return b
}

// firstFrame 读到第一条带内容的 SSE data 帧，返回从 start 起的毫秒数。
func firstFrame(r io.Reader, start time.Time) (int64, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		return time.Since(start).Milliseconds(), nil
	}
	if err := sc.Err(); err != nil {
		return -1, err
	}
	return -1, fmt.Errorf("流结束但没有内容帧")
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
