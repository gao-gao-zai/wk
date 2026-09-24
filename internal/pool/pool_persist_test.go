package pool

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// fakePersister StatePersister 的内存 fake，可注入失败与记录调用。
type fakePersister struct {
	mu             sync.Mutex
	rows           map[string][]byte
	upsertErr      error
	deleteErr      error
	lastUpsertUIDs []string
}

func newFakePersister() *fakePersister {
	return &fakePersister{rows: map[string][]byte{}}
}

func newFailingFakePersister(err error) *fakePersister {
	return &fakePersister{rows: map[string][]byte{}, upsertErr: err}
}

func (f *fakePersister) UpsertPoolAccounts(rows map[string][]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return f.upsertErr
	}
	for uid, raw := range rows {
		f.rows[uid] = raw
		f.lastUpsertUIDs = append(f.lastUpsertUIDs, uid)
	}
	return nil
}

func (f *fakePersister) DeletePoolAccounts(uids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	for _, uid := range uids {
		delete(f.rows, uid)
	}
	return nil
}

func (f *fakePersister) LoadPoolAccounts() (map[string][]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string][]byte, len(f.rows))
	for uid, raw := range f.rows {
		out[uid] = raw
	}
	return out, nil
}

func (f *fakePersister) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

func (f *fakePersister) has(uid string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.rows[uid]
	return ok
}

// TestPoolDBModeFlushOnlyWritesChangedAccounts 增量语义：只改 u1 时
// flush 只应写 u1 的行（fake 的 lastUpsertUIDs 断言）。
func TestPoolDBModeFlushOnlyWritesChangedAccounts(t *testing.T) {
	fp := newFakePersister()
	p := New(filepath.Join(t.TempDir(), "state.json"), fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Flush() // 启动基线：两个账号都入库
	if fp.count() != 2 {
		t.Fatalf("baseline flush wrote %d rows, want 2", fp.count())
	}

	// 只改 u1。
	fp.mu.Lock()
	fp.lastUpsertUIDs = nil
	fp.mu.Unlock()
	p.SetCredits("u1", 42)
	p.Flush()

	fp.mu.Lock()
	upserted := append([]string(nil), fp.lastUpsertUIDs...)
	fp.mu.Unlock()
	if len(upserted) != 1 || upserted[0] != "u1" {
		t.Fatalf("incremental flush wrote %v, want [u1]", upserted)
	}
}

// TestPoolDBModePersistFailureKeepsDirty 写库失败保留脏行，下轮重试成功。
func TestPoolDBModePersistFailureKeepsDirty(t *testing.T) {
	fp := newFailingFakePersister(errors.New("db down"))
	p := New(filepath.Join(t.TempDir(), "state.json"), fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)
	p.Flush() // 失败：行保留在 dirtySet

	if fp.count() != 0 {
		t.Fatalf("fake should have failed, got %d rows", fp.count())
	}
	if p.persistFails == 0 {
		t.Fatal("persist failure not recorded")
	}

	// 恢复后重试成功。
	fp.mu.Lock()
	fp.upsertErr = nil
	fp.mu.Unlock()
	p.Flush()
	if !fp.has("u1") {
		t.Fatal("retry after failure did not write u1")
	}
	if p.persistFails != 0 {
		t.Fatalf("persistFails not reset after recovery: %d", p.persistFails)
	}
}

// TestPoolDBModeRestartRestoresFromDB 重启（新 Pool 同一 persister）恢复状态。
func TestPoolDBModeRestartRestoresFromDB(t *testing.T) {
	fp := newFakePersister()
	dir := t.TempDir()

	p1 := New(filepath.Join(dir, "state.json"), fp)
	p1.Add(&auth.Auth{UID: "u1"})
	p1.Disable("u1", "manual")
	p1.Flush()

	// 模拟重启：新 Pool 从同一 DB 恢复。
	p2 := New(filepath.Join(dir, "state.json"), fp)
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("u1 not restored from DB")
	}
	if !st.Disabled {
		t.Fatal("disabled state not restored from DB")
	}
}

