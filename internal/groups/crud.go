package groups

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// —— 分组 ——

// List 返回全部分组（有序，default 排最前）。
func (s *Store) List() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]string(nil), s.groups...)
	sort.Slice(out, func(i, j int) bool {
		if out[i] == DefaultGroup {
			return true
		}
		if out[j] == DefaultGroup {
			return false
		}
		return out[i] < out[j]
	})
	return out
}

// ValidateName 校验新分组名：非空、不超长、不重复、合法字符集。
// 字符集收窄到字母数字加 -_.：分组名会进 URL 路径与 JSON 键，
// 放开空格/中文只会让两端各自做一遍转义。
func (s *Store) ValidateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("分组名不能为空")
	}
	if name == DefaultGroup {
		return ErrImmutable
	}
	if r := []rune(name); len(r) > maxNameRunes {
		return fmt.Errorf("分组名过长（最多 %d 字符）", maxNameRunes)
	}
	for _, c := range name {
		ok := c == '-' || c == '_' || c == '.' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !ok {
			return fmt.Errorf("分组名只能包含字母、数字与 - _ .")
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.hasGroupLocked(name) {
		return fmt.Errorf("分组已存在")
	}
	return nil
}

// Create 新建分组。
func (s *Store) Create(name string) error {
	name = strings.TrimSpace(name)
	if err := s.ValidateName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.groups = append(s.groups, name)
	return s.persistLocked()
}

// Rename 重命名分组：同步迁移账号归属与密钥引用。default 不可改。
func (s *Store) Rename(oldName, newName string) error {
	if oldName == DefaultGroup {
		return ErrImmutable
	}
	if err := s.ValidateName(newName); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasGroupLocked(oldName) {
		return ErrNotFound
	}
	for i, g := range s.groups {
		if g == oldName {
			s.groups[i] = newName
			break
		}
	}
	for uid, gs := range s.accountOf {
		for i, g := range gs {
			if g == oldName {
				gs[i] = newName
			}
		}
		sort.Strings(gs)
		s.accountOf[uid] = gs
	}
	s.dirtyAcc = true
	for i := range s.keys {
		if s.keys[i].Group == oldName {
			s.keys[i].Group = newName
		}
	}
	s.dirtyKeys = true
	return s.persistLocked()
}

// Delete 删除分组：组内有账号或密钥引用时拒绝（先移出）。
// 账号归属引用删除的组时只解除引用本身（账号还可能在别的组）；
// "组内有账号"的判定 = 该组是某些账号的**唯一**归属。
func (s *Store) Delete(name string) error {
	if name == DefaultGroup {
		return ErrImmutable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasGroupLocked(name) {
		return ErrNotFound
	}
	for uid, gs := range s.accountOf {
		if len(gs) == 1 && gs[0] == name {
			return fmt.Errorf("账号 %s 仅属于分组 %s，请先把它移到其他分组%w", uid, name, ErrGroupInUse)
		}
	}
	for _, k := range s.keys {
		if k.Group == name {
			return fmt.Errorf("密钥 %q 仍绑定分组 %s，请先删除或改绑%w", k.Name, name, ErrGroupInUse)
		}
	}
	out := s.groups[:0]
	for _, g := range s.groups {
		if g != name {
			out = append(out, g)
		}
	}
	s.groups = out
	// 多归属账号解除对该组的引用。
	for uid, gs := range s.accountOf {
		next := make([]string, 0, len(gs))
		for _, g := range gs {
			if g != name {
				next = append(next, g)
			}
		}
		s.accountOf[uid] = next
	}
	s.dirtyAcc = true
	return s.persistLocked()
}

// —— 账号归属 ——

// AccountGroups 返回账号的归属分组；未登记的账号返回 [default]
// （旧数据兼容：分组功能上线前加的账号自动视为 default）。
func (s *Store) AccountGroups(uid string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.accountGroupsLocked(uid)
}

func (s *Store) accountGroupsLocked(uid string) []string {
	if gs, ok := s.accountOf[uid]; ok && len(gs) > 0 {
		return append([]string(nil), gs...)
	}
	return []string{DefaultGroup}
}

// SetAccountGroups 设置账号归属。空列表 = 清空归属（仅在管理密钥下可用）。
// 引用的分组必须存在；uid 本身是否在池里由调用方校验。
func (s *Store) SetAccountGroups(uid string, gs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	valid := map[string]bool{}
	for _, g := range s.groups {
		valid[g] = true
	}
	clean := cleanGroups(gs, valid)
	for _, g := range gs {
		g = strings.TrimSpace(g)
		if g != "" && !valid[g] {
			return fmt.Errorf("分组 %s 不存在", g)
		}
	}
	if _, ok := s.accountOf[uid]; !ok && len(clean) == 0 {
		// 新登记且清空：不必写文件（等价于不存在该键）。
		return nil
	}
	s.accountOf[uid] = clean
	s.dirtyAcc = true
	return s.persistLocked()
}

// AccountsInGroup 返回属于指定分组的账号 uid 集合（多归属账号会同时
// 出现在多个分组）。查询热路径用（请求选号过滤）。
func (s *Store) AccountsInGroup(group string) map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]bool{}
	for uid, gs := range s.accountOf {
		for _, g := range gs {
			if g == group {
				out[uid] = true
				break
			}
		}
	}
	return out
}

