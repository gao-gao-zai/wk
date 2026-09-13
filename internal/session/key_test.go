package session

import (
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/redisstore"
)

// saltedRouter 构建一个带固定盐的路由器，用于断言不可预测性。
func saltedRouter(store redisstore.Store, avail []string, ttl time.Duration, salt []byte) *Router {
	return New(Config{TTL: ttl, Store: store, Available: func() []string { return avail }, Salt: salt})
}

// TestNormalizeKeyIsFixedLength is the core memory-bound property: an attacker
// controls the sticky key, and before this change the raw value was stored
// verbatim as a map key. A 10 MB conversation_id therefore cost 10 MB of heap
// per request. Normalizing to a fixed-size HMAC makes key size irrelevant.
func TestNormalizeKeyIsFixedLength(t *testing.T) {
	r := saltedRouter(newCountingStore(), []string{"a1"}, time.Minute, []byte("0123456789abcdef"))
	small := r.normalizeKey("c1")
	huge := r.normalizeKey(strings.Repeat("A", 5<<20))

	if len(small) != len(huge) {
		t.Errorf("normalized length differs by input size: %d vs %d", len(small), len(huge))
	}
	if len(small) != 64 { // sha256 hex
		t.Errorf("normalized key length = %d, want 64", len(small))
	}
}

// TestNormalizeKeyIsSalted guards against a regression to an unsalted digest,
// which anyone knowing the account list could precompute to steal a slot.
func TestNormalizeKeyIsSalted(t *testing.T) {
	a := saltedRouter(newCountingStore(), []string{"a1"}, time.Minute, []byte("salt-aaaaaaaaaaaa"))
	b := saltedRouter(newCountingStore(), []string{"a1"}, time.Minute, []byte("salt-bbbbbbbbbbbb"))

	if a.normalizeKey("c1") == b.normalizeKey("c1") {
		t.Error("same key must normalize differently under different salts")
	}
}

// TestNormalizeKeyIsStable: the same key must map to the same binding, or
// sticky sessions would re-home on every request.
func TestNormalizeKeyIsStable(t *testing.T) {
	r := saltedRouter(newCountingStore(), []string{"a1", "a2"}, time.Minute, []byte("fixed-salt-1234567890"))
	first, ok := r.Resolve("conversation-1")
	if !ok {
		t.Fatal("first resolve failed")
	}
	for i := 0; i < 5; i++ {
		got, ok := r.Resolve("conversation-1")
		if !ok || got != first {
			t.Fatalf("resolve %d = %q ok=%v, want stable %q", i, got, ok, first)
		}
	}
	if r.Count() != 1 {
		t.Errorf("count=%d want 1: repeated resolves must not create new entries", r.Count())
	}
}

// TestKillSwitchKeyDoesNotForgeBinding documents the concrete attack the salt
// prevents: picking a key that hashes onto a specific account. Without a secret
// salt this is offline-computable.
func TestDistinctKeysNormalizeDistinctly(t *testing.T) {
	r := saltedRouter(newCountingStore(), []string{"a1", "a2"}, time.Minute, []byte("fixed-salt-1234567890"))
	seen := map[string]bool{}
	for _, k := range []string{"c1", "c2", "c3", "conversation-1"} {
		n := r.normalizeKey(k)
		if seen[n] {
			t.Fatalf("distinct keys collided after normalization: %q", k)
		}
		seen[n] = true
	}
}

// TestMaxEntriesBoundsTheMap is the bound the TTL alone could not provide: a
// flood of distinct keys inside one TTL window are all unexpired, so only an
// explicit cap stops growth.
func TestMaxEntriesBoundsTheMap(t *testing.T) {
	const cap = 64
	r := New(Config{
		TTL:        time.Hour, // nothing expires during this test
		Store:      newCountingStore(),
		Available:  func() []string { return []string{"a1", "a2"} },
		Salt:       []byte("fixed-salt-1234567890"),
		MaxEntries: cap,
	})
	for i := 0; i < cap*6; i++ {
		r.Resolve("flood-" + strings.Repeat("x", i%32) + string(rune('a'+i%26)) + itoa(i))
	}
	// evictLocked trims to the cap plus at most one batch of slack.
	if got := r.Count(); got > cap+cap/8+1 {
		t.Errorf("entries=%d exceeded cap %d (unbounded growth)", got, cap)
	}
}

// TestEmptyKeyIsIgnored keeps the "no conversation key → plain rotation"
// contract intact after normalization was added.
func TestEmptyKeyIsIgnored(t *testing.T) {
	r := saltedRouter(newCountingStore(), []string{"a1"}, time.Minute, []byte("fixed-salt-1234567890"))
	if _, ok := r.Resolve(""); ok {
		t.Error("empty key must not resolve")
	}
	if r.Count() != 0 {
		t.Errorf("empty key created an entry (count=%d)", r.Count())
	}
	r.Bind("", "a1")
	if r.Count() != 0 {
		t.Errorf("Bind with empty key created an entry (count=%d)", r.Count())
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