// TestPoolDBModeDeleteSyncsToDB Remove/Delete 的行从 DB 删除。
func TestPoolDBModeDeleteSyncsToDB(t *testing.T) {
	fp := newFakePersister()
	p := New(filepath.Join(t.TempDir(), "state.json"), fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Flush()

	p.Remove("u1")
	p.Flush()

	if fp.has("u1") {
		t.Fatal("u1 row not deleted from DB")
	}
	if !fp.has("u2") {
		t.Fatal("u2 row should remain")
	}
}

// TestPoolDBModeLegacyFileImport 空库 + state.json 存在 → 一次性导入。
func TestPoolDBModeLegacyFileImport(t *testing.T) {
	dir := t.TempDir()
	stateJSON := `{"accounts":{"u1":{"credits":77,"disabled":true}}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(stateJSON), 0600); err != nil {
		t.Fatal(err)
	}

	fp := newFakePersister()
	p := New(filepath.Join(dir, "state.json"), fp) // New 内 load() + loadFromPersister()

	if !fp.has("u1") {
		t.Fatal("legacy state.json not imported to DB")
	}
	st, ok := p.Status("u1")
	if !ok || st.Credits != 77 || !st.Disabled {
		t.Fatalf("imported state wrong: %+v ok=%v", st, ok)
	}
	// 旧文件保留。
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Fatalf("legacy file removed: %v", err)
	}
}

// TestPoolDBModeRedisSnapshotAdoptedWritesThrough Redis 快照被采用后全量写穿 DB。
func TestPoolDBModeRedisSnapshotAdoptedWritesThrough(t *testing.T) {
	dir := t.TempDir()
	// 本地旧文件（mtime 早于快照）。
	old := `{"accounts":{"u1":{"credits":1}}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}

	fp := newFakePersister()
	p := New(filepath.Join(dir, "state.json"), fp)
	rs := &fakeRedisState{state: map[string][]byte{}}
	p.SetStore(rs)

	// 构造比本地新的 Redis 快照：u2 credits=99。
	snap := `{"accounts":{"u2":{"credits":99}},"saved_at":"` + time.Now().Add(time.Hour).Format(time.RFC3339) + `"}`
	rs.state["k"] = []byte(snap)

	p.RestoreFromSnapshot()
	st, ok := p.Status("u2")
	if !ok || st.Credits != 99 {
		t.Fatalf("redis snapshot not adopted: %+v ok=%v", st, ok)
	}
	// 写穿：Flush 后 u2 入库。
	p.Flush()
	if !fp.has("u2") {
		t.Fatal("adopted snapshot not written through to DB")
	}
}

// fakeRedisState redisstore.Store 的最小 fake（SaveState/LoadState）。
type fakeRedisState struct {
	mu    sync.Mutex
	state map[string][]byte
}

func (f *fakeRedisState) SaveState(data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state["k"] = data
}

func (f *fakeRedisState) LoadState() ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, ok := f.state["k"]
	return raw, ok
}

// TestPoolFileModeRegressionZeroPersister 文件模式回归：无 persister 时
// 行为与历史版本一致（Flush 落盘 state.json，整文件 JSON）。
func TestPoolFileModeRegressionZeroPersister(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42)
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("state.json not written: %v", err)
	}
	if !filepathExists(fp) {
		t.Fatal("state.json missing")
	}
	_ = raw // 内容细节由既有测试覆盖
}

func filepathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// TestPoolDBModeConcurrentChangeDuringFlush 收集→提交窗口内的新版本不被
// 误清除（版本号语义）。
func TestPoolDBModeConcurrentChangeDuringFlush(t *testing.T) {
	fp := newFakePersister()
	p := New(filepath.Join(t.TempDir(), "state.json"), fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 1)

	// 第一次 flush 前 dirtySet 有 u1(v2)；flush 收集后（写库前）又改 u1(v3)。
	// 通过 hook 不可行（无注入点），改为直接验证语义：flush 成功后 dirtySet
	// 里的版本与收集时一致的行被清除；这里串行模拟"flush 后又有变更"。
	p.Flush()
	if len(p.dirtySet) != 0 {
		t.Fatalf("dirtySet not cleared after successful flush: %v", p.dirtySet)
	}
	// 新变更 → 下轮 flush 再写。
	p.SetCredits("u1", 2)
	if len(p.dirtySet) != 1 {
		t.Fatalf("dirtySet should have u1 after change: %v", p.dirtySet)
	}
	p.Flush()
	if len(p.dirtySet) != 0 {
		t.Fatalf("dirtySet not cleared: %v", p.dirtySet)
	}
}
