package reqproxy

import (
	"context"
	"net"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

// A rotating proxy hostname must not invalidate a route whose connection IP
// and scheduling resources were already frozen before admission.
func TestGrowthPlanKeepsPinnedRouteAcrossDNSRotation(t *testing.T) {
	if testing.Short() {
		t.Skip("uses a local Xray kernel")
	}
	proxy := fakeUpstream(t, "pinned-proxy")
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
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
	pool := NewPool(store, 33320, 33360)
	node := httpNodeSpec("rotating-proxy", net.JoinHostPort("proxy.fixture.invalid", proxyURL.Port()))
	pool.SetNodes([]NodeSpec{node})
	markHealthy(t, pool, node.ID, 1)
	calls := 0
	manager := &Manager{store: store, pool: pool, kernel: kernel, listenIP: "127.0.0.1", growthLookup: func(context.Context, string) ([]net.IPAddr, error) {
		calls++
		if calls == 1 {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}, {IP: net.ParseIP("192.0.2.1")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("192.0.2.2")}}, nil
	}}
	defer manager.Close()
	ips, direct, acquire, err := manager.GrowthRoutePlan(context.Background(), "account", "cn")
	if err != nil || direct || !reflect.DeepEqual(ips, []string{"127.0.0.1", "192.0.2.1"}) {
		t.Fatalf("invalid pinned plan: ips=%v direct=%v err=%v", ips, direct, err)
	}
	route, release, err := acquire(context.Background())
	if err != nil {
		t.Fatalf("DNS rotation rejected an already pinned route: %v", err)
	}
	defer release()
	if calls != 1 {
		t.Fatalf("acquisition resolved the frozen hostname again: calls=%d", calls)
	}
	port, err := strconv.Atoi(route.Port())
	if err != nil {
		t.Fatal(err)
	}
	if got := proxyGet(t, port, "http://destination.fixture.invalid/"); got != "pinned-proxy" {
		t.Fatalf("request did not use the planned proxy IP: %q", got)
	}
	slots, bindings := manager.Views()
	if len(slots) != 0 || len(bindings) != 0 {
		t.Fatal("private route changed business bindings")
	}
	release()
	if len(pool.probePorts) != 0 || len(manager.growthSlots) != 0 {
		t.Fatal("private route leaked after release")
	}
}
