package lifecycle

import (
	"context"
	"testing"
	"time"
)

func TestPauseTracksAsyncChildAndResumes(t *testing.T) {
	var g Gate
	parent, ok := g.Enter()
	if !ok {
		t.Fatal("closed initially")
	}
	child := g.Track()
	g.Pause()
	parent()
	parent()
	if _, ok = g.Enter(); ok {
		t.Fatal("admission open")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if g.Wait(ctx) == nil {
		t.Fatal("missed child")
	}
	child()
	if e := g.Wait(context.Background()); e != nil {
		t.Fatal(e)
	}
	g.Resume()
	done, ok := g.Enter()
	if !ok {
		t.Fatal("resume failed")
	}
	done()
}

func TestDeferredScheduledDispatchAndCancellation(t *testing.T) {
	var g Gate
	g.Pause()
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		done, err := g.EnterContext(ctx)
		if err == nil {
			close(started)
			done()
		}
	}()
	select {
	case <-started:
		t.Fatal("dispatched during pause")
	case <-time.After(time.Millisecond):
	}
	g.Resume()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("due work lost on resume")
	}
	g.Pause()
	cancel()
	if _, err := g.EnterContext(ctx); err == nil {
		t.Fatal("canceled scheduled work admitted")
	}
}
