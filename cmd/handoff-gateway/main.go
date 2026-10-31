package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"workbuddy2api/internal/handoff"
	"workbuddy2api/internal/lifecycle"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:7863", "stable ingress address")
	target := flag.String("target", "http://127.0.0.1:7864", "initial backend origin")
	release := flag.String("release", "", "initial backend release ID")
	socket := flag.String("control", "", "private Unix control socket")
	state := flag.String("state", "", "durable release-state file; defaults to control socket + .state")
	wait := flag.Duration("wait", 90*time.Second, "maximum queue wait; align every client and proxy timeout")
	pending := flag.Int("pending", 128, "maximum queued requests")
	active := flag.Int("active", 128, "maximum active requests")
	body := flag.Int64("body-bytes", 8000000, "maximum request bytes; match application limit")
	queued := flag.Int64("queued-bytes", 64000000, "maximum reserved queued bytes")
	trustIngress := flag.Bool("trust-ingress", false, "trust forwarded headers from the local TLS ingress; restrict local access")
	flag.Parse()
	if *socket == "" {
		log.Fatal("-control required in a private directory")
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		log.Fatal("listen must be a loopback IP")
	}
	releaseLock, err := lifecycle.Lock(*socket + ".lock")
	if err != nil {
		log.Fatal(err)
	}
	defer releaseLock()
	if *state == "" {
		*state = *socket + ".state"
	}
	g, err := handoff.New(handoff.Config{Target: *target, Release: *release, MaxPending: *pending, MaxActive: *active, MaxBody: *body, MaxQueuedBytes: *queued, Wait: *wait, TrustIngress: *trustIngress, StatePath: *state})
	if err != nil {
		log.Fatal(err)
	}
	control, err := lifecycle.ListenControl(*socket, g.Control())
	if err != nil {
		log.Fatal(err)
	}
	defer control.Close()
	s := &http.Server{Addr: *listen, Handler: g, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 32768}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	done := make(chan struct{})
	go func() {
		<-sig
		defer close(done)
		log.Print("gateway termination requested; waiting for active requests (queued connections cannot survive gateway restart)")
		_ = s.Shutdown(context.Background())
	}()
	if err = s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	<-done
}
