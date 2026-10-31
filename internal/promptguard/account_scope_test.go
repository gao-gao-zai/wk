package promptguard

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestScopedCaptureDedupAndEncryptedPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guard.enc")
	m, err := Open(path, "fixture-key")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	scope := AccountScope{Kind: "group", Group: "a", Accounts: []string{"u2", "u1", "u1"}}
	id, err := m.CaptureScoped("m", "u1", messages("original"), messages("outbound"), scope)
	if err != nil {
		t.Fatal(err)
	}
	scope.Accounts = []string{"u3", "u2"}
	id2, _ := m.CaptureScoped("m", "u2", messages("original"), messages("outbound"), scope)
	if id2 != id {
		t.Fatal("member snapshot prevented dedup within same group")
	}
	scope.Group = "b"
	different, _ := m.CaptureScoped("m", "u2", messages("original"), messages("outbound"), scope)
	if different == id {
		t.Fatal("different groups were merged")
	}
	legacy, _ := m.Capture("m", "u2", messages("original"), messages("outbound"))
	if legacy == id {
		t.Fatal("legacy scope was widened by merging")
	}
	reopened, err := Open(path, "fixture-key")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	item, _ := reopened.Get(id)
	if item.AccountScope == nil || !reflect.DeepEqual(item.AccountScope.Accounts, []string{"u2", "u3"}) || item.Occurrences != 2 {
		t.Fatalf("scope not persisted: %+v", item)
	}
	for _, entry := range reopened.List() {
		if entry.AccountScope != nil && len(entry.AccountScope.Accounts) > 0 {
			t.Fatal("list includes full member snapshot")
		}
	}
	_, err = m.StartWithAccountScope(id, "verify", AccountScope{Kind: "all"})
	if err == nil {
		t.Fatal("known scope allowed override")
	}
}

func TestSessionLeaseReleasedOnCompletionAndPreparationFailure(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "preparation_failure"}[failure], func(t *testing.T) {
			m := memory(t)
			var acquired, released atomic.Int32
			m.SetSessionFactory(func(context.Context, Case) ProbeSession {
				acquired.Add(1)
				return ProbeSession{Selection: AccountSelection{UID: "selected", Reason: "weighted_available", ScopeKind: "all"}, Close: func() { released.Add(1) }, Probe: func(context.Context, []Message, bool) ProbeResult { return ProbeResult{Outcome: "pass", HTTP: 200} }}
			})
			if failure {
				m.SetPreparer(func(Case) ([]Message, error) { return nil, errors.New("fixture") })
			}
			id, _ := m.Capture("m", "original", messages("text"), messages("text"))
			kind := "verify"
			if failure {
				kind = "locate"
			}
			_, err := m.StartWithAccountScope(id, kind, AccountScope{Kind: "all"})
			if err != nil {
				t.Fatal(err)
			}
			item := waitJob(t, m, id)
			if acquired.Load() != 1 || released.Load() != 1 || item.Job.Selection == nil || item.Job.Selection.UID != "selected" {
				t.Fatalf("lease metadata/cleanup failed: %+v", item.Job)
			}
			if item.AccountScope != nil {
				t.Fatal("manual legacy override changed original provenance")
			}
		})
	}
}

func TestSessionLeaseReleasedOnCancellation(t *testing.T) {
	m := memory(t)
	entered := make(chan struct{})
	var released atomic.Int32
	m.SetSessionFactory(func(context.Context, Case) ProbeSession {
		return ProbeSession{Close: func() { released.Add(1) }, Probe: func(ctx context.Context, _ []Message, _ bool) ProbeResult {
			close(entered)
			<-ctx.Done()
			return ProbeResult{Outcome: "inconclusive"}
		}}
	})
	id, _ := m.Capture("m", "original", messages("text"), messages("text"))
	_, _ = m.Start(id, "verify", false)
	<-entered
	_ = m.Cancel(id)
	item := waitJob(t, m, id)
	if item.Job.Status != "cancelled" || released.Load() != 1 {
		t.Fatal("cancel did not release lease")
	}
}
