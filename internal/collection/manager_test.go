package collection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memoryStore struct {
	mu   sync.Mutex
	rows map[string][]byte
	fail bool
}

func (s *memoryStore) CollectionLoad() (map[string][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]byte{}
	for k, b := range s.rows {
		out[k] = append([]byte(nil), b...)
	}
	return out, nil
}
func (s *memoryStore) CollectionSave(rows map[string][]byte, del []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("disk full")
	}
	if s.rows == nil {
		s.rows = map[string][]byte{}
	}
	for k, b := range rows {
		s.rows[k] = append([]byte(nil), b...)
	}
	for _, k := range del {
		delete(s.rows, k)
	}
	return nil
}
func waitJob(t *testing.T, m *Manager, id string) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, ok := m.Job(id)
		if ok && terminalJob(j.Status) {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return Job{}
}
func submit(t *testing.T, m *Manager, r Request) Job {
	t.Helper()
	j, e := m.Submit(r)
	if e != nil {
		t.Fatal(e)
	}
	return j
}
func TestBoundedConcurrencyAndDedup(t *testing.T) {
	var active, maximum, calls atomic.Int32
	started := make(chan struct{}, 20)
	release := make(chan struct{})
	m, e := New(Options{Store: &memoryStore{}, Run: func(ctx context.Context, t Target, s Snapshot) (json.RawMessage, error) {
		n := active.Add(1)
		for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		active.Add(-1)
		return json.RawMessage(`{"remaining":42}`), nil
	}})
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	j := submit(t, m, Request{UIDs: []string{"a", "b", "c", "d", "e", "f"}, Resources: []string{"balance"}})
	for i := 0; i < 4; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("workers not started")
		}
	}
	dup := submit(t, m, Request{UIDs: []string{"a"}, Resources: []string{"balance"}})
	close(release)
	waitJob(t, m, j.ID)
	waitJob(t, m, dup.ID)
	if maximum.Load() != 4 || calls.Load() != 6 {
		t.Fatalf("maximum=%d calls=%d", maximum.Load(), calls.Load())
	}
}
func TestPriorityCancellationAndRetry(t *testing.T) {
	started := make(chan string, 10)
	release := make(chan struct{})
	var fail atomic.Bool
	fail.Store(true)
	m, _ := New(Options{Store: &memoryStore{}, Run: func(ctx context.Context, t Target, s Snapshot) (json.RawMessage, error) {
		started <- t.UID
		if t.UID == "first" {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		if t.UID == "bad" && fail.Load() {
			return nil, &Failure{Kind: "client", Message: "rejected"}
		}
		return json.RawMessage(`1`), nil
	}})
	defer m.Close()
	cfg := m.Settings()
	cfg.Concurrency = 1
	_ = m.Configure(cfg)
	first := submit(t, m, Request{UIDs: []string{"first"}, Resources: []string{"balance"}})
	<-started
	background := submit(t, m, Request{UIDs: []string{"later", "cancel"}, Resources: []string{"balance"}, Source: "scheduled"})
	manual := submit(t, m, Request{UIDs: []string{"bad"}, Resources: []string{"balance"}})
	if _, e := m.Cancel(background.ID); e != nil {
		t.Fatal(e)
	}
	close(release)
	waitJob(t, m, first.ID)
	if uid := <-started; uid != "bad" {
		t.Fatal(uid)
	}
	if j := waitJob(t, m, background.ID); j.Cancelled != 2 {
		t.Fatalf("%+v", j)
	}
	waitJob(t, m, manual.ID)
	fail.Store(false)
	r, e := m.Retry(manual.ID)
	if e != nil {
		t.Fatal(e)
	}
	if j := waitJob(t, m, r.ID); j.Success != 1 || j.Total != 1 {
		t.Fatalf("%+v", j)
	}
}
func TestRestartAndOldValueRetention(t *testing.T) {
	store := &memoryStore{}
	var fail atomic.Bool
	m, _ := New(Options{Store: store, Run: func(context.Context, Target, Snapshot) (json.RawMessage, error) {
		if fail.Load() {
			return nil, &Failure{Kind: "rate", Message: "limited"}
		}
		return json.RawMessage(`42`), nil
	}})
	j := submit(t, m, Request{UIDs: []string{"a"}, Resources: []string{"balance"}})
	waitJob(t, m, j.ID)
	fail.Store(true)
	j = submit(t, m, Request{UIDs: []string{"a"}, Resources: []string{"balance"}})
	waitJob(t, m, j.ID)
	s := m.Snapshot("a", "balance")
	if string(s.Data) != "42" || s.LastSuccess == 0 || s.State != "failed" {
		t.Fatalf("%+v", s)
	}
	m.Close()
	// Simulate hard process loss using durable queued/running records.
	item := &Item{Target: Target{"b", "models"}, Status: "running"}
	job := &Job{ID: "crashed", Status: "running", Items: []*Item{item}, Total: 1}
	_ = store.CollectionSave(map[string][]byte{"job/crashed": jobJSON(job), itemKey("crashed", item.Target): encode(item)}, nil)
	var calls atomic.Int32
	m2, e := New(Options{Store: store, Run: func(context.Context, Target, Snapshot) (json.RawMessage, error) { calls.Add(1); return nil, nil }})
	if e != nil {
		t.Fatal(e)
	}
	defer m2.Close()
	recovered, _ := m2.Job("crashed")
	if recovered.Status != "interrupted" || recovered.Items[0].Status != "interrupted" || calls.Load() != 0 {
		t.Fatalf("%+v", recovered)
	}
	if string(m2.Snapshot("a", "balance").Data) != "42" {
		t.Fatal("lost last value")
	}
}
func TestInvalidationDiscardsLateResponseAndRefetches(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	m, _ := New(Options{Store: &memoryStore{}, Run: func(ctx context.Context, _ Target, _ Snapshot) (json.RawMessage, error) {
		n := calls.Add(1)
		started <- struct{}{}
		if n == 1 {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return json.RawMessage(fmt.Sprint(n)), nil
	}})
	defer m.Close()
	old := submit(t, m, Request{UIDs: []string{"a"}, Resources: []string{"balance"}})
	<-started
	m.Forget("a")
	next := submit(t, m, Request{UIDs: []string{"a"}, Resources: []string{"balance"}})
	select {
	case <-started:
		t.Fatal("same resource ran concurrently across invalidation")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if j := waitJob(t, m, old.ID); j.Skipped != 1 {
		t.Fatalf("%+v", j)
	}
	waitJob(t, m, next.ID)
	if string(m.Snapshot("a", "balance").Data) != "2" {
		t.Fatal("late response won")
	}
}
func TestRetryAfterAndStorageFailure(t *testing.T) {
	store := &memoryStore{}
	var calls atomic.Int32
	started := time.Now()
	m, _ := New(Options{Store: store, Run: func(context.Context, Target, Snapshot) (json.RawMessage, error) {
		if calls.Add(1) == 1 {
			return nil, &Failure{Kind: "rate", Message: "429", Retryable: true, After: 40 * time.Millisecond}
		}
		return json.RawMessage(`1`), nil
	}})
	defer m.Close()
	j := submit(t, m, Request{UIDs: []string{"a"}, Resources: []string{"balance"}})
	if got := waitJob(t, m, j.ID); got.Items[0].Attempts != 2 || time.Since(started) < 40*time.Millisecond {
		t.Fatal("backoff not honored")
	}
	store.mu.Lock()
	store.fail = true
	store.mu.Unlock()
	if _, e := m.Submit(Request{UIDs: []string{"b"}, Resources: []string{"balance"}}); e == nil {
		t.Fatal("acknowledged unpersisted job")
	}
}

func TestCollectionManualPriority(t *testing.T) {
	started := make(chan string, 4)
	release := make(chan struct{})
	m, _ := New(Options{Store: &memoryStore{}, Run: func(ctx context.Context, t Target, _ Snapshot) (json.RawMessage, error) {
		started <- t.UID
		if t.UID == "first" {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return nil, nil
	}})
	defer m.Close()
	cfg := m.Settings()
	cfg.Concurrency = 1
	_ = m.Configure(cfg)
	first := submit(t, m, Request{UIDs: []string{"first"}, Resources: []string{"balance"}})
	<-started
	background := submit(t, m, Request{UIDs: []string{"background"}, Resources: []string{"balance"}, Source: "scheduled"})
	manual := submit(t, m, Request{UIDs: []string{"manual"}, Resources: []string{"balance"}})
	close(release)
	if uid := <-started; uid != "manual" {
		t.Fatalf("manual did not jump queued periodic work: %s", uid)
	}
	waitJob(t, m, first.ID)
	waitJob(t, m, background.ID)
	waitJob(t, m, manual.ID)
}

func TestCollectionAutomaticPolicy(t *testing.T) {
	var calls atomic.Int32
	m, _ := New(Options{Store: &memoryStore{}, UIDs: func() []string { return []string{"active", "disabled"} }, Eligible: func(uid string, allow bool) bool { return uid != "disabled" || allow }, Run: func(context.Context, Target, Snapshot) (json.RawMessage, error) { calls.Add(1); return nil, nil }})
	defer m.Close()
	cfg := m.Settings()
	cfg.Automatic = false
	_ = m.Configure(cfg)
	m.Schedule("startup")
	if jobs, _ := m.List(); len(jobs) != 0 {
		t.Fatal("disabled policy scheduled jobs")
	}
	m.mu.Lock()
	for _, r := range Resources {
		tgt := Target{"active", r}
		m.snapshots[tgt.key()] = Snapshot{Target: tgt, State: "success", LastSuccess: time.Now().Unix()}
	}
	s := m.snapshots["active/balance"]
	s.LastSuccess = time.Now().Add(-time.Hour).Unix()
	m.snapshots["active/balance"] = s
	m.mu.Unlock()
	cfg.Automatic = true
	_ = m.Configure(cfg)
	m.Schedule("startup")
	jobs, _ := m.List()
	if len(jobs) != 1 {
		t.Fatal("missing due job")
	}
	job := waitJob(t, m, jobs[0].ID)
	if job.Total != 1 || calls.Load() != 1 || job.Items[0].Resource != "balance" {
		t.Fatalf("unexpected automatic targets: %+v", job)
	}
}
