package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/coordination"
)

func TestSharedPoolJSONBRouteLeasesAcross41Accounts(t *testing.T) {
	p1, p2, store, _ := sharedTestPools(t)
	ctx := context.Background()
	// Confirm the production failure mechanism without corrupting pool/runtime:
	// encoding/json accepts a NUL key, but PostgreSQL JSONB rejects it as 22P05.
	legacyKey := "tenant" + "\x00" + "session" + "\x00" + "prefix"
	legacy, err := json.Marshal(map[string]any{"Routes": map[string]any{"A": map[string]any{legacyKey: sharedRoute{Active: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	err = store.Update(ctx, "pool/legacy-nul-route-regression", func(json.RawMessage) (json.RawMessage, error) { return legacy, nil })
	var pgError interface{ SQLState() string }
	if !errors.As(err, &pgError) || pgError.SQLState() != "22P05" {
		t.Fatalf("legacy NUL route key did not reproduce SQLSTATE 22P05: %v", err)
	}

	dir := t.TempDir()
	accounts := make([]*auth.Auth, 0, 41)
	for i := 0; i < 41; i++ {
		uid := "A"
		if i != 0 {
			uid = fmt.Sprintf("offline-%02d", i)
		}
		path := filepath.Join(dir, "workbuddy-"+uid+".json")
		raw, _ := json.Marshal(map[string]any{"uid": uid, "accessToken": "offline-test-only", "expiresAt": int64(9999999999)})
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		a, err := auth.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		a.FilePath = path
		accounts = append(accounts, a)
		p1.Add(a)
		p1.SetCredits(uid, 1000)
	}
	if err := p2.AttachSharedAuths(accounts); err != nil {
		t.Fatal(err)
	}
	p1.ConfigureAdmission(AdmissionConfig{Enabled: true, FairQueue: true, LeaseMin: time.Hour, LeaseMax: time.Hour})
	for _, a := range accounts {
		// Real routing supplies printable hex digests in these three fields.
		first := RouteRequest{Tenant: fmt.Sprintf("%064x", 1), Session: fmt.Sprintf("%064x", 2), Prefix: fmt.Sprintf("%064x", 3)}
		second := first
		second.Session = fmt.Sprintf("%064x", 4)
		_, oldLease, err := p1.AcquireShared("m", first, nil, nil, a.UID, true)
		if err != nil || oldLease.ID == "" {
			t.Fatalf("old acquire for %s: %v", a.UID, err)
		}
		_, newLease, err := p2.AcquireShared("m", second, nil, nil, a.UID, true)
		if err != nil || newLease.ID == "" {
			t.Fatalf("new acquire for %s: %v", a.UID, err)
		}
		state, ok := p1.Status(a.UID)
		active, _ := p2.RouteActivity(a.UID)
		if !ok || state.InFlight != 2 || active != 2 {
			t.Fatalf("overlapping routes for %s: in_flight=%d active=%d", a.UID, state.InFlight, active)
		}
		if got, _, err := p1.AcquireShared("m", first, nil, nil, a.UID, true); err != nil || got != nil {
			t.Fatalf("overlap capacity exceeded for %s: %v", a.UID, err)
		}
		p1.NoteRouteSuccess(a.UID, first, time.Second)
		p2.NoteRouteSuccess(a.UID, second, time.Second)
		if err := p2.ReleaseLease(oldLease); err == nil {
			t.Fatal("candidate released the old owner's route lease")
		}
		if err := p1.ReleaseLease(oldLease); err != nil {
			t.Fatal(err)
		}
		state, _ = p2.Status(a.UID)
		active, _ = p1.RouteActivity(a.UID)
		if state.InFlight != 1 || active != 1 {
			t.Fatalf("old completion lost the new lease for %s: in_flight=%d active=%d", a.UID, state.InFlight, active)
		}
		if err := p2.ReleaseLease(newLease); err != nil {
			t.Fatal(err)
		}
		state, _ = p1.Status(a.UID)
		active, _ = p2.RouteActivity(a.UID)
		if state.InFlight != 0 || active != 0 {
			t.Fatalf("route lease leaked for %s: in_flight=%d active=%d", a.UID, state.InFlight, active)
		}
	}
	for _, p := range []*Pool{p1, p2} {
		if err := p.FlushWithError(); err != nil {
			t.Fatal(err)
		}
		if len(p.List()) != 41 || p.SharedError() != nil {
			t.Fatal("route traffic fenced or emptied the 41-account pool")
		}
	}
	raw, err := store.View(ctx, "pool/runtime")
	if err != nil {
		t.Fatal(err)
	}
	var state sharedState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Accounts) != 41 || len(state.Leases) != 0 || strings.Contains(string(raw), `\u0000`) {
		t.Fatal("persisted runtime lost accounts, retained requests or contains a NUL escape")
	}
	for _, a := range accounts {
		if len(state.Routes[a.UID]) != 2 {
			t.Fatalf("route session identities collided for %s", a.UID)
		}
	}
}

func sharedTestPools(t *testing.T) (*Pool, *Pool, *coordination.Store, func(string, bool)) {
	t.Helper()
	dsn := os.Getenv("WB2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("WB2API_TEST_POSTGRES_DSN is required for PostgreSQL overlap tests")
	}
	domain := fmt.Sprintf("pool-test/%s/%d", t.Name(), time.Now().UnixNano())
	s1, err := coordination.OpenPostgres(context.Background(), dsn, domain)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s1.Close() })
	s2, err := coordination.OpenPostgres(context.Background(), dsn, domain)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	var mu sync.Mutex
	owners := map[string]bool{"old": true, "new": true}
	alive := func(id string) (bool, error) { mu.Lock(); defer mu.Unlock(); return owners[id], nil }
	set := func(id string, v bool) { mu.Lock(); owners[id] = v; mu.Unlock() }
	a := &auth.Auth{UID: "A", FilePath: "offline-account-A.json"}
	p1 := modelTestPool(a)
	p1.SetMaxInFlight(2)
	if err = p1.EnableShared(context.Background(), s1, "old", alive, true); err != nil {
		t.Fatal(err)
	}
	p2 := New("")
	if err = p2.EnableShared(context.Background(), s2, "new", alive); err != nil {
		t.Fatal(err)
	}
	if err = p2.AttachSharedAuths([]*auth.Auth{a}); err != nil {
		t.Fatal(err)
	}
	return p1, p2, s1, set
}

func TestSharedPoolGlobalSlotsLeaseIdentityAndOwnerProof(t *testing.T) {
	p1, p2, _, set := sharedTestPools(t)
	r1 := RouteRequest{Tenant: "one", Session: "one"}
	r2 := RouteRequest{Tenant: "two", Session: "two"}
	a, l1, err := p1.AcquireShared("m", r1, nil, nil, "A", true)
	if err != nil || a == nil {
		t.Fatalf("old acquire: %v", err)
	}
	a, l2, err := p2.AcquireShared("m", r2, nil, nil, "A", true)
	if err != nil || a == nil {
		t.Fatalf("new acquire: %v", err)
	}
	if l1.ID == l2.ID {
		t.Fatal("requests shared a lease identity")
	}
	if a, _, err = p2.AcquireShared("m", r2, nil, nil, "A", true); err != nil || a != nil {
		t.Fatalf("global capacity exceeded: %v", err)
	}
	if err = p2.ReleaseLease(l1); err == nil {
		t.Fatal("different owner released an old stream")
	}
	if err = p1.ReleaseLease(l1); err != nil {
		t.Fatal(err)
	}
	if err = p1.ReleaseLease(l1); err != nil {
		t.Fatal("release was not idempotent", err)
	}
	state, _ := p2.Status("A")
	if state.InFlight != 1 {
		t.Fatalf("duplicate release lost another request: %d", state.InFlight)
	}
	set("new", false) // a real deployment supplies this only after local flock proof
	state, _ = p1.Status("A")
	if state.InFlight != 0 {
		t.Fatalf("dead owner reservation not reclaimed: %d", state.InFlight)
	}
}

func TestSharedPoolMutationsAndStaleFlush(t *testing.T) {
	p1, p2, _, _ := sharedTestPools(t)
	p1.SpendCredits("A", 123)
	p2.SpendCredits("A", 7)
	if got, _ := p1.EffectiveCredits("A"); got != 870 {
		t.Fatalf("shadow credit update lost: %d", got)
	}
	p2.Disable("A", "offline-test")
	if err := p1.FlushWithError(); err != nil {
		t.Fatal(err)
	}
	if p1.HealthyForModel("A", "m") {
		t.Fatal("old flush overwrote disable")
	}
	if err := p1.SharedError(); err != nil {
		t.Fatal(err)
	}
}

func TestSharedPoolAuthorityFailureClosesAdmission(t *testing.T) {
	p1, _, store, _ := sharedTestPools(t)
	_ = store.Close()
	if a, _, err := p1.AcquireShared("m", RouteRequest{}, nil, nil, "A", false); err == nil || a != nil {
		t.Fatal("authority error admitted upstream work")
	}
	p1.SetCredits("A", 999999)
	if a, _, err := p1.AcquireShared("m", RouteRequest{}, nil, nil, "A", false); err == nil || a != nil {
		t.Fatal("local mutation bypassed authority fence")
	}
}

func TestSharedPoolQueueCrossVersionAndCancellation(t *testing.T) {
	p1, p2, _, _ := sharedTestPools(t)
	p1.SetMaxInFlight(1)
	a, held, err := p1.AcquireShared("m", RouteRequest{}, nil, nil, "A", false)
	if err != nil || a == nil {
		t.Fatal("initial acquire", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	type result struct {
		a   *auth.Auth
		l   Lease
		err error
	}
	done := make(chan result, 1)
	go func() {
		a, l, err := p2.WaitAcquireShared(ctx, "A", "m", 1, 1, RouteRequest{Tenant: "t", Session: "s"}, nil, nil)
		done <- result{a, l, err}
	}()
	awaitQueue(t, p1, 1)
	if _, _, err = p1.WaitAcquireShared(ctx, "A", "m", 1, 1, RouteRequest{}, nil, nil); err != ErrWaitQueueFull {
		t.Fatalf("global pending cap ignored: %v", err)
	}
	if err = p1.ReleaseLease(held); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.a == nil {
		t.Fatalf("new version did not receive released slot: %v", r.err)
	}
	if err = p2.ReleaseLease(r.l); err != nil {
		t.Fatal(err)
	}
	awaitQueue(t, p1, 0)
}

func TestSharedPoolDiscoversEnrolledCredentialWithoutRevivingDeletion(t *testing.T) {
	p1, p2, store, _ := sharedTestPools(t)
	path := filepath.Join(t.TempDir(), "workbuddy-new.json")
	raw := []byte(`{"uid":"new","accessToken":"offline-new-token","expiresAt":9999999999}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	a.FilePath = path
	p1.Add(a)
	p1.SetCredits("new", 1000)
	// Candidate was constructed before enrollment and never AttachSharedAuths
	// this account; its next transaction must discover the shared file.
	got, lease, err := p2.AcquireShared("m", RouteRequest{}, nil, nil, "new", false)
	if err != nil || got == nil || got.Snapshot().AccessToken != "offline-new-token" || got.Snapshot().FilePath != path {
		t.Fatalf("candidate did not load enrolled file: account=%t err=%v", got != nil, err)
	}
	if err = p2.ReleaseLease(lease); err != nil {
		t.Fatal(err)
	}
	state, err := store.View(context.Background(), "pool/runtime")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(state), "offline-new-token") {
		t.Fatal("credential contents leaked into coordination state")
	}
	removed, busy := p1.Remove("new")
	if !removed || busy {
		t.Fatalf("remove failed: removed=%t busy=%t", removed, busy)
	}
	if err = p2.FlushWithError(); err != nil {
		t.Fatal(err)
	}
	if got, _, err = p2.AcquireShared("m", RouteRequest{}, nil, nil, "new", false); err != nil || got != nil {
		t.Fatalf("stale candidate revived deleted account: %v", err)
	}
	if _, exists := p1.Status("new"); exists {
		t.Fatal("deleted account reappeared after stale flush")
	}
}

func TestSharedPoolUnreadableEnrollmentRemainsExcludedAndKeepsPath(t *testing.T) {
	p1, p2, store, _ := sharedTestPools(t)
	path := filepath.Join(t.TempDir(), "workbuddy-pending.json")
	p1.Add(&auth.Auth{UID: "pending", FilePath: path})
	p1.SetCredits("pending", 1000)
	if got, _, err := p2.AcquireShared("m", RouteRequest{}, nil, nil, "pending", false); err != nil || got != nil {
		t.Fatalf("unreadable credential was admitted: %v", err)
	}
	// Reads/flushes from the candidate must retain the authoritative path so
	// the same process can recover when the file becomes readable.
	if err := p2.FlushWithError(); err != nil {
		t.Fatal(err)
	}
	state, err := store.View(context.Background(), "pool/runtime")
	if err != nil {
		t.Fatal(err)
	}
	var shared sharedState
	if err = json.Unmarshal(state, &shared); err != nil {
		t.Fatal(err)
	}
	if shared.Accounts["pending"].CredentialPath != path {
		t.Fatal("placeholder cleared credential metadata")
	}
	if err = os.WriteFile(path, []byte(`{"uid":"wrong","accessToken":"offline-test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got, _, err := p2.AcquireShared("m", RouteRequest{}, nil, nil, "pending", false); err != nil || got != nil {
		t.Fatalf("UID-mismatched file was admitted: %v", err)
	}
	if err = os.WriteFile(path, []byte(`{"uid":"pending","accessToken":"offline-test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	got, lease, err := p2.AcquireShared("m", RouteRequest{}, nil, nil, "pending", false)
	if err != nil || got == nil {
		t.Fatalf("readable file did not recover: %v", err)
	}
	if err = p2.ReleaseLease(lease); err != nil {
		t.Fatal(err)
	}
}
