package pool

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestRouteLeaseKeyPreservesPrintableFieldBoundaries(t *testing.T) {
	routes := []RouteRequest{
		{},
		{Tenant: "a", Session: "bc", Prefix: "d"},
		{Tenant: "ab", Session: "c", Prefix: "d"},
		{Tenant: "a", Session: "b", Prefix: "cd"},
		{Tenant: "a.b", Session: "c", Prefix: "d"},
		{Tenant: "a", Session: "b.c", Prefix: "d"},
		{Tenant: "a\x00b", Session: "c", Prefix: "d"},
		{Tenant: "a", Session: "b\x00c", Prefix: "d"},
		{Tenant: "", Session: "\x00", Prefix: ""},
		{Tenant: "\x00", Session: "", Prefix: ""},
		{Tenant: "租户", Session: "session/-_", Prefix: "\xff\x00"},
	}
	seen := map[string]bool{}
	for _, route := range routes {
		key := routeLeaseKey(route)
		if seen[key] {
			t.Fatalf("distinct route identities share a key: %#v", route)
		}
		seen[key] = true
		for _, ch := range key {
			if ch < 0x20 || ch > 0x7e {
				t.Fatalf("route key contains a nonprintable character: %q", key)
			}
		}
		parts := strings.Split(key, ".")
		if len(parts) != 3 {
			t.Fatalf("route key lost field boundaries: %q", key)
		}
		for i, want := range []string{route.Tenant, route.Session, route.Prefix} {
			got, err := base64.RawURLEncoding.DecodeString(parts[i])
			if err != nil || string(got) != want {
				t.Fatalf("field %d round trip failed: %v", i, err)
			}
		}
		route.Related = true
		if routeLeaseKey(route) != key {
			t.Fatal("request-local related hint changed route identity")
		}
	}
}

func admissionPool(uids ...string) *Pool {
	var accounts []*auth.Auth
	for _, uid := range uids {
		accounts = append(accounts, &auth.Auth{UID: uid})
	}
	p := modelTestPool(accounts...)
	p.SetMaxInFlight(3)
	p.ConfigureAdmission(AdmissionConfig{Enabled: true, FairQueue: true, ReservedSlots: 1})
	return p
}
func routeRequest(session string) RouteRequest {
	return RouteRequest{Tenant: "tenant", Session: session, Prefix: "prefix-" + session}
}
func warmRoute(t *testing.T, p *Pool, uid string, r RouteRequest) {
	t.Helper()
	if p.PickAndAcquireByUIDForRoute(uid, "m", r, nil, nil) == nil {
		t.Fatal("warm route could not acquire")
	}
	p.NoteRouteSuccess(uid, r, time.Second)
	p.ReleaseRoute(uid, r)
}

func TestAdmissionProtectsSlotFromOtherSessions(t *testing.T) {
	p := admissionPool("A", "B")
	old := routeRequest("old")
	warmRoute(t, p, "A", old)
	for _, id := range []string{"s2", "s3"} {
		r := routeRequest(id)
		if p.PickAndAcquireByUIDForRoute("A", "m", r, nil, nil) == nil {
			t.Fatal("unused nonreserved slot rejected")
		}
		defer p.ReleaseRoute("A", r)
	}
	fresh := routeRequest("s4")
	if a := p.PickAndAcquireByUIDForRoute("A", "m", fresh, nil, nil); a != nil {
		p.ReleaseRoute("A", fresh)
		t.Fatal("new session took reserved slot")
	}
	if _, err := p.WaitAndAcquireByUIDForRoute(context.Background(), "A", "m", 8, 64, fresh, nil, nil); !errors.Is(err, ErrRouteReserved) {
		t.Fatalf("queue bypassed admission: %v", err)
	}
	a := p.PickAndAcquireRoute("m", fresh, nil, nil)
	if a == nil || a.UID != "B" {
		t.Fatal("new session did not go to available alternative")
	}
	p.ReleaseRoute("B", fresh)
	if p.PickAndAcquireByUIDForRoute("A", "m", old, nil, nil) == nil {
		t.Fatal("returning session lost its slot")
	}
	p.ReleaseRoute("A", old)
}

func TestAdmissionReservationIsBorrowableAndScoped(t *testing.T) {
	for _, restricted := range []bool{false, true} {
		t.Run(map[bool]string{false: "single_account", true: "scope"}[restricted], func(t *testing.T) {
			p := admissionPool("A")
			if restricted {
				p.Add(&auth.Auth{UID: "B", ExpiresAt: 9999999999})
				p.SetCredits("B", 100)
			}
			warmRoute(t, p, "A", routeRequest("old"))
			allow := map[string]bool{"A": true}
			for _, id := range []string{"s2", "s3", "s4"} {
				r := routeRequest(id)
				if p.PickAndAcquireByUIDForRoute("A", "m", r, nil, allow) == nil {
					t.Fatal("soft reservation wasted authorized capacity")
				}
				defer p.ReleaseRoute("A", r)
			}
			state, _ := p.Status("A")
			if state.InFlight != 3 {
				t.Fatal(state.InFlight)
			}
		})
	}
}

