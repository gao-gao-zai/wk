package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/haozhuma"
)

type monitorRoundTrip func(*http.Request) (*http.Response, error)

func (f monitorRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMonitorOrphanCleanupUsesOriginalIdentityAndProject(t *testing.T) {
	for _, gone := range []bool{false, true} {
		t.Run(fmt.Sprintf("already_gone=%v", gone), func(t *testing.T) {
			f := newFakeHZM(t)
			old := f.client()
			old.Token = "original-fixture-token"
			phone, err := old.GetPhone(context.Background(), "61903")
			if err != nil {
				t.Fatal(err)
			}
			seen := make(chan [2]string, 1)
			old.HTTP = &http.Client{Transport: monitorRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.URL.Query().Get("api") == "cancelRecv" {
					seen <- [2]string{r.URL.Query().Get("token"), r.URL.Query().Get("sid")}
					if gone {
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":"-1","msg":"释放失败"}`))}, nil
					}
				}
				return http.DefaultTransport.RoundTrip(r)
			})}
			a := NewAutoEnroller(nil, old, "52283", nil, nil, nil)
			ledger := filepath.Join(t.TempDir(), "held.json")
			a.SetLedgerPath(ledger)
			a.trackHeld(phone)
			replacement := haozhuma.New("new-fixture-token")
			err = a.prepareMonitorClient(context.Background(), func(context.Context) (*haozhuma.Client, error) {
				if a.Status().Held != 0 || a.CurrentSMSClient() != old {
					t.Fatal("client replaced before cleanup completed")
				}
				return replacement, nil
			})
			if err != nil || a.CurrentSMSClient() != replacement || a.Status().ConfigBusy {
				t.Fatalf("preparation failed: %v, status=%+v", err, a.Status())
			}
			if got := <-seen; got != [2]string{"original-fixture-token", "61903"} {
				t.Fatalf("cleanup used another identity or lost the original project: %v", got)
			}
			raw, err := os.ReadFile(ledger)
			if err != nil || string(raw) != "[]" || f.blacklist.Load() != 0 {
				t.Fatalf("cleanup did not persist safely: %q %v", raw, err)
			}
		})
	}
}