// RemoveAccount 账号被删（auths 文件移除）时清理归属记录。
func (s *Store) RemoveAccount(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accountOf[uid]; ok {
		delete(s.accountOf, uid)
		s.dirtyAcc = true
		_ = s.persistLocked()
	}
}

// MigrateUnregistered 把池里存在、但归属表没有记录的账号显式登记进
// default 分组并落盘。
//
// 为什么需要显式迁移：读取路径（AccountGroups）对未登记账号确实回落
// default，但**写路径全部跳过未登记账号**——AccountsInGroup（分组密钥
// 选号过滤）和 /admin/groups 的账号数统计都只看归属表。分组功能上线
// 前的存量账号会因此"看得见（回落）但选不着（不在表里）"：default
// 密钥按 default 组过滤时一个号都选不到。
//
// 返回补登记的账号数（0 = 无需迁移）。
func (s *Store) MigrateUnregistered(uids []string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	migrated := 0
	for _, uid := range uids {
		if uid == "" {
			continue
		}
		if _, ok := s.accountOf[uid]; !ok {
			s.accountOf[uid] = []string{DefaultGroup}
			migrated++
		}
	}
	if migrated == 0 {
		return 0
	}
	s.dirtyAcc = true
	if err := s.persistLocked(); err != nil {
		// 迁移失败不致命：读取路径的 default 回落仍然兜底。返回补登记数，
		// 调用方打日志即可。
		return migrated
	}
	return migrated
}

// SnapshotAccountGroups 导出全量归属（管理页展示用）。
func (s *Store) SnapshotAccountGroups() map[string][]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]string, len(s.accountOf))
	for uid, gs := range s.accountOf {
		out[uid] = append([]string(nil), gs...)
	}
	return out
}

// —— 密钥 ——

// KeyInfo 列表视图：完整密钥不回显（只在创建时给一次）。
type KeyInfo struct {
	ID        string    `json:"id"`
	Key       string    `json:"key"` // 脱敏：前 6 位 + …
	Name      string    `json:"name,omitempty"`
	Group     string    `json:"group,omitempty"` // 空 = 不限分组（管理密钥语义）
	CreatedAt time.Time `json:"created_at"`
}

func maskKey(k string) string {
	if len(k) <= 10 {
		return k[:4] + "…"
	}
	return k[:6] + "…" + k[len(k)-4:]
}

// ListKeys 返回密钥列表（脱敏）。
func (s *Store) ListKeys() []KeyInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]KeyInfo, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, KeyInfo{ID: k.ID, Key: maskKey(k.Key), Name: k.Name, Group: k.Group, CreatedAt: k.CreatedAt})
	}
	return out
}

// CreateKey 生成新密钥并绑定分组（空 = 不限）。返回完整密钥——
// 只在此处完整出现一次，列表/日志一律脱敏。
func (s *Store) CreateKey(name, group string) (Key, error) {
	if group != "" {
		s.mu.RLock()
		ok := s.hasGroupLocked(group)
		s.mu.RUnlock()
		if !ok {
			return Key{}, fmt.Errorf("分组 %s 不存在", group)
		}
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return Key{}, err
	}
	k := Key{
		ID:        hex.EncodeToString(b[:4]),
		Key:       "wk-" + hex.EncodeToString(b),
		Name:      strings.TrimSpace(name),
		Group:     strings.TrimSpace(group),
		CreatedAt: time.Now(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, k)
	s.dirtyKeys = true
	if err := s.persistLocked(); err != nil {
		return Key{}, err
	}
	return k, nil
}

// UpdateKey 改备注名或改绑分组。空 group = 不限分组。
func (s *Store) UpdateKey(id, name, group string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i, k := range s.keys {
		if k.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ErrNotFound
	}
	if group != "" && !s.hasGroupLocked(group) {
		return fmt.Errorf("分组 %s 不存在", group)
	}
	if name != "" {
		s.keys[idx].Name = strings.TrimSpace(name)
	}
	s.keys[idx].Group = strings.TrimSpace(group)
	s.dirtyKeys = true
	return s.persistLocked()
}

// DeleteKey 删除密钥。
func (s *Store) DeleteKey(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i, k := range s.keys {
		if k.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ErrNotFound
	}
	s.keys = append(s.keys[:idx], s.keys[idx+1:]...)
	s.dirtyKeys = true
	return s.persistLocked()
}

// LookupKey 按完整密钥查记录；不存在返回 ErrNotFound。
// 鉴权热路径用，RLock 只读。
func (s *Store) LookupKey(key string) (Key, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.keys {
		if k.Key == key {
			return k, nil
		}
	}
	return Key{}, ErrNotFound
}
