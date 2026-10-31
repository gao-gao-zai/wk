package handoff

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gatewayFor(t *testing.T, target string) *Gateway {
	t.Helper()
	g, err := New(Config{Target: target, Release: "a", MaxPending: 2, MaxActive: 4, MaxBody: 100000, MaxQueuedBytes: 300000, Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return g
}
func control(t *testing.T, g *Gateway, path string, want int) {
	t.Helper()
	w := httptest.NewRecorder()
	g.Control().ServeHTTP(w, httptest.NewRequest("POST", path, nil))
	if w.Code != want {
		t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
	}
}
func TestStreamDrainsAndQueuedPOSTRunsOnlyOnNewRelease(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: old\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-finish
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer a.Close()
	g := gatewayFor(t, a.URL)
	ingress := httptest.NewServer(g)
	defer ingress.Close()
	oldDone := make(chan string, 1)
	go func() {
		r, e := http.Get(ingress.URL)
		if e != nil {
			oldDone <- e.Error()
			return
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		oldDone <- string(b)
	}()
	<-started
	control(t, g, "/hold?expected=a", 204)
	control(t, g, "/switch?expected=a&next=b&target="+a.URL, 409)
	newCalls := make(chan string, 2)
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		newCalls <- string(raw)
		io.WriteString(w, "new")
	}))
	defer b.Close()
	newDone := make(chan string, 1)
	go func() {
		r, e := http.Post(ingress.URL, "application/json", strings.NewReader(`{"input":"one"}`))
		if e != nil {
			newDone <- e.Error()
			return
		}
		defer r.Body.Close()
		v, _ := io.ReadAll(r.Body)
		newDone <- string(v)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		g.mu.Lock()
		n := len(g.queue)
		g.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request not queued")
		}
		time.Sleep(time.Millisecond)
	}
	close(finish)
	if v := <-oldDone; !strings.Contains(v, "[DONE]") {
		t.Fatal(v)
	}
	control(t, g, "/switch?expected=a&next=b&target="+b.URL, 204)
	control(t, g, "/resume?expected=a", 409)
	control(t, g, "/resume?expected=b", 204)
	if v := <-newDone; v != "new" {
		t.Fatal(v)
	}
	if v := <-newCalls; v != `{"input":"one"}` {
		t.Fatal(v)
	}
	select {
	case <-newCalls:
		t.Fatal("request replayed")
	default:
	}
}
func TestQueueCancelFullAndTimeout(t *testing.T) {
	g := gatewayFor(t, "http://127.0.0.1:1")
	g.cfg.Wait = 30 * time.Millisecond
	control(t, g, "/hold?expected=a", 204)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := g.acquire(ctx, 200000); done <- err }()
	for {
		g.mu.Lock()
		n := len(g.queue)
		g.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := g.acquire(context.Background(), 200000); err == nil {
		t.Fatal("byte limit ignored")
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatal(err)
	}
	if _, err := g.acquire(context.Background(), 1); err == nil {
		t.Fatal("deadline ignored")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.queue) != 0 || g.bytes != 0 || g.active != 0 {
		t.Fatal("leaked queue reservation")
	}
}
func TestTargetsStayLocal(t *testing.T) {
	for _, s := range []string{"http://example.com:80", "http://127.0.0.1:80/path", "https://127.0.0.1:80", "http://user:pass@127.0.0.1:80"} {
		if _, e := localTarget(s); e == nil {
			t.Fatal(s)
		}
	}
}

func TestDurableTargetAndHold(t *testing.T) {
	c := Config{Target: "http://127.0.0.1:10001", Release: "a", MaxPending: 2, MaxActive: 2, MaxBody: 1000, MaxQueuedBytes: 100000, Wait: time.Second, StatePath: filepath.Join(t.TempDir(), "gateway.state")}
	g, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	control(t, g, "/hold?expected=a", 204)
	control(t, g, "/switch?expected=a&next=b&target=http://127.0.0.1:10002", 204)
	restarted, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.release != "b" || restarted.target.Port() != "10002" || !restarted.held {
		t.Fatal("restart lost committed release or hold")
	}
}
