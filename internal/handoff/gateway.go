// Package handoff keeps client connections open across single-writer releases.
package handoff

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type Config struct {
	Target, Release         string
	MaxPending, MaxActive   int
	MaxBody, MaxQueuedBytes int64
	Wait                    time.Duration
	TrustIngress            bool
	StatePath               string
}
type waiter struct {
	ready      chan struct{}
	target     *url.URL
	bytes      int64
	dispatched bool
}
type Gateway struct {
	mu      sync.Mutex
	cfg     Config
	target  *url.URL
	release string
	held    bool
	active  int
	bytes   int64
	queue   []*waiter
	proxy   *httputil.ReverseProxy
}

func localTarget(raw string) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("target must be a loopback HTTP origin")
	}
	ip := net.ParseIP(u.Hostname())
	port, e := strconv.Atoi(u.Port())
	if ip == nil || !ip.IsLoopback() || e != nil || port < 1 || port > 65535 {
		return nil, errors.New("target must use a loopback IP and explicit port")
	}
	return u, nil
}
func New(c Config) (*Gateway, error) {
	held := false
	if c.StatePath != "" {
		raw, err := os.ReadFile(c.StatePath)
		if err == nil {
			var s gatewayState
			if json.Unmarshal(raw, &s) != nil || s.Version != 1 || s.Release == "" {
				return nil, errors.New("invalid gateway state")
			}
			c.Target = s.Target
			c.Release = s.Release
			held = s.Held
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	u, e := localTarget(c.Target)
	if e != nil {
		return nil, e
	}
	if c.Release == "" || c.MaxPending < 1 || c.MaxActive < 1 || c.MaxBody < 1 || c.MaxQueuedBytes < c.MaxBody || c.Wait <= 0 {
		return nil, errors.New("invalid gateway limits or release")
	}
	g := &Gateway{cfg: c, target: u, release: c.Release, held: held}
	if err := g.persist(); err != nil {
		return nil, err
	}
	g.proxy = &httputil.ReverseProxy{
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(p.In.Context().Value(targetKey{}).(*url.URL))
			p.Out.Host = p.In.Host
			if c.TrustIngress {
				p.Out.Header["X-Forwarded-For"] = append([]string(nil), p.In.Header.Values("X-Forwarded-For")...)
			}
			p.SetXForwarded()
			if c.TrustIngress {
				proto := p.In.Header.Get("X-Forwarded-Proto")
				if proto == "https" || proto == "http" {
					p.Out.Header.Set("X-Forwarded-Proto", proto)
				}
			}
		},
		FlushInterval: -1,
		// Never retry a request whose upstream execution may already have begun.
		Transport:    &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext, MaxIdleConnsPerHost: 32},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) { http.Error(w, "backend unavailable", 502) },
	}
	return g, nil
}

type gatewayState struct {
	Version         int
	Target, Release string
	Held            bool
}

func (g *Gateway) persist() error {
	if g.cfg.StatePath == "" {
		return nil
	}
	raw, err := json.Marshal(gatewayState{1, g.target.String(), g.release, g.held})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(g.cfg.StatePath+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(g.cfg.StatePath+".tmp", g.cfg.StatePath); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(g.cfg.StatePath))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

type targetKey struct{}

func (g *Gateway) dispatch() {
	for !g.held && g.active < g.cfg.MaxActive && len(g.queue) > 0 {
		q := g.queue[0]
		g.queue = g.queue[1:]
		g.bytes -= q.bytes
		q.target = g.target
		q.dispatched = true
		g.active++
		close(q.ready)
	}
}
func (g *Gateway) leave() { g.mu.Lock(); g.active--; g.dispatch(); g.mu.Unlock() }
func (g *Gateway) acquire(ctx context.Context, size int64) (*url.URL, error) {
	g.mu.Lock()
	if !g.held && g.active < g.cfg.MaxActive && len(g.queue) == 0 {
		g.active++
		u := g.target
		g.mu.Unlock()
		return u, nil
	}
	if len(g.queue) >= g.cfg.MaxPending || size > g.cfg.MaxQueuedBytes-g.bytes {
		g.mu.Unlock()
		return nil, errors.New("upgrade queue full")
	}
	q := &waiter{ready: make(chan struct{}), bytes: size}
	g.queue = append(g.queue, q)
	g.bytes += size
	g.mu.Unlock()
	t := time.NewTimer(g.cfg.Wait)
	defer t.Stop()
	var err error
	select {
	case <-q.ready:
		return q.target, nil
	case <-ctx.Done():
		err = ctx.Err()
	case <-t.C:
		err = errors.New("upgrade queue timeout")
	}
	g.mu.Lock()
	if q.dispatched {
		g.active--
	} else {
		for i, v := range g.queue {
			if v == q {
				g.queue = append(g.queue[:i], g.queue[i+1:]...)
				g.bytes -= q.bytes
				break
			}
		}
	}
	g.dispatch()
	g.mu.Unlock()
	return nil, err
}
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > g.cfg.MaxBody {
		http.Error(w, "request too large", 413)
		return
	}
	// Do not read or spool queued bodies. Reserving the maximum for unknown
	// lengths and bounding headers/connections keeps admission memory bounded.
	size := r.ContentLength
	if size < 0 {
		size = g.cfg.MaxBody
	}
	size += 32768
	u, err := g.acquire(r.Context(), size)
	if err != nil {
		w.Header().Set("Retry-After", "1")
		http.Error(w, err.Error(), 503)
		return
	}
	defer g.leave()
	if r.Context().Err() != nil {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, g.cfg.MaxBody)
	g.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), targetKey{}, u)))
}

// Control must only be bound to a private Unix socket. Expected release is a
// compare-and-swap precondition, not an authentication credential.
func (g *Gateway) Control() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"release": g.release, "held": g.held, "active": g.active, "pending": len(g.queue), "queued_bytes": g.bytes, "wait_seconds": g.cfg.Wait.Seconds()})
	})
	m.HandleFunc("POST /{action}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if r.URL.Query().Get("expected") != g.release {
			http.Error(w, "release mismatch", 409)
			return
		}
		oldTarget, oldRelease, oldHeld := g.target, g.release, g.held
		switch r.PathValue("action") {
		case "hold":
			g.held = true
		case "resume":
			g.held = false
		case "switch":
			if !g.held || g.active != 0 {
				http.Error(w, "not drained", 409)
				return
			}
			u, err := localTarget(r.URL.Query().Get("target"))
			next := r.URL.Query().Get("next")
			if err != nil || next == "" || next == g.release {
				http.Error(w, "invalid target or release", 400)
				return
			}
			g.target = u
			g.release = next
		default:
			http.Error(w, "unknown action", 404)
			return
		}
		if err := g.persist(); err != nil {
			g.target = oldTarget
			g.release = oldRelease
			g.held = oldHeld
			http.Error(w, "state commit failed; inspect gateway state before recovery", 500)
			return
		}
		g.dispatch()
		w.WriteHeader(204)
	})
	return m
}
