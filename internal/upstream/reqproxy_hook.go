// reqproxy 接入：Client 的 DialProxy 钩子 + 按端口缓存 Transport。
//
// 设计文档 3.6：账号级统一路由——绑定账号的一切上游流量走它的槽位端口。
// DialProxy == nil（模块关闭）时行为与现状逐字节一致（零回归）。
package upstream

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"

	"workbuddy2api/internal/auth"
)

// ErrNoProxyNode 代理层没有可用节点（reqproxy D4）。
// 这是池级故障：请求必须失败，但调用方不应把它记成账号的错
// （不喂熔断、不禁用）。reqproxy 侧用 errors.Is 可识别的同名错误包装进来。
var ErrNoProxyNode = errors.New("无可用代理节点")

// DialProxyFunc 返回该账号应走的本地代理（reqproxy 槽位端口）。
//
// uid: 账号 UID（绑定表索引）；region: cn|global（新槽位分域）。
// 返回 (nil, nil) = 直连（模块关闭/空池降级）；err = 无可用节点（不回退直连）。
type DialProxyFunc func(uid, region string) (*url.URL, error)

// transportCache 按代理地址缓存 Transport（连接池不串）。
type transportCache struct {
	mu    sync.Mutex
	cache map[string]*http.Transport
}

var proxyTransports = &transportCache{cache: map[string]*http.Transport{}}

// transportFor 取（或建）指向 proxy 的 Transport。proxy 为 nil 返回 nil（直连）。
func (t *transportCache) transportFor(proxy *url.URL) *http.Transport {
	if proxy == nil {
		return nil
	}
	key := proxy.String()
	t.mu.Lock()
	defer t.mu.Unlock()
	if tr, ok := t.cache[key]; ok {
		return tr
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.MaxIdleConns = 512
	base.MaxIdleConnsPerHost = 256
	base.Proxy = http.ProxyURL(proxy)
	t.cache[key] = base
	return base
}

// authOpt 账号的最小识别信息（从 *auth.Auth 提取，避免多处重复）。
type authOpt struct {
	uid    string
	region string
}

func authOptOf(a *auth.Auth) *authOpt {
	if a == nil {
		return nil
	}
	return &authOpt{uid: a.UID, region: a.Region()}
}

// proxyClientFor 返回该账号应使用的 http.Client（直连时为 c.HTTP 原样）。
// 走代理时基于 c.HTTP 复制并替换 Transport——保留 Timeout 等策略。
func (c *Client) proxyClientFor(a *auth.Auth) (*http.Client, error) {
	if c.DialProxy == nil {
		return c.HTTP, nil
	}
	ao := authOptOf(a)
	if ao == nil {
		return c.HTTP, nil
	}
	proxy, err := c.DialProxy(ao.uid, ao.region)
	if err != nil {
		// D4：无可用节点，调用方必须让请求失败。
		// 包一层哨兵：账号侧可以识别出"这是代理池的错"，不惩罚账号。
		return nil, fmt.Errorf("%w: %w", ErrNoProxyNode, err)
	}
	if proxy == nil {
		return c.HTTP, nil // 直连
	}
	cli := *c.HTTP
	cli.Transport = proxyTransports.transportFor(proxy)
	return &cli, nil
}
