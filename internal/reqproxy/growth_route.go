package reqproxy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/growthlimit"
)

// SetGrowthBudget is startup wiring; runner admission owns the same budget.
func (m *Manager) SetGrowthBudget(b *growthlimit.Budget) {
	m.growthMu.Lock()
	defer m.growthMu.Unlock()
	m.growthBudget = b
}

// GrowthRoutePlan is read-only. Queued accounts do not reserve ports, install
// listeners or modify business bindings. Acquire revalidates the exact plan.
func (m *Manager) GrowthRoutePlan(ctx context.Context, uid, region string) ([]string, bool, func(context.Context) (*url.URL, func(), error), error) {
	m.growthMu.Lock()
	defer m.growthMu.Unlock()
	if m.growthClosed {
		return nil, false, nil, errors.New("growth proxy manager closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, nil, err
	}
	node, proxied, err := m.selectGrowthNode(uid, region)
	if err != nil {
		return nil, false, nil, err
	}
	if m.SharedError() != nil {
		return nil, false, nil, errors.New("growth proxy state unavailable")
	}
	if !proxied {
		return nil, true, func(ctx context.Context) (*url.URL, func(), error) {
			m.growthMu.Lock()
			defer m.growthMu.Unlock()
			if m.growthClosed {
				return nil, nil, errors.New("growth proxy manager closed")
			}
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			_, nowProxied, err := m.selectGrowthNode(uid, region)
			if err != nil || nowProxied {
				return nil, nil, errors.New("growth proxy route changed after planning")
			}
			if m.SharedError() != nil {
				return nil, nil, errors.New("growth proxy state unavailable")
			}
			return nil, nil, nil
		}, nil
	}
	expected, _ := json.Marshal(node)
	frozen, ips := freezeGrowthNode(ctx, node, m.growthLookup)
	frozenRaw, _ := json.Marshal(frozen)
	if err = ctx.Err(); err != nil {
		return nil, false, nil, err
	}
	acquire := func(ctx context.Context) (*url.URL, func(), error) {
		m.growthMu.Lock()
		defer m.growthMu.Unlock()
		if m.growthClosed {
			return nil, nil, errors.New("growth proxy manager closed")
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		current, stillProxied, err := m.selectGrowthNode(uid, region)
		currentRaw, _ := json.Marshal(current)
		if err != nil || !stillProxied || string(currentRaw) != string(expected) {
			return nil, nil, errors.New("growth proxy route changed after planning")
		}
		current, currentIPs := freezeGrowthNode(ctx, current, m.growthLookup)
		currentRaw, _ = json.Marshal(current)
		if string(currentRaw) != string(frozenRaw) || !slices.Equal(currentIPs, ips) {
			return nil, nil, errors.New("growth proxy DNS changed after planning")
		}
		if m.SharedError() != nil {
			return nil, nil, errors.New("growth proxy state unavailable")
		}
		if err = ctx.Err(); err != nil {
			return nil, nil, err
		}
		return m.acquireGrowthNode(ctx, frozen)
	}
	return append([]string(nil), ips...), false, acquire, nil
}

// Compatibility entrypoint; the production runner plans before admission and
// acquires only for the active set. Both entrypoints revalidate node and DNS.
func (m *Manager) GrowthRouteLease(ctx context.Context, uid, region string) (*url.URL, []string, func(), error) {
	ips, _, acquire, err := m.GrowthRoutePlan(ctx, uid, region)
	if err != nil {
		return nil, nil, nil, err
	}
	proxy, release, err := acquire(ctx)
	return proxy, ips, release, err
}

// Caller holds growthMu; never hold the pool lock across kernel operations.
func (m *Manager) acquireGrowthNode(ctx context.Context, node NodeSpec) (*url.URL, func(), error) {
	if m.kernel == nil {
		return nil, nil, errors.New("growth proxy kernel unavailable")
	}
	if len(m.growthSlots) >= m.growthBudget.LeaseLimit() {
		return nil, nil, errors.New("growth proxy active route limit reached")
	}
	port, err := m.pool.allocGrowthPort()
	if err != nil {
		return nil, nil, errors.New("growth proxy port capacity unavailable (business/probe ports reserved)")
	}
	tag := newID("growth")
	if err = m.kernel.AddSlot(tag, port, node); err != nil {
		m.pool.releaseProbePort(port)
		return nil, nil, errors.New("growth proxy private route unavailable")
	}
	if err = ctx.Err(); err != nil {
		_ = m.kernel.RemoveSlot(tag)
		m.pool.releaseProbePort(port)
		return nil, nil, err
	}
	if m.growthSlots == nil {
		m.growthSlots = map[string]int{}
	}
	m.growthSlots[tag] = port
	var once sync.Once
	release := func() { once.Do(func() { m.releaseGrowthRoute(tag) }) }
	return mustParseURL("http://" + net.JoinHostPort(m.ProxyHost(), strconv.Itoa(port))), release, nil
}

// Select exactly as an account route would, without creating, moving or
// persisting its business binding. Existing legal bindings keep their node;
// unbound accounts use the pool's existing region/group/health ranking.
func (m *Manager) selectGrowthNode(uid, region string) (NodeSpec, bool, error) {
	p := m.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mu.err != nil {
		return NodeSpec{}, false, errors.New("growth proxy state unavailable")
	}
	if !p.store.st.Enabled {
		return NodeSpec{}, false, nil
	}
	var allowed map[string]bool
	if p.groupResolver != nil {
		allowed = p.groupResolver.AllowedProxyGroups(uid)
	}
	var chosen *Node
	if binding, ok := p.bindngs[uid]; ok {
		if slot := p.slots[binding.SlotID]; slot != nil {
			if n := p.nodes[slot.NodeID]; n != nil && p.nodeServesRegionLocked(*n, region) && nodeAllowedGroup(*n, allowed) {
				chosen = n
			}
		}
	}
	if chosen == nil {
		candidates := p.candidatesLocked(region, allowed)
		if len(candidates) == 0 {
			if len(p.nodes) == 0 {
				return NodeSpec{}, false, nil
			}
			return NodeSpec{}, false, errors.New("growth proxy selection failed")
		}
		n := p.pickNodeLocked(candidates)
		chosen = &n
	}
	raw, err := json.Marshal(chosen.NodeSpec)
	var node NodeSpec
	if err != nil || json.Unmarshal(raw, &node) != nil {
		return NodeSpec{}, false, errors.New("growth proxy node unavailable")
	}
	return node, true, nil
}

func (m *Manager) releaseGrowthRoute(tag string) {
	m.growthMu.Lock()
	defer m.growthMu.Unlock()
	if port, ok := m.growthSlots[tag]; ok {
		_ = m.kernel.RemoveSlot(tag)
		m.pool.releaseProbePort(port)
		delete(m.growthSlots, tag)
	}
}

func (m *Manager) closeGrowthRoutes() {
	m.growthMu.Lock()
	defer m.growthMu.Unlock()
	m.growthClosed = true
	for tag, port := range m.growthSlots {
		if m.kernel != nil {
			_ = m.kernel.RemoveSlot(tag)
		}
		m.pool.releaseProbePort(port)
		delete(m.growthSlots, tag)
	}
}

// freezeGrowthNode uses a bounded system-DNS lookup, never a public IP probe.
// All answers are resources, even though the private outbound is pinned to one
// address. Unknown/oversized answer sets stay exclusive instead of guessing.
func freezeGrowthNode(ctx context.Context, node NodeSpec, lookup func(context.Context, string) ([]net.IPAddr, error)) (NodeSpec, []string) {
	host, _ := node.Spec["host"].(string)
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if ip := net.ParseIP(host); ip != nil {
		return node, []string{ip.String()}
	}
	if host == "" {
		return node, nil
	}
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	answers, err := lookup(ctx, host)
	if err != nil || len(answers) == 0 || len(answers) > 32 {
		return node, nil
	}
	seen := map[string]bool{}
	var ips []string
	for _, answer := range answers {
		if answer.IP == nil || answer.Zone != "" {
			return node, nil
		}
		ip := answer.IP.String()
		if net.ParseIP(ip) == nil {
			return node, nil
		}
		if !seen[ip] {
			seen[ip] = true
			ips = append(ips, ip)
		}
	}
	if len(ips) == 0 {
		return node, nil
	}
	sort.Strings(ips)
	// Spec belongs to the independent JSON copy above; preserving host-derived
	// SNI and HTTP transport authority avoids changing the proxy handshake.
	copySpec := make(map[string]any, len(node.Spec)+2)
	for k, v := range node.Spec {
		copySpec[k] = v
	}
	if serverName, _ := copySpec["serverName"].(string); serverName == "" {
		copySpec["serverName"] = host
	}
	if network, _ := copySpec["network"].(string); network != "" && network != "tcp" && network != "raw" {
		if authority, _ := copySpec["wsHost"].(string); authority == "" {
			copySpec["wsHost"] = host
		}
	}
	copySpec["host"] = ips[0]
	node.Spec = copySpec
	return node, ips
}
