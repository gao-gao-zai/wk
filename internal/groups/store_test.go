package groups

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestDefaultGroupAlwaysExists default 恒存在、不可改删；文件被清空后重开也自动补回。
func TestDefaultGroupAlwaysExists(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if g := s.List(); len(g) != 1 || g[0] != DefaultGroup {
		t.Fatalf("initial list = %v, want [default]", g)
	}
	if err := s.Rename(DefaultGroup, "x"); !errors.Is(err, ErrImmutable) {
		t.Fatalf("rename default = %v, want ErrImmutable", err)
	}
	if err := s.Delete(DefaultGroup); !errors.Is(err, ErrImmutable) {
		t.Fatalf("delete default = %v, want ErrImmutable", err)
	}
	// 手删文件后重开：default 必须回来。
	if err := os.Remove(filepath.Join(dir, "groups.json")); err != nil {
		t.Fatal(err)
	}
	s2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if g := s2.List(); len(g) != 1 || g[0] != DefaultGroup {
		t.Fatalf("after file removal list = %v, want [default]", g)
	}
}

// TestGroupCRUD 建改名删的完整生命周期 + 引用迁移。
func TestGroupCRUD(t *testing.T) {
	s := newStore(t)
	if err := s.Create("vip"); err != nil {
		t.Fatal(err)
	}
	if err := s.Create("vip"); err == nil {
		t.Fatal("duplicate create must fail")
	}
	if err := s.Create("bad name"); err == nil {
		t.Fatal("space in name must fail")
	}
	if err := s.Create(""); err == nil {
		t.Fatal("empty name must fail")
	}

	// 账号归属 + 改名迁移。
	if err := s.SetAccountGroups("u1", []string{"vip"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename("vip", "premium"); err != nil {
		t.Fatal(err)
	}
	if g := s.AccountGroups("u1"); len(g) != 1 || g[0] != "premium" {
		t.Fatalf("after rename u1 groups = %v, want [premium]", g)
	}

	// 组内还有账号 → 拒绝删除。
	if err := s.Delete("premium"); !errors.Is(err, ErrGroupInUse) {
		t.Fatalf("delete in-use group = %v, want ErrGroupInUse", err)
	}
	// 移出后可删。
	if err := s.SetAccountGroups("u1", []string{DefaultGroup}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("premium"); err != nil {
		t.Fatal(err)
	}

	// 重启后持久化正确（重开同一目录）。
	dir := filepath.Dir(s.fp)
	s2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if g := s2.List(); len(g) != 1 || g[0] != DefaultGroup {
		t.Fatalf("reloaded list = %v", g)
	}
	if g := s2.AccountGroups("u1"); len(g) != 1 || g[0] != DefaultGroup {
		t.Fatalf("reloaded u1 = %v", g)
	}
}

// TestAccountGroupsMultiMembership 多归属 + 未登记回落 default。
func TestAccountGroupsMultiMembership(t *testing.T) {
	s := newStore(t)
	if err := s.Create("a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Create("b"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountGroups("u1", []string{"a", "b", "a", " "}); err != nil {
		t.Fatal(err)
	}
	g := s.AccountGroups("u1")
	if len(g) != 2 || g[0] != "a" || g[1] != "b" {
		t.Fatalf("u1 = %v, want [a b]", g)
	}
	// 未登记的账号 = default。
	if g := s.AccountGroups("unknown"); len(g) != 1 || g[0] != DefaultGroup {
		t.Fatalf("unknown uid = %v, want [default]", g)
	}
	// 引用不存在的分组要报错，而不是静默丢弃。
	if err := s.SetAccountGroups("u1", []string{"a", "nope"}); err == nil {
		t.Fatal("unknown group must fail SetAccountGroups")
	}
	// AccountsInGroup 双组都能查到。
	if !s.AccountsInGroup("a")["u1"] || !s.AccountsInGroup("b")["u1"] {
		t.Fatal("u1 must appear in both groups")
	}
	// 清空归属：只在管理密钥下可用（不在任何组）。
	if err := s.SetAccountGroups("u1", nil); err != nil {
		t.Fatal(err)
	}
	if m := s.AccountsInGroup("a"); len(m) != 0 {
		t.Fatalf("u1 must be removed from group a: %v", m)
	}
}

// TestKeyLifecycle 密钥创建/查改删 + 脱敏。
func TestKeyLifecycle(t *testing.T) {
	s := newStore(t)
	if err := s.Create("vip"); err != nil {
		t.Fatal(err)
	}
	k, err := s.CreateKey("测试密钥", "vip")
	if err != nil {
		t.Fatal(err)
	}
	if len(k.Key) < 20 || k.ID == "" {
		t.Fatalf("generated key looks wrong: %+v", k)
	}
	// Lookup 精确命中。
	got, err := s.LookupKey(k.Key)
	if err != nil || got.Group != "vip" {
		t.Fatalf("lookup = %+v err=%v", got, err)
	}
	if _, err := s.LookupKey("wk-wrong"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong key err = %v, want ErrNotFound", err)
	}
	// 列表脱敏：完整密钥不回显。
	for _, info := range s.ListKeys() {
		if info.Key == k.Key {
			t.Fatal("list must not expose the full key")
		}
	}
	// 改绑到不存在的分组 → 报错。
	if err := s.UpdateKey(k.ID, "n", "nope"); err == nil {
		t.Fatal("update to unknown group must fail")
	}
	if err := s.UpdateKey(k.ID, "改名", DefaultGroup); err != nil {
		t.Fatal(err)
	}
	got, _ = s.LookupKey(k.Key)
	if got.Name != "改名" || got.Group != DefaultGroup {
		t.Fatalf("after update = %+v", got)
	}
	// 绑定的分组有密钥时删除分组被拒。
	if err := s.Delete(DefaultGroup); !errors.Is(err, ErrImmutable) {
		t.Fatalf("delete default = %v", err)
	}
	// 删除密钥后可查（语义：组不再被密钥引用）。
	if err := s.DeleteKey(k.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LookupKey(k.Key); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted key must not authenticate")
	}
}

// TestMigrateUnregistered 存量账号显式迁移进 default：读取路径虽然有
// "未登记=default"兜底，但 AccountsInGroup（选号过滤）与分组计数只看
// 归属表——不迁移的话 default 密钥选不到任何存量号。
func TestMigrateUnregistered(t *testing.T) {
	s := newStore(t)
	if err := s.Create("vip"); err != nil {
		t.Fatal(err)
	}
	// 两个未登记账号 + 一个已登记账号。
	if n := s.MigrateUnregistered([]string{"u1", "u2"}); n != 2 {
		t.Fatalf("migrated = %d, want 2", n)
	}
	if err := s.SetAccountGroups("u3", []string{"vip"}); err != nil {
		t.Fatal(err)
	}
	// 幂等：再跑一遍不再补（都在表里了）。
	if n := s.MigrateUnregistered([]string{"u1", "u2", "u3"}); n != 0 {
		t.Fatalf("second pass migrated = %d, want 0", n)
	}
	// default 组的选号集合现在包含存量账号。
	in := s.AccountsInGroup(DefaultGroup)
	if !in["u1"] || !in["u2"] || in["u3"] {
		t.Fatalf("default members = %v, want u1 u2 only", in)
	}
	// 重启后持久化保持。
	dir := filepath.Dir(s.fp)
	s2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if g := s2.AccountGroups("u1"); len(g) != 1 || g[0] != DefaultGroup {
		t.Fatalf("reloaded u1 = %v, want [default]", g)
	}
}

// TestKeyGroupFallbackDroppedGroup 密钥绑定的分组被（外部手段）删掉后，
// 加载时回落 default——删除接口会拦截，这是对手改文件的兜底。
func TestKeyGroupFallbackDroppedGroup(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create("vip"); err != nil {
		t.Fatal(err)
	}
	k, err := s.CreateKey("", "vip")
	if err != nil {
		t.Fatal(err)
	}
	_ = s
	// 重开：正常加载保持 vip。
	s2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s2.LookupKey(k.Key); got.Group != "vip" {
		t.Fatalf("group = %q, want vip", got.Group)
	}
}