// Drive the real handler-installed preparer and watcher twice: a transient
// release failure must keep the ledger, then the next round must actually enroll.
func TestMonitorOrphanRetryResumesWatcher(t *testing.T) {
	e := newManualWatchEnv(t, map[string]string{"52283-CHEAP": "0.88|23"})
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"sms":{"haozhuma":{"token":"tok-1","sid":"52283"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	base := haozhumaBase
	haozhumaBase = e.fz.srv.URL + "/"
	t.Cleanup(func() { haozhumaBase = base })
	h := newTestHandler(t, Config{AutoEnroll: e.en, ConfigPath: path})
	h.SetUIDWatcher(e.w)
	e.en.trackHeld("17000000999")
	e.fz.releaseFail.Store(1)
	config := WatchConfig{Enabled: true, IntervalSeconds: 120, WantPerTrigger: 1, Projects: []WatchProject{e.proj(1, 1)}}
	e.w.Reconfigure(config)
	if _, err := e.w.AddBudget(10, nil); err != nil {
		t.Fatal(err)
	}
	e.w.tick(context.Background(), config)
	st := e.w.Status()
	if e.en.Status().Held != 1 || e.fz.getPhone.Load() != 0 || !strings.Contains(st.PausedReason, "1 个遗留号码未释放") || strings.Contains(st.PausedReason, "任务运行中") {
		t.Fatalf("failed cleanup lost the ledger or misreported a running task: %+v", st)
	}
	e.w.tick(context.Background(), config)
	st = e.w.Status()
	if st.EnrolledTotal != 1 || st.BudgetRemaining != 9.12 || st.PausedReason != "" || e.en.Status().Held != 0 {
		t.Fatalf("next round did not recover and enroll: %+v task=%+v", st, e.en.Status())
	}
	if e.fz.firstReleaseSeq.Load() >= e.fz.firstPhoneSeq.Load() {
		t.Fatal("new number was fetched before orphan cleanup")
	}
	raw, err := os.ReadFile(e.w.statePath)
	var saved watcherState
	if err != nil || json.Unmarshal(raw, &saved) != nil || saved.PausedReason != "" {
		t.Fatalf("stale pause persisted after recovery: %s %v", raw, err)
	}
}

func TestMonitorOrphanCleanupFencesConcurrentChangesAndCancellation(t *testing.T) {
	entered := make(chan struct{})
	finish := make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("api") != "cancelRecv" {
			t.Errorf("unexpected API during cleanup: %s", r.URL.Query().Get("api"))
		}
		close(entered)
		select {
		case <-finish:
		case <-r.Context().Done():
		}
		fmt.Fprint(w, `{"code":"0"}`)
	}))
	defer srv.Close()
	defer close(finish)
	old := haozhuma.New("original-fixture")
	old.Base = srv.URL
	a := NewAutoEnroller(nil, old, "52283", nil, nil, nil)
	a.SetLedgerPath(filepath.Join(t.TempDir(), "held.json"))
	a.trackHeld("17000000999")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	var builds atomic.Int32
	build := func(context.Context) (*haozhuma.Client, error) {
		builds.Add(1)
		return haozhuma.New("new-fixture"), nil
	}
	go func() { result <- a.prepareMonitorClient(ctx, build) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup did not contact the original provider")
	}
	if st := a.Status(); !st.ConfigBusy || st.Running {
		t.Fatalf("preparation status is incorrect: %+v", st)
	}
	for _, source := range []string{"manual", "monitor"} {
		if err := a.AutoRunWith(AutoRunOptions{Want: 1, Source: source}); err == nil {
			t.Fatal("task entered during cleanup")
		}
	}
	written := false
	newClient := haozhuma.New("new-fixture")
	if err := a.ApplySMSUpdate(newClient, nil, func() error { written = true; return nil }); !errors.Is(err, ErrBusyAuth) || written {
		t.Fatal("credential update entered cleanup")
	}
	if err := a.ReplaceClient(newClient); !errors.Is(err, ErrBusyAuth) {
		t.Fatal("legacy client replacement entered cleanup")
	}
	if err := a.applyFetchConfig(haozhuma.Settings{Sid: "61903"}, func() error { written = true; return nil }); !errors.Is(err, ErrBusyAuth) || written {
		t.Fatal("fetch settings entered cleanup")
	}
	if _, err := a.borrowMonitorFetch("61903", "61903-OTHER"); !errors.Is(err, ErrBusyAuth) {
		t.Fatal("another monitor borrowed parameters during cleanup")
	}
	if _, _, err := a.ReleaseAllHeld(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatal("account-wide release entered cleanup")
	}
	if err := a.prepareMonitorClient(context.Background(), build); !errors.Is(err, ErrBusyAuth) {
		t.Fatal("another preparation entered cleanup")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup ignored cancellation")
	}
	if builds.Load() != 0 || calls.Load() != 1 || a.CurrentSMSClient() != old || a.Status().Held != 1 || a.Status().ConfigBusy {
		t.Fatalf("canceled preparation changed identity or lost the ledger: %+v", a.Status())
	}
}

func TestMonitorOrphanBuildFailurePreservesOriginalClient(t *testing.T) {
	f := newFakeHZM(t)
	old := f.client()
	a := NewAutoEnroller(nil, old, "52283", nil, nil, nil)
	a.trackHeld("17000000999")
	err := a.prepareMonitorClient(context.Background(), func(context.Context) (*haozhuma.Client, error) {
		return nil, errors.New("fixture login failure")
	})
	if err == nil || a.CurrentSMSClient() != old || a.Status().Held != 0 || a.Status().ConfigBusy {
		t.Fatalf("build failure corrupted cleanup or identity: %v %+v", err, a.Status())
	}
	// The existing safeguards must still reject a real credential change when
	// an orphan remains, with a reason that names the actual blocker.
	a.trackHeld("17000000999")
	err = a.ApplySMSUpdate(haozhuma.New("other-fixture"), nil, func() error { t.Fatal("blocked update persisted"); return nil })
	if !errors.Is(err, ErrBusyAuth) || !strings.Contains(err.Error(), "1 个遗留号码") || strings.Contains(err.Error(), "任务运行中") {
		t.Fatalf("misleading update error: %v", err)
	}
}
