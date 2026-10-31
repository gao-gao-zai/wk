package lifecycle

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
)

// ListenControl never removes an existing socket: an operator must first prove
// its owner has exited. The parent directory must be private to the service.
func ListenControl(path string, handler http.Handler) (*http.Server, error) {
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		l.Close()
		return nil, err
	}
	s := &http.Server{Handler: handler, ReadHeaderTimeout: 5e9, MaxHeaderBytes: 8192}
	go func() { _ = s.Serve(l) }()
	return s, nil
}

// Control is available only on the private Unix socket, not public HTTP routes.
func Control(release string, foreground *Gate, ready func() bool, finalize func() error, stop func()) http.Handler {
	m := http.NewServeMux()
	var mu sync.Mutex
	stopping := false
	m.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		p, n := foreground.Status()
		b, tasks := Default.Status()
		_ = json.NewEncoder(w).Encode(map[string]any{"release": release, "paused": p, "active": n, "background_paused": b, "background": tasks, "ready": ready()})
	})
	m.HandleFunc("POST /{action}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if stopping {
			http.Error(w, "already stopping", 409)
			return
		}
		if r.URL.Query().Get("release") != release {
			http.Error(w, "release mismatch", 409)
			return
		}
		switch r.PathValue("action") {
		case "pause":
			foreground.Pause()
		case "quiesce":
			p, n := foreground.Status()
			if !p || n != 0 {
				http.Error(w, "foreground not drained", 409)
				return
			}
			Default.Pause()
		case "resume":
			Default.Resume()
			foreground.Resume()
		case "stop":
			p, n := foreground.Status()
			b, tasks := Default.Status()
			if !p || !b || n != 0 || tasks != 0 {
				http.Error(w, "not drained", 409)
				return
			}
			if err := finalize(); err != nil {
				http.Error(w, "final commit failed; release remains paused", 500)
				return
			}
			stopping = true
			// The process may exit before net/http writes a chunked terminator.
			// A fixed-length flushed acknowledgment remains complete on close.
			w.Header().Set("Content-Length", "3")
			fmt.Fprintln(w, "ok")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			stop()
			return
		default:
			http.Error(w, "unknown action", 404)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	return m
}
