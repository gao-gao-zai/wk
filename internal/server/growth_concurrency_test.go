package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"
	"time"

	"workbuddy2api/internal/growthlimit"
)

func TestGrowthConcurrencyConfigPersistsAndApplies(t *testing.T) {
	h, path := adminConfigTestEnv(t)
	if got := doAdminConfig(t, h, http.MethodGet, nil)["growth_concurrency"]; got != float64(16) {
		t.Fatalf("default=%v", got)
	}
	for _, n := range []int{1, 8, 32, 128} {
		doAdminConfig(t, h, http.MethodPost, map[string]any{"growth_concurrency": n})
		if h.cfg.GrowthBudget.Current() != n || doAdminConfig(t, h, http.MethodGet, nil)["growth_concurrency"] != float64(n) {
			t.Fatal("runtime did not update")
		}
		raw, _ := os.ReadFile(path)
		var doc map[string]any
		_ = json.Unmarshal(raw, &doc)
		if doc["growth_concurrency"] != float64(n) || doc["listen"] != "127.0.0.1:7863" {
			t.Fatal("partial config did not persist safely")
		}
	}
	for _, body := range []string{`{"growth_concurrency":0}`, `{"growth_concurrency":129}`, `{"growth_concurrency":1.5}`, `{"growth_concurrency":"16"}`} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/config", bytes.NewBufferString(body)))
		if rec.Code != 400 || h.cfg.GrowthBudget.Current() != 128 {
			t.Fatal("invalid setting changed budget")
		}
	}
}

func TestGrowthConcurrencyConfigWriteFailureKeepsBudget(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses read-only proc fd directory")
	}
	file, err := os.CreateTemp(t.TempDir(), "config")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err = file.WriteString(`{"schedule":{}}`); err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(t, Config{ConfigPath: fmt.Sprintf("/proc/self/fd/%d", file.Fd())})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/config", bytes.NewBufferString(`{"growth_concurrency":32}`)))
	if rec.Code != 500 || h.cfg.GrowthBudget.Current() != 16 {
		t.Fatalf("write failure changed budget: code=%d limit=%d", rec.Code, h.cfg.GrowthBudget.Current())
	}
}

func TestGrowthConcurrencyOverFourShrinkAndIncrease(t *testing.T) {
	g := growthRunner{budget: growthlimit.New(6)}
	started := make(chan string, 8)
	finished := make(chan string, 8)
	releases := make([]chan struct{}, 8)
	for i := range 8 {
		uid := fmt.Sprint(i)
		releases[i] = make(chan struct{})
		release := releases[i]
		if !g.enqueue(accountJobType{uid: uid, resources: []string{uid}, run: func() { started <- uid; <-release }, done: func(bool) { finished <- uid }}) {
			t.Fatal("enqueue")
		}
	}
	defer func() {
		for _, c := range releases {
			select {
			case <-c:
			default:
				close(c)
			}
		}
	}()
	for range 6 {
		runnerReceive(t, started)
	}
	_ = g.budget.Set(2)
	if g.budget.LeaseLimit() != 6 {
		t.Fatal("shrink revoked old admission before acquire")
	}
	for i := range 4 {
		close(releases[i])
		runnerReceive(t, finished)
	}
	select {
	case uid := <-started:
		t.Fatalf("excess admission after shrink: %s", uid)
	case <-time.After(40 * time.Millisecond):
	}
	close(releases[4])
	runnerReceive(t, finished)
	if runnerReceive(t, started) != "6" {
		t.Fatal("new limit did not allow available permit")
	}
	_ = g.budget.Set(3)
	g.mu.Lock()
	g.signalLocked()
	g.mu.Unlock()
	if runnerReceive(t, started) != "7" {
		t.Fatal("increase did not admit queued work")
	}
	close(releases[5])
	close(releases[6])
	close(releases[7])
	for range 3 {
		runnerReceive(t, finished)
	}
}
