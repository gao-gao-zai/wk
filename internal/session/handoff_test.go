package session

import "testing"

func TestBindingHandoff(t *testing.T) {
	cfg := Config{Salt: []byte("stable-test-secret"), Available: func() []string { return []string{"a"} }}
	a := New(cfg)
	want, _ := a.Resolve("session")
	raw, e := a.Export()
	if e != nil {
		t.Fatal(e)
	}
	b := New(cfg)
	if e = b.Import(raw); e != nil {
		t.Fatal(e)
	}
	got, ok := b.LookupLease("session")
	if !ok || got.UID != want {
		t.Fatal("binding lost")
	}
}
