package upstream

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/growthrouteerr"
)

var ErrGrowthAuthStale = errors.New("growth account identity or credentials changed")
var errGrowthRouteClosed = errors.New("growth task route is closed")

// Legacy hook: callers without planning support run exclusively, and acquire
// this lease only after entering the active set. Production uses Plan below.
type GrowthRouteLeaseFunc func(context.Context, string, string) (*url.URL, []string, func(), error)

// Planning is read-only: no listener, reserved port or persistent binding. The
// acquisition closure must revalidate the original node configuration and use
// the planned, frozen connection IP. Unknown proxy IPs require global exclusive
// scheduling for the entire task.
type GrowthRoutePlanFunc func(context.Context, string, string) ([]string, bool, func(context.Context) (*url.URL, func(), error), error)

type growthRouteScope struct {
	client      *http.Client
	err         error
	ctx         context.Context
	cancel      context.CancelFunc
	guard       func() error
	closed      atomic.Bool
	webMu       sync.Mutex
	webSessions map[string]websiteSession
}

func (s *growthRouteScope) check() error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if s.closed.Load() {
		return errGrowthRouteClosed
	}
	if s.err != nil {
		return s.err
	}
	if s.guard != nil {
		return s.guard()
	}
	return nil
}

func (c *Client) growthRouteFor(a *auth.Auth) *growthRouteScope {
	if a == nil {
		return nil
	}
	s, _ := a.EphemeralRequestScope().(*growthRouteScope)
	return s
}
func (c *Client) registerGrowthRoute(a *auth.Auth, s *growthRouteScope) {
	// There is no Client registry. The scope lives only as long as its task
	// auth/aliases. Closed aliases keep their closed scope and fail closed.
	a.SetEphemeralRequestScope(s)
}
func (c *Client) growthAuthSnapshot(a *auth.Auth, s auth.Credentials) *auth.Auth {
	clone := authFromCredentials(s)
	clone.ImportDisabled = s.ImportDisabled
	if scope := c.growthRouteFor(a); scope != nil {
		c.registerGrowthRoute(clone, scope)
	}
	return clone
}

// Ordinary auth values retain their existing behavior. Task snapshots must keep
// their original identity/credential generation and live route until completion.
func (c *Client) GrowthRouteValid(a *auth.Auth) bool {
	s := c.growthRouteFor(a)
	return s == nil || s.check() == nil
}

type GrowthRoute struct {
	Auth      *auth.Auth
	Resources []string
	Err       error
	release   func()
}

func (r *GrowthRoute) Release() {
	if r != nil && r.release != nil {
		r.release()
	}
}

type GrowthRoutePlan struct {
	Auth      *auth.Auth
	Resources []string
	client    *Client
	ctx       context.Context
	guard     func() error
	acquire   func(context.Context) (*url.URL, func(), error)
	err       error
	once      sync.Once
	route     *GrowthRoute
}

func proxyIPResources(ips []string) []string {
	var resources []string
	seen := map[string]bool{}
	for _, raw := range ips {
		ip := net.ParseIP(raw)
		if ip == nil {
			return nil
		}
		key := "proxy-ip:" + ip.String()
		if !seen[key] {
			seen[key] = true
			resources = append(resources, key)
		}
	}
	return resources
}

