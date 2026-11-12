package reqproxy

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"workbuddy2api/internal/growthlimit"
)

func TestFreezeGrowthNodeResolvesAllProxyConnectionIPs(t *testing.T) {
	original := NodeSpec{Protocol: "vless", Spec: map[string]any{"host": "proxy.fixture.invalid", "port": 443, "tls": true, "network": "ws"}}
	called := 0
	lookup := func(ctx context.Context, host string) ([]net.IPAddr, error) {
		called++
		if host != "proxy.fixture.invalid" {
			t.Fatalf("unexpected DNS host %s", host)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 2*time.Second {
			t.Error("DNS lookup is not bounded")
		}
		return []net.IPAddr{{IP: net.ParseIP("192.0.2.2")}, {IP: net.ParseIP("::ffff:192.0.2.1")}, {IP: net.ParseIP("192.0.2.2")}}, nil
	}
	frozen, ips := freezeGrowthNode(context.Background(), original, lookup)
	if called != 1 || !reflect.DeepEqual(ips, []string{"192.0.2.1", "192.0.2.2"}) {
		t.Fatalf("DNS resources %v calls=%d", ips, called)
	}
	if frozen.Spec["host"] != "192.0.2.1" || frozen.Spec["serverName"] != "proxy.fixture.invalid" || frozen.Spec["wsHost"] != "proxy.fixture.invalid" {
		t.Fatalf("private node host/SNI/authority not pinned correctly")
	}
	if original.Spec["host"] != "proxy.fixture.invalid" || original.Spec["serverName"] != nil {
		t.Fatal("shared proxy node mutated")
	}
	// Neither port nor node ID creates a different resource for the same IP.
	frozen.ID = "different-node"
	frozen.Spec["port"] = 8443
	_, sameIPs := freezeGrowthNode(context.Background(), frozen, func(context.Context, string) ([]net.IPAddr, error) {
		t.Fatal("literal IP invoked DNS")
		return nil, nil
	})
	if !reflect.DeepEqual(sameIPs, []string{"192.0.2.1"}) {
		t.Fatal(sameIPs)
	}
}

func TestFreezeGrowthNodeUnknownDNSIsExclusive(t *testing.T) {
	node := NodeSpec{Spec: map[string]any{"host": "unresolved.fixture.invalid", "port": 443}}
	for _, lookup := range []func(context.Context, string) ([]net.IPAddr, error){
		func(context.Context, string) ([]net.IPAddr, error) { return nil, errors.New("fixture DNS error") },
		func(context.Context, string) ([]net.IPAddr, error) { return make([]net.IPAddr, 33), nil },
		func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("fe80::1"), Zone: "eth0"}}, nil
		},
	} {
		frozen, ips := freezeGrowthNode(context.Background(), node, lookup)
		if len(ips) != 0 || frozen.Spec["host"] != node.Spec["host"] {
			t.Fatal("unknown DNS must remain globally exclusive")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, ips := freezeGrowthNode(ctx, node, func(ctx context.Context, _ string) ([]net.IPAddr, error) { <-ctx.Done(); return nil, ctx.Err() })
	if len(ips) != 0 {
		t.Fatal("cancelled DNS produced resources")
	}
}

// Local Xray and httptest proxies only: never starts manager health workers or
// contacts public DNS, suppliers, or a public IP identification service.
func TestGrowthPrivateRouteSurvivesSharedSlotRetargetAndRemoval(t *testing.T) {
	if testing.Short() {
		t.Skip("uses a local Xray kernel")
	}
	proxyA := fakeUpstream(t, "proxy-A")
	defer proxyA.Close()
	proxyB := fakeUpstream(t, "proxy-B")
	defer proxyB.Close()
	kernel, err := NewKernel()
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(t.TempDir(), "proxy.json"))
	if err != nil {
		kernel.Close()
		t.Fatal(err)
	}
	store.Update(func(st *State) { st.Enabled = true })
	pool := NewPool(store, 32920, 32960)
	manager := &Manager{store: store, pool: pool, kernel: kernel, listenIP: "127.0.0.1"}
	defer manager.Close()
	nodeA := httpNodeSpec("node-A", strings.TrimPrefix(proxyA.URL, "http://"))
	nodeB := httpNodeSpec("node-B", strings.TrimPrefix(proxyB.URL, "http://"))
	pool.SetNodes([]NodeSpec{nodeA})
	markHealthy(t, pool, nodeA.ID, 1)
	// Fixture setup creates an ordinary binding; acquiring the growth lease
	// itself must not touch it or create bindings for other task accounts.
	if _, err := manager.DialProxy("account", "cn"); err != nil {
		t.Fatal(err)
	}
	route, ips, release, err := manager.GrowthRouteLease(context.Background(), "account", "cn")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !reflect.DeepEqual(ips, []string{"127.0.0.1"}) {
		t.Fatalf("remote proxy IPs %v", ips)
	}
	port, err := strconv.Atoi(route.Port())
	if err != nil {
		t.Fatal(err)
	}
	var tag string
	manager.growthMu.Lock()
	for id := range manager.growthSlots {
		tag = id
	}
	manager.growthMu.Unlock()
	if !strings.HasPrefix(tag, "growth_") {
		t.Fatal("no private growth route")
	}
	slots, _ := pool.Views()
	if len(slots) != 1 || slots[0].Port == port {
		t.Fatal("private slot leaked into business binding")
	}
	reserved, err := pool.allocProbePort()
	if err != nil {
		t.Fatal(err)
	}
	if reserved == port {
		t.Fatal("leased port reused by temporary allocator")
	}
	pool.releaseProbePort(reserved)
	if got := proxyGet(t, port, proxyA.URL); got != "proxy-A" {
		t.Fatal(got)
	}
	if err = kernel.RetargetSlot(slots[0].ID, nodeB); err != nil {
		t.Fatal(err)
	}
	if err = kernel.RemoveSlot(slots[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := proxyGet(t, port, proxyA.URL); got != "proxy-A" {
		t.Fatalf("task followed shared slot mutation: %s", got)
	}
	release()
	release()
	if kernel.HasSlot(tag) {
		t.Fatal("private route leaked after release")
	}
	pool.mu.Lock()
	occupied := pool.probePorts[port]
	pool.mu.Unlock()
	if occupied {
		t.Fatal("leased port reservation leaked")
	}

	// A second lease is also reclaimed on shutdown, and late release is safe.
	_, _, release2, err := manager.GrowthRouteLease(context.Background(), "second", "cn")
	if err != nil {
		t.Fatal(err)
	}
	_, bindings := pool.Views()
	if len(bindings) != 1 || bindings[0].UID != "account" {
		t.Fatal("private lease changed business bindings")
	}
	manager.closeGrowthRoutes()
	release2()
	if len(manager.growthSlots) != 0 {
		t.Fatal("shutdown leaked private slots")
	}
	if _, _, _, err = manager.GrowthRouteLease(context.Background(), "third", "cn"); err == nil {
		t.Fatal("closed manager admitted growth lease")
	}
}

func TestGrowthPlanningLeavesEntireQueueWithoutPortReservations(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.Update(func(st *State) { st.Enabled = true })
	pool := NewPool(store, 31080, 31082)
	node := httpNodeSpec("same-ip", "192.0.2.1:8080")
	pool.SetNodes([]NodeSpec{node})
	markHealthy(t, pool, node.ID, 1)
	manager := &Manager{store: store, pool: pool, listenIP: "127.0.0.1"}
	for i := 0; i < 100; i++ {
		ips, direct, acquire, err := manager.GrowthRoutePlan(context.Background(), strconv.Itoa(i), "cn")
		if err != nil || direct || acquire == nil || !reflect.DeepEqual(ips, []string{"192.0.2.1"}) {
			t.Fatal("invalid read-only resource plan")
		}
	}
	pool.mu.Lock()
	ports, slots, bindings := len(pool.probePorts), len(pool.slots), len(pool.bindngs)
	pool.mu.Unlock()
	if ports != 0 || slots != 0 || bindings != 0 || len(manager.growthSlots) != 0 {
		t.Fatalf("planning consumed resources: ports=%d slots=%d bindings=%d", ports, slots, bindings)
	}
}

func TestGrowthPlanRejectsNodeAndDNSDriftBeforeLease(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.Update(func(st *State) { st.Enabled = true })
	pool := NewPool(store, 31080, 31082)
	node := httpNodeSpec("node", "proxy.fixture.invalid:8080")
	pool.SetNodes([]NodeSpec{node})
	markHealthy(t, pool, node.ID, 1)
	answer := "192.0.2.1"
	manager := &Manager{store: store, pool: pool, growthLookup: func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP(answer)}}, nil
	}}
	_, _, acquire, err := manager.GrowthRoutePlan(context.Background(), "a", "cn")
	if err != nil {
		t.Fatal(err)
	}
	answer = "192.0.2.2"
	if _, _, err = acquire(context.Background()); err == nil || !strings.Contains(err.Error(), "DNS changed") {
		t.Fatalf("DNS drift accepted: %v", err)
	}
	_, _, acquire, err = manager.GrowthRoutePlan(context.Background(), "a", "cn")
	if err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	pool.nodes[node.ID].Spec["port"] = 9090
	pool.mu.Unlock()
	if _, _, err = acquire(context.Background()); err == nil || !strings.Contains(err.Error(), "route changed") {
		t.Fatalf("node drift accepted: %v", err)
	}
	if len(pool.probePorts) != 0 || len(manager.growthSlots) != 0 {
		t.Fatal("failed acquisition left reserved ports")
	}
}

func TestGrowthPrivateRouteHasHardActiveLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("uses local Xray kernel")
	}
	proxy := fakeUpstream(t, "fixture")
	defer proxy.Close()
	kernel, err := NewKernel()
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(t.TempDir(), "limit.json"))
	if err != nil {
		kernel.Close()
		t.Fatal(err)
	}
	store.Update(func(st *State) { st.Enabled = true })
	pool := NewPool(store, 32920, 32960)
	node := httpNodeSpec("fixture", strings.TrimPrefix(proxy.URL, "http://"))
	pool.SetNodes([]NodeSpec{node})
	markHealthy(t, pool, node.ID, 1)
	manager := &Manager{store: store, pool: pool, kernel: kernel, listenIP: "127.0.0.1", growthBudget: growthlimit.New(8)}
	defer manager.Close()
	var releases []func()
	for range 8 {
		if !manager.growthBudget.Enter() {
			t.Fatal("configured admission unavailable")
		}
	}
	for i := 0; i < 8; i++ {
		if i == 6 {
			_ = manager.growthBudget.Set(2)
		} // Already admitted work must still acquire.
		_, _, acquire, err := manager.GrowthRoutePlan(context.Background(), strconv.Itoa(i), "cn")
		if err != nil {
			t.Fatal(err)
		}
		_, release, err := acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	_, _, acquire, err := manager.GrowthRoutePlan(context.Background(), "ninth", "cn")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = acquire(context.Background()); err == nil {
		t.Fatal("ninth private route exceeded hard active limit")
	}
	if len(manager.growthSlots) != 8 || len(pool.probePorts) != 8 || len(pool.slots) != 0 || len(pool.bindngs) != 0 {
		t.Fatal("private slot bound or business binding isolation failed")
	}
	for _, release := range releases {
		release()
		manager.growthBudget.Leave()
	}
	if len(manager.growthSlots) != 0 || len(pool.probePorts) != 0 {
		t.Fatal("private slot cleanup leaked resources")
	}
}

func TestGrowthPortCapacityPreservesBindingsAndProbeHeadroom(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "capacity.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pool := NewPool(store, 33000, 33007)
	for range 6 {
		if _, err := pool.allocGrowthPort(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.allocGrowthPort(); err == nil {
		t.Fatal("growth consumed reserved headroom")
	}
	if _, err := pool.allocProbePort(); err != nil {
		t.Fatal("growth blocked ordinary probe")
	}
	pool.mu.Lock()
	_, err = pool.allocPortLocked()
	pool.mu.Unlock()
	if err != nil {
		t.Fatal("growth blocked ordinary binding")
	}
	for port := 33000; port <= 33007; port++ {
		pool.releaseProbePort(port)
	}
	if len(pool.probePorts) != 0 {
		t.Fatal("reservation leak")
	}
}
