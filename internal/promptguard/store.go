package promptguard

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

type Rule struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Match              string `json:"match"`
	Replacement        string `json:"replacement"`
	Enabled            bool   `json:"enabled"`
	RequireEndBoundary bool   `json:"require_end_boundary,omitempty"`
	Source             string `json:"source,omitempty"`
}
type Settings struct {
	AutoLocate    bool `json:"auto_locate"`
	ProbeBudget   int  `json:"probe_budget"`
	RetentionDays int  `json:"retention_days"`
	MaxCases      int  `json:"max_cases"`
}
type Config struct {
	Revision               uint64   `json:"revision"`
	Settings               Settings `json:"settings"`
	Rules                  []Rule   `json:"rules"`
	LegacyMigrationVersion int      `json:"legacy_migration_version,omitempty"`
}
type ProbeResult struct {
	Outcome string `json:"outcome"` // pass, blocked, inconclusive
	HTTP    int    `json:"http"`
	Reason  string `json:"reason,omitempty"` // fixed, non-sensitive classification
	// nil denotes an older record whose request-attempt state was not recorded.
	RequestAttempted *bool `json:"request_attempted,omitempty"`
}
type Trace struct {
	Step       string `json:"step"`
	Characters int    `json:"characters"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	CachedFrom int    `json:"cached_from,omitempty"` // one-based trace index
	ProbeResult
}
type Job struct {
	Kind         string            `json:"kind"`
	Status       string            `json:"status"`
	Automatic    bool              `json:"automatic"`
	Calls        int               `json:"calls"`
	Budget       int               `json:"budget"`
	CacheHits    int               `json:"cache_hits,omitempty"`
	CurrentStep  string            `json:"current_step,omitempty"`
	EndReason    string            `json:"end_reason,omitempty"`
	Revision     uint64            `json:"revision"`
	Started      time.Time         `json:"started"`
	Finished     *time.Time        `json:"finished,omitempty"`
	Note         string            `json:"note"`
	Trace        []Trace           `json:"trace"`
	AccountScope *AccountScope     `json:"account_scope,omitempty"`
	Selection    *AccountSelection `json:"selection,omitempty"`
}
type Case struct {
	ID                 string        `json:"id"`
	Model              string        `json:"model"`
	UID                string        `json:"uid"`
	AccountScope       *AccountScope `json:"account_scope,omitempty"`
	Original           []Message     `json:"original,omitempty"`
	Outbound           []Message     `json:"outbound,omitempty"`
	Diagnostic         []Message     `json:"diagnostic,omitempty"`
	FirstSeen          time.Time     `json:"first_seen"`
	LastSeen           time.Time     `json:"last_seen"`
	Occurrences        int           `json:"occurrences"`
	Candidate          string        `json:"candidate,omitempty"`
	CandidateMinimal   bool          `json:"candidate_minimal"`
	CandidateRevision  uint64        `json:"candidate_revision,omitempty"`
	CandidateLocatedAt *time.Time    `json:"candidate_located_at,omitempty"`
	VerifiedRevision   uint64        `json:"verified_revision"`
	Resolved           bool          `json:"resolved"`
	Job                *Job          `json:"job,omitempty"`
}
type diskState struct {
	Config Config           `json:"config"`
	Cases  map[string]*Case `json:"cases"`
}
type Summary struct {
	Pending      int    `json:"pending"`
	Total        int    `json:"total"`
	Active       int    `json:"active"`
	AutoLocate   bool   `json:"auto_locate"`
	StorageError string `json:"storage_error,omitempty"`
}

// ProbeFactory creates a job-scoped probe (the server pins the account here).
// rewrite=false tests captured outbound text without applying rules a second time.
type Probe func(context.Context, []Message, bool) ProbeResult
type ProbeFactory func(Case) Probe

type Manager struct {
	mu             sync.Mutex
	state          diskState
	path           string
	aead           cipher.AEAD
	factory        ProbeFactory
	sessionFactory SessionFactory
	prepare        func(Case) ([]Message, error)
	cancel         map[string]context.CancelFunc
	active         bool
	closed         bool
	wg             sync.WaitGroup
	storageError   string
	stop           chan struct{}
}

func Open(path, secret string) (*Manager, error) {
	m := &Manager{path: path, stop: make(chan struct{}), cancel: map[string]context.CancelFunc{}, state: diskState{Config: Config{Revision: 1, Settings: Settings{ProbeBudget: 64, RetentionDays: 30, MaxCases: 200}, Rules: []Rule{}}, Cases: map[string]*Case{}}}
	if path != "" {
		if secret == "" {
			return nil, errors.New("prompt guard storage requires an administrator secret")
		}
		key := sha256.Sum256([]byte("workbuddy2api/prompt-guard/v1\x00" + secret))
		block, _ := aes.NewCipher(key[:])
		m.aead, _ = cipher.NewGCM(block)
		b, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil {
			if len(b) < m.aead.NonceSize() {
				return nil, errors.New("invalid prompt guard store")
			}
			plain, e := m.aead.Open(nil, b[:m.aead.NonceSize()], b[m.aead.NonceSize():], []byte("prompt-guard-v1"))
			if e != nil {
				return nil, errors.New("cannot decrypt prompt guard store; restore original administrator secret")
			}
			if json.Unmarshal(plain, &m.state) != nil || m.state.Cases == nil {
				return nil, errors.New("invalid prompt guard store")
			}
		}
	}
	for _, c := range m.state.Cases {
		if c.Job != nil && (c.Job.Status == "running" || c.Job.Status == "queued") {
			c.Job.Status = "interrupted"
			c.Job.Note = "服务重启，需手动重新启动诊断"
		}
	}
	if m.pruneLocked() {
		if err := m.saveLocked(); err != nil {
			return nil, err
		}
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		timer := time.NewTicker(time.Hour)
		defer timer.Stop()
		for {
			select {
			case <-m.stop:
				return
			case <-timer.C:
				m.mu.Lock()
				if m.pruneLocked() {
					_ = m.saveLocked()
				}
				m.mu.Unlock()
			}
		}
	}()
	return m, nil
}
func (m *Manager) SetProbeFactory(f ProbeFactory) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.factory = f
	m.sessionFactory = nil
}
func (m *Manager) SetPreparer(f func(Case) ([]Message, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prepare = f
}
func (m *Manager) Close() {
	m.mu.Lock()
	if !m.closed {
		close(m.stop)
	}
	m.closed = true
	for _, c := range m.cancel {
		c()
	}
	m.mu.Unlock()
	m.wg.Wait()
}
func copyJSON[T any](v T) T {
	b, _ := json.Marshal(v)
	var out T
	_ = json.Unmarshal(b, &out)
	return out
}
func (m *Manager) Config() Config { m.mu.Lock(); defer m.mu.Unlock(); return copyJSON(m.state.Config) }
func (m *Manager) Apply(doc map[string]any) error {
	m.mu.Lock()
	rules := append([]Rule{}, m.state.Config.Rules...)
	m.mu.Unlock()
	return ApplyToDocument(doc, rules)
}

// Invalidate also covers changes to the legacy compatibility pipeline.
func (m *Manager) Invalidate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.Config.Revision++
	for _, c := range m.state.Cases {
		c.Resolved = false
	}
	_ = m.saveLocked()
}

func (m *Manager) Update(c Config) error {
	if c.Settings.ProbeBudget < 4 || c.Settings.ProbeBudget > 256 || c.Settings.RetentionDays < 1 || c.Settings.RetentionDays > 365 || c.Settings.MaxCases < 1 || c.Settings.MaxCases > 1000 {
		return errors.New("请求预算须为 4–256、保留天数 1–365、记录上限 1–1000")
	}
	if len(c.Rules) > 256 {
		return errors.New("最多 256 条替换规则")
	}
	ids := map[string]bool{}
	for i := range c.Rules {
		r := &c.Rules[i]
		if r.Match == "" || len(r.Match) > 1<<20 || len(r.Replacement) > 1<<20 || len(r.Name) > 200 || len(r.Source) > 100 {
			return errors.New("匹配内容不可为空；单条匹配/替换最多 1 MiB")
		}
		if r.ID == "" {
			b := make([]byte, 12)
			if _, err := rand.Read(b); err != nil {
				return err
			}
			r.ID = hex.EncodeToString(b)
		}
		if ids[r.ID] {
			return errors.New("规则 ID 重复")
		}
		ids[r.ID] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.Revision != m.state.Config.Revision {
		return errors.New("配置已被更新，请刷新后重试")
	}
	old := copyJSON(m.state)
	// This is a durable migration marker, not an administrator-editable setting.
	c.LegacyMigrationVersion = m.state.Config.LegacyMigrationVersion
	rulesChanged := !slices.Equal(c.Rules, m.state.Config.Rules)
	c.Revision++
	m.state.Config = copyJSON(c)
	// Retention and diagnostic budgets do not change outbound prompt content.
	if rulesChanged {
		for _, item := range m.state.Cases {
			item.Resolved = false
		}
	}
	m.pruneLocked()
	if err := m.saveLocked(); err != nil {
		m.state = old
		return err
	}
	if !c.Settings.AutoLocate {
		for id, cancel := range m.cancel {
			if item := m.state.Cases[id]; item != nil && item.Job.Automatic {
				cancel()
			}
		}
		for _, item := range m.state.Cases {
			if item.Job != nil && item.Job.Status == "queued" {
				item.Job.Status = "cancelled"
				item.Job.Note = "自动定位已关闭"
			}
		}
		_ = m.saveLocked()
	}
	return nil
}
func (m *Manager) Summary() Summary {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pruneLocked() {
		_ = m.saveLocked()
	}
	s := Summary{Total: len(m.state.Cases), AutoLocate: m.state.Config.Settings.AutoLocate, StorageError: m.storageError}
	for _, c := range m.state.Cases {
		if !c.Resolved {
			s.Pending++
		}
		if c.Job != nil && (c.Job.Status == "running" || c.Job.Status == "queued") {
			s.Active++
		}
	}
	return s
}
func (m *Manager) List() []Case {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pruneLocked() {
		_ = m.saveLocked()
	}
	out := make([]Case, 0, len(m.state.Cases))
	for _, c := range m.state.Cases {
		v := *c
		v.Original = nil
		v.Outbound = nil
		v.Diagnostic = nil
		v.Candidate = ""
		if v.AccountScope != nil {
			scope := *v.AccountScope
			scope.Accounts = nil
			v.AccountScope = &scope
		}
		if v.Job != nil {
			job := *v.Job
			job.Trace = nil
			if job.AccountScope != nil {
				scope := *job.AccountScope
				scope.Accounts = nil
				job.AccountScope = &scope
			}
			v.Job = &job
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}
func (m *Manager) Get(id string) (Case, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.state.Cases[id]
	if !ok {
		return Case{}, false
	}
	return copyJSON(*c), true
}
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.state.Cases[id]; c != nil && c.Job != nil && (c.Job.Status == "running" || c.Job.Status == "queued") {
		return errors.New("请先取消正在运行或排队的诊断")
	}
	old := m.state.Cases[id]
	delete(m.state.Cases, id)
	if err := m.saveLocked(); err != nil {
		m.state.Cases[id] = old
		return err
	}
	return nil
}
func (m *Manager) Capture(model, uid string, original, outbound []Message) (string, error) {
	return m.capture(model, uid, original, outbound, nil)
}

func (m *Manager) CaptureScoped(model, uid string, original, outbound []Message, scope AccountScope) (string, error) {
	normalized, err := normalizedScope(scope)
	if err != nil {
		return "", err
	}
	return m.capture(model, uid, original, outbound, &normalized)
}

func (m *Manager) capture(model, uid string, original, outbound []Message, scope *AccountScope) (string, error) {
	b, _ := json.Marshal([]any{model, original, outbound})
	if scope != nil {
		// Group identity participates in dedup; member snapshots may change.
		b, _ = json.Marshal([]any{model, original, outbound, scope.Kind, scope.Group})
	}
	hash := sha256.Sum256(b)
	id := hex.EncodeToString(hash[:16])
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", errors.New("prompt guard closed")
	}
	m.pruneLocked()
	now := time.Now().UTC()
	c, exists := m.state.Cases[id]
	var previous *Case
	if exists {
		v := copyJSON(*c)
		previous = &v
	}
	if exists {
		c.Occurrences++
		c.UID = uid
		c.AccountScope = copyJSON(scope)
		c.LastSeen = now
		c.Resolved = false
	} else {
		c = &Case{ID: id, Model: model, UID: uid, Original: Clone(original), Outbound: Clone(outbound), FirstSeen: now, LastSeen: now, Occurrences: 1}
		c.AccountScope = copyJSON(scope)
		m.state.Cases[id] = c
	}
	m.pruneLocked()
	var err error
	if m.state.Cases[id] == nil {
		m.storageError = "诊断队列已满，无法保存新提示词；请取消任务或提高记录上限"
		err = errors.New("prompt guard case capacity exceeded")
	} else {
		err = m.saveLocked()
	}
	if err != nil {
		if exists {
			m.state.Cases[id] = previous
		} else {
			delete(m.state.Cases, id)
		}
	}
	// Only a newly captured case schedules automatic paid work. Repeated
	// failures do not create an unbounded loop or resume interrupted jobs.
	auto := err == nil && !exists && m.state.Config.Settings.AutoLocate
	m.mu.Unlock()
	if auto {
		_, _ = m.Start(id, "locate", true)
	}
	return id, err
}
func (m *Manager) pruneLocked() bool {
	before := len(m.state.Cases)
	cut := time.Now().Add(-time.Duration(m.state.Config.Settings.RetentionDays) * 24 * time.Hour)
	for id, c := range m.state.Cases {
		if _, active := m.cancel[id]; !active && (c.Job == nil || c.Job.Status != "queued") && c.LastSeen.Before(cut) {
			delete(m.state.Cases, id)
		}
	}
	for len(m.state.Cases) > m.state.Config.Settings.MaxCases {
		old := ""
		for id, c := range m.state.Cases {
			if _, active := m.cancel[id]; !active && (c.Job == nil || c.Job.Status != "queued") && (old == "" || c.LastSeen.Before(m.state.Cases[old].LastSeen)) {
				old = id
			}
		}
		if old == "" {
			break
		}
		delete(m.state.Cases, old)
	}
	return before != len(m.state.Cases)
}
func (m *Manager) saveLocked() error {
	if m.path == "" {
		return nil
	}
	plain, err := json.Marshal(m.state)
	if err == nil && len(plain) > 64<<20 {
		err = errors.New("prompt guard storage exceeds 64 MiB; delete old records")
	}
	if err == nil {
		nonce := make([]byte, m.aead.NonceSize())
		_, err = rand.Read(nonce)
		if err == nil {
			data := m.aead.Seal(nonce, nonce, plain, []byte("prompt-guard-v1"))
			err = os.MkdirAll(filepath.Dir(m.path), 0700)
			if err == nil {
				var f *os.File
				f, err = os.CreateTemp(filepath.Dir(m.path), ".prompt-guard-*")
				if err == nil {
					name := f.Name()
					defer os.Remove(name)
					if err = f.Chmod(0600); err == nil {
						_, err = f.Write(data)
					}
					if err == nil {
						err = f.Sync()
					}
					closeErr := f.Close()
					if err == nil {
						err = closeErr
					}
					if err == nil {
						err = os.Rename(name, m.path)
					}
				}
			}
		}
	}
	if err != nil {
		m.storageError = "诊断存储失败，请检查磁盘、权限或 64 MiB 上限"
		return fmt.Errorf("prompt guard persistence failed")
	}
	m.storageError = ""
	return nil
}

func (m *Manager) Preview(messages []Message) []Message {
	doc := map[string]any{"messages": []any{}}
	for _, v := range Clone(messages) {
		doc["messages"] = append(doc["messages"].([]any), map[string]any{"role": v.Role, "content": v.Content})
	}
	m.Apply(doc)
	return Extract(doc)
}

func safeReason(r ProbeResult) ProbeResult {
	if r.Outcome != "pass" && r.Outcome != "blocked" {
		r.Outcome = "inconclusive"
	}
	// Probe adapters must not return upstream bodies (which may echo secrets).
	if len(r.Reason) > 120 || strings.ContainsAny(r.Reason, "\r\n") {
		r.Reason = "probe_error"
	}
	return r
}