func TestAdmissionLeaseExpiryAndRunningProtection(t *testing.T) {
	p := admissionPool("A", "B")
	old := routeRequest("old")
	warmRoute(t, p, "A", old)
	if p.PickAndAcquireByUIDForRoute("A", "m", old, nil, nil) == nil {
		t.Fatal("return failed")
	}
	p.mu.Lock()
	p.routeLeases["A"][routeLeaseKey(old)].expires = time.Now().Add(-time.Second)
	p.mu.Unlock()
	other := routeRequest("other")
	if p.PickAndAcquireByUIDForRoute("A", "m", other, nil, nil) == nil {
		t.Fatal("second slot rejected")
	}
	defer p.ReleaseRoute("A", other)
	fresh := routeRequest("fresh")
	if p.PickAndAcquireByUIDForRoute("A", "m", fresh, nil, nil) != nil {
		t.Fatal("running hot session lost protection at TTL")
	}
	// Failure/release does not renew grace after its TTL has expired.
	p.ReleaseRoute("A", old)
	_, hot := p.RouteActivity("A")
	if hot != 0 {
		t.Fatal("failure renewed protection")
	}
	if p.PickAndAcquireByUIDForRoute("A", "m", fresh, nil, nil) == nil {
		t.Fatal("expired protection still denied slot")
	}
	p.ReleaseRoute("A", fresh)
}

func TestAdmissionIdentityAndPrefixDoNotGrantFalsePriority(t *testing.T) {
	p := admissionPool("A", "B")
	old := routeRequest("old")
	warmRoute(t, p, "A", old)
	for _, id := range []string{"s2", "s3"} {
		r := routeRequest(id)
		p.PickAndAcquireByUIDForRoute("A", "m", r, nil, nil)
		defer p.ReleaseRoute("A", r)
	}
	for _, mutation := range []string{"prefix", "tenant"} {
		r := old
		if mutation == "prefix" {
			r.Prefix = "unrelated"
		} else {
			r.Tenant = "other-tenant"
		}
		if p.PickAndAcquireByUIDForRoute("A", "m", r, nil, nil) != nil {
			t.Fatal("unrelated request claimed old priority: " + mutation)
		}
	}
}

func TestAdmissionAtomicConcurrentColdRequests(t *testing.T) {
	p := admissionPool("A", "B")
	warmRoute(t, p, "A", routeRequest("old"))
	for _, id := range []string{"s2", "s3"} {
		r := routeRequest(id)
		p.PickAndAcquireByUIDForRoute("A", "m", r, nil, nil)
		defer p.ReleaseRoute("A", r)
	}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := routeRequest("fresh")
			if p.PickAndAcquireByUIDForRoute("A", "m", r, nil, nil) != nil {
				p.ReleaseRoute("A", r)
				t.Error("race consumed reserved slot")
			}
		}()
	}
	wg.Wait()
	state, _ := p.Status("A")
	if state.InFlight != 2 {
		t.Fatal("capacity drift", state.InFlight)
	}
}

func TestAdmissionTrackingBoundAndAdaptiveLease(t *testing.T) {
	p := admissionPool("A")
	p.ConfigureAdmission(AdmissionConfig{Enabled: true, ReservedSlots: 1, MaxSessions: 2, LeaseMin: time.Second, LeaseMax: 3 * time.Second})
	for _, id := range []string{"s1", "s2", "s3", "s4"} {
		warmRoute(t, p, "A", routeRequest(id))
	}
	p.mu.Lock()
	if p.leaseCount != 2 {
		t.Fatal("unbounded lease tracking", p.leaseCount)
	}
	r := routeRequest("s4")
	l := p.routeLeases["A"][routeLeaseKey(r)]
	l.lastSuccess = time.Now().Add(-10 * time.Second)
	p.mu.Unlock()
	p.PickAndAcquireByUIDForRoute("A", "m", r, nil, nil)
	p.NoteRouteSuccess("A", r, time.Second)
	p.ReleaseRoute("A", r)
	p.mu.RLock()
	ttl := p.routeLeases["A"][routeLeaseKey(r)].expires.Sub(p.routeLeases["A"][routeLeaseKey(r)].lastSuccess)
	p.mu.RUnlock()
	if ttl != 3*time.Second {
		t.Fatal("adaptive lease did not clamp", ttl)
	}
}

func TestAdmissionUsesServiceTimeAndDoesNotCrossScope(t *testing.T) {
	p := admissionPool("A", "B")
	p.mu.Lock()
	p.serviceTimes["A"] = 30 * time.Second
	p.serviceTimes["B"] = time.Second
	p.mu.Unlock()
	p.Acquire("A")
	defer p.Release("A")
	p.Acquire("B")
	p.Acquire("B")
	defer p.Release("B")
	defer p.Release("B")
	if uid := p.RecommendRoute("m", routeRequest("new"), nil, nil); uid != "B" {
		t.Fatal("ignored expected occupancy", uid)
	}
	if uid := p.RecommendRoute("m", routeRequest("new"), nil, map[string]bool{"A": true}); uid != "A" {
		t.Fatal("scope escaped", uid)
	}
}

