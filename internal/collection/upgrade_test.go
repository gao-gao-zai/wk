package collection

import (
	"context"
	"encoding/json"
	"testing"
)

func TestUpgradeCommitReportsStorageFailure(t *testing.T) {
	s := &memoryStore{}
	m, err := New(Options{Store: s, Run: func(context.Context, Target, Snapshot) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	s.mu.Lock()
	s.fail = true
	s.mu.Unlock()
	if m.FlushChecked() == nil {
		t.Fatal("failed final commit accepted")
	}
	s.mu.Lock()
	s.fail = false
	s.mu.Unlock()
	if err = m.FlushChecked(); err != nil {
		t.Fatal(err)
	}
}
