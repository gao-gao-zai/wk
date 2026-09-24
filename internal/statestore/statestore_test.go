package statestore

import (
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPoolAccountUpsertLoadDelete(t *testing.T) {
	s := openTestStore(t)

	loaded, err := s.LoadPoolAccounts()
	if err != nil {
		t.Fatalf("LoadPoolAccounts on empty: %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("empty store returned %d rows", len(loaded))
	}

	rows := map[string][]byte{
		"u1": []byte(`{"credits":10}`),
		"u2": []byte(`{"credits":20}`),
	}
	if err := s.UpsertPoolAccounts(rows); err != nil {
		t.Fatalf("UpsertPoolAccounts: %v", err)
	}
	loaded, err = s.LoadPoolAccounts()
	if err != nil {
		t.Fatalf("LoadPoolAccounts: %v", err)
	}
	if len(loaded) != 2 || string(loaded["u1"]) != `{"credits":10}` || string(loaded["u2"]) != `{"credits":20}` {
		t.Fatalf("unexpected rows: %#v", loaded)
	}

	// 覆盖写：u1 更新，u2 不动。
	if err := s.UpsertPoolAccounts(map[string][]byte{"u1": []byte(`{"credits":11}`)}); err != nil {
		t.Fatalf("UpsertPoolAccounts overwrite: %v", err)
	}
	loaded, err = s.LoadPoolAccounts()
	if err != nil {
		t.Fatalf("LoadPoolAccounts: %v", err)
	}
	if string(loaded["u1"]) != `{"credits":11}` || string(loaded["u2"]) != `{"credits":20}` {
		t.Fatalf("overwrite produced wrong rows: %#v", loaded)
	}

	// 删除。
	if err := s.DeletePoolAccounts([]string{"u2"}); err != nil {
		t.Fatalf("DeletePoolAccounts: %v", err)
	}
	loaded, err = s.LoadPoolAccounts()
	if err != nil {
		t.Fatalf("LoadPoolAccounts after delete: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("delete left %d rows, want 1", len(loaded))
	}
	if _, ok := loaded["u1"]; !ok {
		t.Fatalf("u1 missing after delete: %#v", loaded)
	}

	// MaxPoolUpdatedAt：有行后 > 0。
	ts, err := s.MaxPoolUpdatedAt()
	if err != nil {
		t.Fatalf("MaxPoolUpdatedAt: %v", err)
	}
	if ts <= 0 {
		t.Fatalf("MaxPoolUpdatedAt = %d, want > 0", ts)
	}
}

func TestPoolAccountEmptyOpsAreNoops(t *testing.T) {
	s := openTestStore(t)
	if err := s.UpsertPoolAccounts(nil); err != nil {
		t.Fatalf("UpsertPoolAccounts(nil): %v", err)
	}
	if err := s.DeletePoolAccounts(nil); err != nil {
		t.Fatalf("DeletePoolAccounts(nil): %v", err)
	}
	if ts, err := s.MaxPoolUpdatedAt(); err != nil || ts != 0 {
		t.Fatalf("MaxPoolUpdatedAt on empty table = (%d, %v), want (0, nil)", ts, err)
	}
}

func TestGrowthClaimIdempotency(t *testing.T) {
	s := openTestStore(t)

	// 空表加载。
	if claims, err := s.LoadGrowthClaims(); err != nil || len(claims) != 0 {
		t.Fatalf("LoadGrowthClaims on empty = (%v, %v)", claims, err)
	}

	first := GrowthClaim{UID: "u1", TaskCode: "t1", Nickname: "nick-old", Credit: 5, Energy: 2, ClaimedAtUnix: 1000}
	if err := s.RecordGrowthClaim(first); err != nil {
		t.Fatalf("RecordGrowthClaim: %v", err)
	}
	// 重复领取（不同时间/积分/昵称）：保首条时间与积分，昵称刷新。
	dup := GrowthClaim{UID: "u1", TaskCode: "t1", Nickname: "nick-new", Credit: 99, Energy: 99, ClaimedAtUnix: 2000}
	if err := s.RecordGrowthClaim(dup); err != nil {
		t.Fatalf("RecordGrowthClaim dup: %v", err)
	}
	claims, err := s.LoadGrowthClaims()
	if err != nil {
		t.Fatalf("LoadGrowthClaims: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("want 1 claim, got %d", len(claims))
	}
	got := claims[0]
	if got.ClaimedAtUnix != 1000 || got.Credit != 5 || got.Energy != 2 {
		t.Fatalf("idempotency broken: first-write fields not preserved: %+v", got)
	}
	if got.Nickname != "nick-new" {
		t.Fatalf("nickname not refreshed: %+v", got)
	}

	// 同账号不同任务、不同账号同任务互不影响。
	if err := s.RecordGrowthClaim(GrowthClaim{UID: "u1", TaskCode: "t2", ClaimedAtUnix: 3000}); err != nil {
		t.Fatalf("RecordGrowthClaim t2: %v", err)
	}
	if err := s.RecordGrowthClaim(GrowthClaim{UID: "u2", TaskCode: "t1", ClaimedAtUnix: 4000}); err != nil {
		t.Fatalf("RecordGrowthClaim u2: %v", err)
	}
	claims, err = s.LoadGrowthClaims()
	if err != nil {
		t.Fatalf("LoadGrowthClaims: %v", err)
	}
	if len(claims) != 3 {
		t.Fatalf("want 3 claims, got %d", len(claims))
	}
	// 排序确定性：u1/t1, u1/t2, u2/t1。
	wantOrder := []struct{ uid, task string }{{"u1", "t1"}, {"u1", "t2"}, {"u2", "t1"}}
	for i, w := range wantOrder {
		if claims[i].UID != w.uid || claims[i].TaskCode != w.task {
			t.Fatalf("claim[%d] = (%s,%s), want (%s,%s)", i, claims[i].UID, claims[i].TaskCode, w.uid, w.task)
		}
	}
}

func TestDialectLabel(t *testing.T) {
	s := openTestStore(t)
	if s.Dialect() != "sqlite" {
		t.Fatalf("Dialect() = %q, want sqlite", s.Dialect())
	}
}