func TestAdmissionForgetsRemovedAccount(t *testing.T) {
	for _, syncDir := range []bool{false, true} {
		p := admissionPool("A")
		warmRoute(t, p, "A", routeRequest("old"))
		if syncDir {
			p.SyncToDir(nil)
		} else if removed, busy := p.Remove("A"); !removed || busy {
			t.Fatal("idle account was not removed")
		}
		p.mu.RLock()
		count, leases, timings := p.leaseCount, len(p.routeLeases), len(p.serviceTimes)
		p.mu.RUnlock()
		if count != 0 || leases != 0 || timings != 0 {
			t.Fatal("removed account left admission state", count, leases, timings)
		}
	}
}

func TestFairQueueRotatesTenantsAndSessions(t *testing.T) {
	p := admissionPool("A")
	p.SetMaxInFlight(1)
	p.Acquire("A")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ids := []string{"a1", "a1", "a2", "b1", "b2"}
	got := make(chan string, len(ids))
	requests := make([]RouteRequest, len(ids))
	for i, id := range ids {
		r := routeRequest(id)
		if id[0] == 'b' {
			r.Tenant = "tenant-b"
		}
		requests[i] = r
		go func(r RouteRequest) {
			_, err := p.WaitAndAcquireByUIDForRoute(ctx, "A", "m", 8, 64, r, nil, nil)
			if err != nil {
				got <- "error"
			} else {
				got <- r.Session
			}
		}(r)
		awaitQueue(t, p, i+1)
	}
	p.Release("A")
	want := []string{"a1", "b1", "a2", "b2", "a1"}
	for _, id := range want {
		select {
		case seen := <-got:
			if seen != id {
				t.Fatalf("queue order got %s want %s", seen, id)
			}
			r := routeRequest(seen)
			if seen[0] == 'b' {
				r.Tenant = "tenant-b"
			}
			p.ReleaseRoute("A", r)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	awaitQueue(t, p, 0)
}

func TestFairQueueBoundsHotPriorityAndAgesColdWaiters(t *testing.T) {
	for _, aged := range []bool{false, true} {
		t.Run(map[bool]string{false: "hot_priority_budget", true: "aged_cold"}[aged], func(t *testing.T) {
			p := admissionPool("A")
			p.SetMaxInFlight(1)
			p.Acquire("A")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			got := make(chan RouteRequest, 4)
			for i, id := range []string{"cold", "hot1", "hot2", "hot3"} {
				r := routeRequest(id)
				r.Related = i > 0
				go func(r RouteRequest) {
					_, err := p.WaitAndAcquireByUIDForRoute(ctx, "A", "m", 8, 64, r, nil, nil)
					if err == nil {
						got <- r
					}
				}(r)
				awaitQueue(t, p, i+1)
			}
			if aged {
				p.mu.Lock()
				p.waiters["A"][0].enqueued = time.Now().Add(-3 * time.Second)
				p.mu.Unlock()
			}
			p.Release("A")
			want := []string{"hot1", "hot2", "cold", "hot3"}
			if aged {
				want = []string{"cold", "hot1", "hot2", "hot3"}
			}
			for _, id := range want {
				select {
				case r := <-got:
					if r.Session != id {
						t.Fatalf("got %s want %s", r.Session, id)
					}
					p.ReleaseRoute("A", r)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			awaitQueue(t, p, 0)
		})
	}
}

func TestFairQueueLimitsSingleSessionBacklog(t *testing.T) {
	p := admissionPool("A")
	p.SetMaxInFlight(1)
	p.ConfigureAdmission(AdmissionConfig{FairQueue: true, MaxPendingPerSession: 2})
	p.Acquire("A")
	defer p.Release("A")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 4)
	queue := func(r RouteRequest, size int) {
		go func() {
			_, err := p.WaitAndAcquireByUIDForRoute(ctx, "A", "m", 8, 64, r, nil, nil)
			finished <- err
		}()
		awaitQueue(t, p, size)
	}
	same := routeRequest("same")
	queue(same, 1)
	queue(same, 2)
	if _, err := p.WaitAndAcquireByUIDForRoute(ctx, "A", "m", 8, 64, same, nil, nil); !errors.Is(err, ErrSessionQueueFull) {
		t.Fatalf("single session exceeded its waiting budget: %v", err)
	}
	queue(routeRequest("different"), 3)
	otherTenant := same
	otherTenant.Tenant = "other-tenant"
	queue(otherTenant, 4)
	cancel()
	for i := 0; i < 4; i++ {
		select {
		case err := <-finished:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("unexpected queued result: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("canceled waiter did not leave queue")
		}
	}
	awaitQueue(t, p, 0)
}
