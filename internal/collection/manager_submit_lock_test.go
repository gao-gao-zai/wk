package collection

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func submitLockWait(t *testing.T, done <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func submitLockCall(t *testing.T, label string, call func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("%s was blocked by account preparation", label)
	}
}

// No dispatcher is started: accepted jobs remain queued, so admission limits
// and callback counts are independent of worker scheduling and request rates.
func submitLockManager(t *testing.T, opt Options) *Manager {
	t.Helper()
	if opt.Store == nil {
		opt.Store = &memoryStore{}
	}
	if opt.Run == nil {
		opt.Run = func(context.Context, Target, Snapshot) (json.RawMessage, error) { return nil, nil }
	}
	opt.StartPaused = true
	m, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	m.active = true
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() { m.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("manager cleanup blocked")
		}
	})
	return m
}

func TestSubmitPreparationDoesNotBlockManagerAPIs(t *testing.T) {
	for _, callback := range []string{"UIDs", "Eligible"} {
		t.Run(callback, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var enterOnce, releaseOnce sync.Once
			block := func() { enterOnce.Do(func() { close(entered) }); <-release }
			opt := Options{}
			req := Request{Resources: append([]string(nil), Resources...)}
			if callback == "UIDs" {
				opt.UIDs = func() []string { block(); return []string{"blocked"} }
			} else {
				req.UIDs = []string{"blocked"}
				opt.Eligible = func(uid string, _ bool) bool {
					if uid == "blocked" {
						block()
					}
					return true
				}
			}
			m := submitLockManager(t, opt)
			// Registered after manager cleanup, so failures release the callback
			// before Close attempts to acquire any lock held by an old implementation.
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			seed := submit(t, m, Request{UIDs: []string{"seed"}, Resources: []string{"balance"}})
			submitted := make(chan error, 1)
			go func() { _, err := m.Submit(req); submitted <- err }()
			submitLockWait(t, entered, "account callback")
			submitLockCall(t, "Snapshot", func() error { m.Snapshot("seed", "balance"); return nil })
			submitLockCall(t, "List", func() error {
				jobs, _ := m.List()
				if len(jobs) != 1 {
					return fmt.Errorf("preparing submit became visible: %d jobs", len(jobs))
				}
				return nil
			})
			submitLockCall(t, "Settings", func() error { m.Settings(); return nil })
			submitLockCall(t, "Configure", func() error {
				settings := DefaultSettings()
				settings.Automatic = false
				return m.Configure(settings)
			})
			submitLockCall(t, "Cancel", func() error {
				job, err := m.Cancel(seed.ID)
				if err == nil && job.Cancelled != 1 {
					return fmt.Errorf("queued job was not cancelled: %+v", job)
				}
				return err
			})
			releaseOnce.Do(func() { close(release) })
			select {
			case err := <-submitted:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("released submit did not finish")
			}
		})
	}
}

type submitLockCountingStore struct {
	memoryStore
	saves atomic.Int32
}

func (s *submitLockCountingStore) CollectionSave(rows map[string][]byte, deleted []string) error {
	s.saves.Add(1)
	return s.memoryStore.CollectionSave(rows, deleted)
}

func TestSubmitPreparationCloseRejectsWithoutSave(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	store := &submitLockCountingStore{}
	m := submitLockManager(t, Options{Store: store, Eligible: func(string, bool) bool {
		close(entered)
		<-release
		return true
	}})
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	finished := make(chan error, 1)
	go func() {
		_, err := m.Submit(Request{UIDs: []string{"a"}, Resources: []string{"balance"}})
		finished <- err
	}()
	submitLockWait(t, entered, "eligibility")
	before := store.saves.Load()
	submitLockCall(t, "Close during preparation", func() error { m.Close(); return nil })
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("submit admitted after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("submit did not observe Close")
	}
	if store.saves.Load() != before {
		t.Fatal("closed preparation wrote durable job state")
	}
	if jobs, _ := m.List(); len(jobs) != 0 {
		t.Fatal("closed preparation published a job")
	}
}

