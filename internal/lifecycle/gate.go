// Package lifecycle provides a reversible admission barrier for planned upgrades.
package lifecycle

import (
	"context"
	"net/http"
	"sync"
)

// Gate serializes admission and pause. The zero value is open.
type Gate struct {
	mu      sync.Mutex
	paused  bool
	active  int
	changed chan struct{}
}

var Default Gate

// Track is for a child of an already admitted operation. Reserve synchronously
// before starting its goroutine so Wait cannot miss the child.
func (g *Gate) Track() func() {
	g.mu.Lock()
	g.active++
	g.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { g.mu.Lock(); g.active--; g.signal(); g.mu.Unlock() }) }
}
func Go(fn func()) { done := Default.Track(); go func() { defer done(); fn() }() }

func (g *Gate) signal() {
	if g.changed != nil {
		close(g.changed)
	}
	g.changed = make(chan struct{})
}

// Enter reserves an entire operation, including its asynchronous completion.
func (g *Gate) Enter() (func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused {
		return nil, false
	}
	g.active++
	var once sync.Once
	return func() { once.Do(func() { g.mu.Lock(); g.active--; g.signal(); g.mu.Unlock() }) }, true
}

// EnterContext defers a scheduled dispatch while paused. A canceled upgrade
// resumes the same due work instead of silently losing a scheduled time slot.
func (g *Gate) EnterContext(ctx context.Context) (func(), error) {
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if done, ok := g.Enter(); ok {
			return done, nil
		}
		g.mu.Lock()
		if !g.paused {
			g.mu.Unlock()
			continue
		}
		if g.changed == nil {
			g.changed = make(chan struct{})
		}
		ch := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ch:
		}
	}
}

func (g *Gate) Pause()              { g.mu.Lock(); g.paused = true; g.signal(); g.mu.Unlock() }
func (g *Gate) Resume()             { g.mu.Lock(); g.paused = false; g.signal(); g.mu.Unlock() }
func (g *Gate) Status() (bool, int) { g.mu.Lock(); defer g.mu.Unlock(); return g.paused, g.active }
func (g *Gate) Wait(ctx context.Context) error {
	for {
		g.mu.Lock()
		if g.active == 0 {
			g.mu.Unlock()
			return nil
		}
		if g.changed == nil {
			g.changed = make(chan struct{})
		}
		ch := g.changed
		g.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (g *Gate) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		done, ok := g.Enter()
		if !ok {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "service draining", http.StatusServiceUnavailable)
			return
		}
		defer done()
		next.ServeHTTP(w, r)
	})
}
