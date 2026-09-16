// Package groups 账号分组与 API 密钥的持久化存储。
//
// 两组数据都以独立 JSON 文件落盘（data/ 目录），与 pool 的 state.json、
// auths/ 的凭证文件互不干扰：
//
//   - groups.json:       ["default", "vip", ...]（default 恒存在、不可改删）
//   - account-groups.json: {"<uid>": ["default", "vip"], ...}（账号多归属）
//   - keys.json:         [{"id","key","name","group","created_at"}, ...]
//
// 所有方法并发安全（单 RWMutex）。文件缺失时按空数据初始化并补写 default。
package groups

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultGroup 默认分组名。永远存在、不可编辑、不可删除。
const DefaultGroup = "default"

// maxNameRunes 分组名的长度上限（rune 数）。
const maxNameRunes = 32

// ErrNotFound 操作的目标不存在。
var ErrNotFound = errors.New("not found")

// ErrImmutable 操作的目标不可变更（default 分组）。
var ErrImmutable = errors.New("default 分组不可编辑或删除")

// ErrGroupInUse 删除分组时组内仍有账号。
var ErrGroupInUse = errors.New("分组内仍有账号，请先移出")

// Store 分组 + 账号归属 + 密钥的联合存储。
type Store struct {
	mu sync.RWMutex

	fp        string // groups.json 路径（目录即数据目录）
	groups    []string
	accountOf map[string][]string // uid -> groups（去重有序）
	dirtyAcc  bool
	accFp     string // account-groups.json 路径
	keys      []Key
	keysFp    string // keys.json 路径
	dirtyKeys bool
}

// Key 一条 API 密钥记录。
type Key struct {
	ID        string    `json:"id"`    // 短 id（列表展示/删除用）
	Key       string    `json:"key"`   // 完整密钥（仅创建时完整回显一次；列表脱敏）
	Name      string    `json:"name"`  // 备注名（可选）
	Group     string    `json:"group"` // 绑定分组；空 = 不限（管理密钥语义）
	CreatedAt time.Time `json:"created_at"`
}

// New 打开或初始化存储。dir 为数据目录；文件不存在时创建含 default 的初始状态。
func New(dir string) (*Store, error) {
	s := &Store{
		fp:        filepath.Join(dir, "groups.json"),
		accFp:     filepath.Join(dir, "account-groups.json"),
		keysFp:    filepath.Join(dir, "keys.json"),
		accountOf: map[string][]string{},
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := s.loadAll(); err != nil {
		return nil, err
	}
	// 恒有 default：文件缺失/被删后自动补齐（这就是"默认分组不可删除"的
	// 落地：删除接口拒绝之外，加载层也兜底）。
	if !s.hasGroupLocked(DefaultGroup) {
		s.groups = append(s.groups, DefaultGroup)
	}
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// —— 加载 ——

func (s *Store) loadAll() error {
	if raw, err := os.ReadFile(s.fp); err == nil {
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil {
			return fmt.Errorf("groups.json 损坏: %w", err)
		}
		s.groups = list
	} else if !os.IsNotExist(err) {
		return err
	}
	if raw, err := os.ReadFile(s.accFp); err == nil {
		m := map[string][]string{}
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("account-groups.json 损坏: %w", err)
		}
		// 清洗：去掉不存在的分组引用、去重排序，保证不变量。
		valid := map[string]bool{}
		for _, g := range s.groups {
			valid[g] = true
		}
		for uid, gs := range m {
			m[uid] = cleanGroups(gs, valid)
		}
		s.accountOf = m
	} else if !os.IsNotExist(err) {
		return err
	}
	if raw, err := os.ReadFile(s.keysFp); err == nil {
		var list []Key
		if err := json.Unmarshal(raw, &list); err != nil {
			return fmt.Errorf("keys.json 损坏: %w", err)
		}
		// 清洗：密钥绑定的分组被删后回落 default（删除接口会拒绝有密钥
		// 引用的分组，这里只是对旧数据/手改文件的兜底）。
		valid := map[string]bool{}
		for _, g := range s.groups {
			valid[g] = true
		}
		for i := range list {
			if list[i].Group != "" && !valid[list[i].Group] {
				list[i].Group = DefaultGroup
			}
		}
		s.keys = list
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// cleanGroups 过滤到有效分组、去重、排序。
func cleanGroups(in []string, valid map[string]bool) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, g := range in {
		if g = strings.TrimSpace(g); g == "" || !valid[g] || seen[g] {
			continue
		}
		seen[g] = true
		out = append(out, g)
	}
	sort.Strings(out)
	if len(out) == 0 {
		// 归属清空的账号维持空列表（= 不属于任何组，只在管理密钥下可用）。
		return []string{}
	}
	return out
}

// persistLocked 落盘所有脏文件。调用方必须持锁。
func (s *Store) persistLocked() error {
	if raw, err := json.MarshalIndent(s.groups, "", "  "); err == nil {
		if err := writeFileAtomic(s.fp, append(raw, '\n')); err != nil {
			return fmt.Errorf("写 groups.json: %w", err)
		}
	}
	if s.dirtyAcc || s.accountOf != nil {
		if raw, err := json.MarshalIndent(s.accountOf, "", "  "); err == nil {
			if err := writeFileAtomic(s.accFp, append(raw, '\n')); err != nil {
				return fmt.Errorf("写 account-groups.json: %w", err)
			}
		}
		s.dirtyAcc = false
	}
	if s.dirtyKeys || s.keys != nil {
		if raw, err := json.MarshalIndent(s.keys, "", "  "); err == nil {
			if err := writeFileAtomic(s.keysFp, append(raw, '\n')); err != nil {
				return fmt.Errorf("写 keys.json: %w", err)
			}
		}
		s.dirtyKeys = false
	}
	return nil
}

// writeFileAtomic 先写临时文件再 rename：进程随时可能被 SIGKILL（容器
// 重启就是），半个 JSON 会让下次启动读不出来。
func writeFileAtomic(fp string, data []byte) error {
	tmp := fp + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, fp)
}

func (s *Store) hasGroupLocked(name string) bool {
	for _, g := range s.groups {
		if g == name {
			return true
		}
	}
	return false
}