// PlanGrowthRoute freezes auth and resource metadata without acquiring a slot.
func (c *Client) PlanGrowthRoute(ctx context.Context, a *auth.Auth, guard func() error) *GrowthRoutePlan {
	s := a.Snapshot()
	s.FilePath = ""
	frozen := authFromCredentials(s)
	frozen.ImportDisabled = s.ImportDisabled
	p := &GrowthRoutePlan{Auth: frozen, client: c, ctx: ctx, guard: guard}
	if guard != nil {
		p.err = guard()
	}
	if p.err == nil {
		p.err = ctx.Err()
	}
	if p.err != nil {
		return p
	}
	if c.GrowthRoutePlan != nil {
		var ips []string
		var direct bool
		ips, direct, p.acquire, p.err = c.GrowthRoutePlan(ctx, s.UID, a.Region())
		if p.err != nil {
			p.err = growthrouteerr.Public("growth task proxy planning failed", p.err)
		} else if direct {
			p.Resources = []string{"direct"}
		} else {
			p.Resources = proxyIPResources(ips)
			if p.acquire == nil {
				p.err = growthrouteerr.Public("growth task proxy acquisition unavailable", growthrouteerr.New(growthrouteerr.AcquisitionUnavailable))
			}
		}
	} else if c.GrowthRouteLease != nil {
		p.acquire = func(ctx context.Context) (*url.URL, func(), error) {
			proxy, _, release, err := c.GrowthRouteLease(ctx, s.UID, a.Region())
			return proxy, release, err
		}
	} else if c.DialProxy != nil {
		// Mutable ordinary slots cannot establish a reliable resource plan.
		p.acquire = func(context.Context) (*url.URL, func(), error) {
			proxy, err := c.DialProxy(s.UID, a.Region())
			return proxy, nil, err
		}
	} else {
		p.Resources = []string{"direct"}
	}
	return p
}

func (p *GrowthRoutePlan) Acquire() *GrowthRoute {
	p.once.Do(func() { p.route = p.acquireRoute() })
	return p.route
}
func (p *GrowthRoutePlan) acquireRoute() *GrowthRoute {
	c := p.client
	r := &GrowthRoute{Auth: p.Auth, Resources: p.Resources}
	ctx, cancel := context.WithCancel(p.ctx)
	scope := &growthRouteScope{ctx: ctx, cancel: cancel, guard: p.guard, err: p.err}
	var proxy *url.URL
	var release func()
	if scope.err == nil {
		scope.err = scope.check()
	}
	if scope.err == nil && p.acquire != nil {
		var err error
		proxy, release, err = p.acquire(ctx)
		if err != nil {
			scope.err = growthrouteerr.Public("growth task proxy route unavailable", err)
		}
	}
	if scope.err == nil {
		scope.err = scope.check()
	}
	base := c.HTTP
	if base == nil {
		base = http.DefaultClient
	}
	cli := *base
	var transport *http.Transport
	if proxy != nil {
		if tr, ok := base.Transport.(*http.Transport); ok {
			transport = tr.Clone()
		} else {
			transport = http.DefaultTransport.(*http.Transport).Clone()
		}
		transport.Proxy = http.ProxyURL(proxy)
		cli.Transport = transport
	}
	pinned := c.protocolClient(&cli)
	guarded := *pinned
	rt := pinned.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	guarded.Transport = &growthContextTransport{scope: scope, next: rt}
	scope.client = &guarded
	c.registerGrowthRoute(p.Auth, scope)
	r.Err = scope.err
	var once sync.Once
	r.release = func() {
		once.Do(func() {
			scope.closed.Store(true)
			cancel()
			scope.webMu.Lock()
			scope.webSessions = nil
			scope.webMu.Unlock()
			if transport != nil {
				pinned.CloseIdleConnections()
				c.fingerprintTransports.Delete(transport)
			}
			if release != nil {
				release()
			}
		})
	}
	return r
}

// Compatibility helper for callers that already own their resource reservation.
func (c *Client) PrepareGrowthRoute(ctx context.Context, a *auth.Auth) *GrowthRoute {
	return c.PlanGrowthRoute(ctx, a, nil).Acquire()
}

type growthContextTransport struct {
	scope *growthRouteScope
	next  http.RoundTripper
}

func (t *growthContextTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := t.scope.check(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(t.scope.ctx, cancel)
	resp, err := t.next.RoundTrip(r.Clone(ctx))
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	if err = t.scope.check(); err != nil {
		resp.Body.Close()
		stop()
		cancel()
		return nil, err
	}
	resp.Body = &growthResponseBody{ReadCloser: resp.Body, done: func() { stop(); cancel() }}
	return resp, nil
}

type growthResponseBody struct {
	io.ReadCloser
	once sync.Once
	done func()
}

func (b *growthResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.done)
	return err
}
