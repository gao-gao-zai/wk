// Health：周期测速 + 摘除联动（设计文档 3.4）。
//
// 测速目标按账号 region：cn → copilot.tencent.com，global → workbuddy.ai。
// 测速路径：为节点开临时内核出站 → 经本地端口发 HEAD 请求 → 记录首字节延迟 → 拆除。
// 已指向该节点的槽位直接复用其端口（不额外开销）。
package reqproxy

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

func mustParseURL(raw string) *url.URL {
	u, _ := url.Parse(raw)
	return u
}

// HealthConfig 测速参数。
type HealthConfig struct {
	Interval          time.Duration // 周期，默认 15m
	Timeout           time.Duration // 单次，默认 5s
	UnhealthyCooldown time.Duration // 摘除冷却，默认 10m
	RegionTargets     map[string]string // region → 测速 URL
}

// DefaultHealthConfig 默认参数。
func DefaultHealthConfig() HealthConfig {
	return HealthConfig{
		Interval:          15 * time.Minute,
		Timeout:           5 * time.Second,
		UnhealthyCooldown: 10 * time.Minute,
		RegionTargets: map[string]string{
			"cn":     "https://copilot.tencent.com",
			"global": "https://www.workbuddy.ai",
		},
	}
}

// Health 周期测速器。
type Health struct {
	cfg     HealthConfig
	pool    *Pool
	dial    TempPortDialer // 临时通道：为节点取一个可测的本地端口
	after   func()         // 每轮结束后回调（Manager 注入：预热未绑定账号）
	client  *http.Client
	mu      sync.Mutex
	roundMu sync.Mutex // RunOnce 串行化：周期轮与手动轮绝不并发（临时槽位 tag 冲突 + 完成语义）
	stop    chan struct{}
	done    chan struct{}
}

// TempPortDialer 为测速提供节点的本地端口。
// 已有槽位指向该节点 → 返回槽位端口；否则临时开通道（测试用 stub）。
type TempPortDialer interface {
	// TempPort 返回该节点可用的本地端口，用完调 Release。
	TempPort(node NodeSpec, region string) (port int, release func(), err error)
}

// NewHealth 构建测速器。dial 为 nil 时只用槽位端口（保守）。
func NewHealth(cfg HealthConfig, pool *Pool, dial TempPortDialer) *Health {
	if cfg.Interval <= 0 {
		cfg.Interval = 15 * time.Minute
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.UnhealthyCooldown <= 0 {
		cfg.UnhealthyCooldown = 10 * time.Minute
	}
	return &Health{
		cfg:  cfg,
		pool: pool,
		dial: dial,
		client: &http.Client{
			Timeout: cfg.Timeout,
			// 禁止跟随重定向：只测首连接
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// SetAfterRound 每轮测速结束后的回调（预热挂点）。nil = 无。
func (h *Health) SetAfterRound(fn func()) { h.after = fn }

// Start 启动周期测速。
func (h *Health) Start() {
	go func() {
		defer close(h.done)
		t := time.NewTicker(h.cfg.Interval)
		defer t.Stop()
		h.RunOnce() // 启动立即测一轮（首轮隔离需要数据）
		for {
			select {
			case <-t.C:
				h.RunOnce()
			case <-h.stop:
				return
			}
		}
	}()
}

// Stop 停止。
func (h *Health) Stop() {
	close(h.stop)
	<-h.done
}

// RunOnce 全量测速一轮（手动触发同入口）。
// 返回 (成功数, 失败数)。结束后触发 after 回调（预热）。
// roundMu 串行化：周期轮与手动按钮轮绝不并发——并发时两个轮次会对同一批
// 节点开同名 probe 槽位（tag 冲突 → 内核报错 → 探测全挂），且先结束的
// 轮次会把任务标完成，动画停了而后一轮还在跑。
func (h *Health) RunOnce() (ok, fail int) {
	h.roundMu.Lock()
	defer h.roundMu.Unlock()
	nodes := h.pool.NodesSnapshot()
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, n := range nodes {
		wg.Add(1)
		go func(n Node) {
			defer wg.Done()
			latency, err := h.probe(n)
			h.pool.MarkProbed(n.ID, latency, err)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				fail++
			} else {
				ok++
			}
		}(n)
	}
	wg.Wait()
	if h.after != nil {
		h.after() // 预热：测速数据就绪后给未绑定账号建槽
	}
	return
}

// probe 测单节点：经节点本地端口对 region 目标发 HEAD，量首字节延迟。
func (h *Health) probe(n Node) (int64, error) {
	// 节点的地区（HK/JP/...）不决定测速目标——目标按账号 region（cn/global）分；
	// 节点没有账号维度，统一按 cn 目标测（两个目标都是上游真实地址，可达性高度相关）。
	region := "cn"
	target, ok := h.cfg.RegionTargets[region]
	if !ok {
		for _, v := range h.cfg.RegionTargets {
			target = v
			break
		}
	}
	port, release, err := h.tempPort(n, region)
	if err != nil {
		return -1, fmt.Errorf("节点 %s 无测速通道: %w", n.Name, err)
	}
	if release != nil {
		defer release()
	}

	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	req, err := http.NewRequest(http.MethodHead, target, nil)
	if err != nil {
		return -1, err
	}
	client := &http.Client{
		Timeout: h.cfg.Timeout,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxyURL)),
			// 每次新连接：测的是完整握手 + 出站延迟
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	// 任意 HTTP 状态都算连通（403/404 也说明代理通）
	return time.Since(start).Milliseconds(), nil
}

// tempPort 取测速端口：优先已有槽位（复用），否则经 TempPortDialer 临时开。
func (h *Health) tempPort(n Node, region string) (int, func(), error) {
	// 已有槽位指向该节点 → 直接用
	slots, _ := h.pool.Views()
	for _, s := range slots {
		if s.NodeID == n.ID && s.Port > 0 {
			return s.Port, nil, nil
		}
	}
	if h.dial == nil {
		return 0, nil, fmt.Errorf("无临时通道")
	}
	return h.dial.TempPort(n.NodeSpec, region)
}