func TestSubmitPreparationConcurrentJobLimit(t *testing.T) {
	const attempts = 21
	entered := make(chan struct{}, attempts)
	release := make(chan struct{})
	var releaseOnce sync.Once
	m := submitLockManager(t, Options{Eligible: func(string, bool) bool { entered <- struct{}{}; <-release; return true }})
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	results := make(chan error, attempts)
	for index := 0; index < attempts; index++ {
		go func(index int) {
			_, err := m.Submit(Request{UIDs: []string{fmt.Sprintf("account-%d", index)}, Resources: []string{"balance"}})
			results <- err
		}(index)
	}
	for index := 0; index < attempts; index++ {
		submitLockWait(t, entered, "concurrent eligibility preparations")
	}
	releaseOnce.Do(func() { close(release) })
	accepted := 0
	for index := 0; index < attempts; index++ {
		select {
		case err := <-results:
			if err == nil {
				accepted++
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent submit did not finish")
		}
	}
	jobs, _ := m.List()
	if accepted != 20 || len(jobs) != 20 {
		t.Fatalf("accepted=%d active jobs=%d, want 20", accepted, len(jobs))
	}
}

func TestSubmitPreparationEligibilityOncePerUID(t *testing.T) {
	calls := map[string]int{}
	m := submitLockManager(t, Options{Eligible: func(uid string, allow bool) bool {
		if !allow {
			t.Error("include-disabled flag was lost")
		}
		calls[uid]++
		return uid != "disabled"
	}})
	j := submit(t, m, Request{UIDs: []string{"a", "a", "disabled"}, Resources: append([]string(nil), Resources...), IncludeDisabled: true})
	if calls["a"] != 1 || calls["disabled"] != 1 || len(calls) != 2 {
		t.Fatalf("eligibility calls=%v, want one per distinct UID", calls)
	}
	if j.Total != 10 || j.Skipped != 5 {
		t.Fatalf("five-resource eligibility/dedup result: %+v", j)
	}
}

func TestSubmitPreparationCallbacksMayReadManager(t *testing.T) {
	var m *Manager
	callbackErrors := make(chan error, 2)
	read := func() bool {
		done := make(chan struct{})
		go func() { m.Snapshot("a", "balance"); m.Settings(); close(done) }()
		select {
		case <-done:
			return true
		case <-time.After(time.Second):
			callbackErrors <- fmt.Errorf("account callback cannot reenter snapshot/settings")
			return false
		}
	}
	m = submitLockManager(t, Options{UIDs: func() []string { read(); return []string{"a"} }, Eligible: func(string, bool) bool { return read() }})
	job := submit(t, m, Request{Resources: []string{"balance"}})
	select {
	case err := <-callbackErrors:
		t.Fatal(err)
	default:
	}
	if job.Total != 1 || job.Skipped != 0 {
		t.Fatalf("callback result: %+v", job)
	}
}

func TestSubmitPreparationRechecksEligibilityBeforeDispatch(t *testing.T) {
	var checks, runs atomic.Int32
	rechecked := make(chan struct{})
	m, err := New(Options{Store: &memoryStore{}, Eligible: func(string, bool) bool {
		if checks.Add(1) == 1 {
			return true
		}
		close(rechecked)
		return false
	}, Run: func(context.Context, Target, Snapshot) (json.RawMessage, error) { runs.Add(1); return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { submitLockCall(t, "dispatcher cleanup", func() error { m.Close(); return nil }) })
	job := submit(t, m, Request{UIDs: []string{"a"}, Resources: []string{"balance"}})
	submitLockWait(t, rechecked, "dispatch eligibility recheck")
	if result := waitJob(t, m, job.ID); result.Skipped != 1 || runs.Load() != 0 || checks.Load() != 2 {
		t.Fatalf("dispatch bypassed eligibility recheck: job=%+v checks=%d runs=%d", result, checks.Load(), runs.Load())
	}
}
