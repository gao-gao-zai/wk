package server

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeLedgerStore growthLedgerStore 的内存 fake，可注入失败。
type fakeLedgerStore struct {
	claims    []GrowthLedgerClaim
	recordErr error
}

func (f *fakeLedgerStore) RecordGrowthClaim(c GrowthLedgerClaim) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	// 保首条语义（与真实现一致）：已存在则只更新昵称。
	for i := range f.claims {
		if f.claims[i].UID == c.UID && f.claims[i].TaskCode == c.TaskCode {
			f.claims[i].Nickname = c.Nickname
			return nil
		}
	}
	f.claims = append(f.claims, c)
	return nil
}

func (f *fakeLedgerStore) LoadGrowthClaims() ([]GrowthLedgerClaim, error) {
	return f.claims, nil
}

func TestGrowthLedgerDBModeRecordAndReload(t *testing.T) {
	store := &fakeLedgerStore{}
	l := newGrowthLedger("", store)

	l.record("u1", "nick", "t1", 5, 2)
	l.record("u1", "nick", "t2", 3, 0)
	l.record("u2", "nick2", "t1", 1, 1)

	if len(store.claims) != 3 {
		t.Fatalf("store recorded %d claims, want 3", len(store.claims))
	}

	// 重新打开（模拟重启）：内存从库恢复。
	l2 := newGrowthLedger("", store)
	acc := l2.byUID["u1"]
	if acc == nil || len(acc.Claims) != 2 {
		t.Fatalf("u1 not restored: %#v", acc)
	}
	totalCredit := int64(0)
	for _, c := range acc.Claims {
		totalCredit += c.Credit
	}
	if totalCredit != 8 {
		t.Fatalf("u1 credits = %d, want 8", totalCredit)
	}
	// 领取时间往返（unix → RFC3339）非空。
	if acc.Claims[0].At == "" {
		t.Fatal("claimed_at lost in unix→RFC3339 roundtrip")
	}
}

func TestGrowthLedgerDBModeIdempotentRecord(t *testing.T) {
	store := &fakeLedgerStore{}
	l := newGrowthLedger("", store)

	l.record("u1", "nick", "t1", 5, 2)
	first := l.byUID["u1"].Claims[0].At
	time.Sleep(1100 * time.Millisecond)  // 保证时间戳不同
	l.record("u1", "nick", "t1", 99, 99) // 重复领取

	claims := l.byUID["u1"].Claims
	if len(claims) != 1 {
		t.Fatalf("duplicate record produced %d claims", len(claims))
	}
	if claims[0].Credit != 5 || claims[0].At != first {
		t.Fatalf("first-write fields not preserved: %+v", claims[0])
	}
}

func TestGrowthLedgerDBModeRecordFailureKeepsMemory(t *testing.T) {
	store := &fakeLedgerStore{recordErr: errors.New("db down")}
	l := newGrowthLedger("", store)

	l.record("u1", "nick", "t1", 5, 2) // 只打日志，不 panic 不回滚

	acc := l.byUID["u1"]
	if acc == nil || len(acc.Claims) != 1 || acc.Claims[0].Credit != 5 {
		t.Fatalf("memory record lost on store failure: %#v", acc)
	}
}

func TestGrowthLedgerDBModeImportsLegacyFile(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "growth-ledger.json")
	content := `{"accounts":[{"uid":"u1","nickname":"nick","claims":[{"task_code":"t1","credit":5,"energy":2,"at":"2025-01-01T00:00:00Z"}]}]}`
	if err := os.WriteFile(legacy, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	store := &fakeLedgerStore{}
	l := newGrowthLedger(legacy, store) // 空库 + 旧文件存在 → 导入

	if len(store.claims) != 1 {
		t.Fatalf("legacy import wrote %d claims, want 1", len(store.claims))
	}
	got := store.claims[0]
	if got.UID != "u1" || got.TaskCode != "t1" || got.Credit != 5 || got.ClaimedAtUnix <= 0 {
		t.Fatalf("imported claim wrong: %+v", got)
	}
	if len(l.byUID) != 1 || len(l.byUID["u1"].Claims) != 1 {
		t.Fatalf("memory not populated during import: %#v", l.byUID)
	}
	// 旧文件保留（可回滚）。
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy file removed: %v", err)
	}
}

func TestGrowthLedgerDBModeSkipsImportWhenStoreHasData(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "growth-ledger.json")
	content := `{"accounts":[{"uid":"legacy","claims":[{"task_code":"t0","credit":1,"energy":0,"at":"2025-01-01T00:00:00Z"}]}]}`
	if err := os.WriteFile(legacy, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	store := &fakeLedgerStore{claims: []GrowthLedgerClaim{{UID: "u1", TaskCode: "t1", ClaimedAtUnix: 1000}}}
	l := newGrowthLedger(legacy, store)

	if len(store.claims) != 1 || store.claims[0].UID != "u1" {
		t.Fatalf("legacy data leaked into non-empty store: %+v", store.claims)
	}
	if _, ok := l.byUID["legacy"]; ok {
		t.Fatal("legacy account loaded despite non-empty store")
	}
}

func TestGrowthLedgerFileModeUnchanged(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "growth-ledger.json")

	l := newGrowthLedger(fp, nil) // 无 store = 文件模式
	l.record("u1", "nick", "t1", 5, 2)

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("file mode did not persist: %v", err)
	}
	if !strings.Contains(string(raw), `"task_code": "t1"`) {
		t.Fatalf("file content wrong: %s", raw)
	}
}
