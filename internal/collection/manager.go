package collection

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/lifecycle"
)

const historyLimit = 100

type work struct {
	upgradeDone func()
	target      Target
	subscribers map[string]*Item
	running     bool
	manual      bool
	allow       bool
	version     uint64
	previous    *work
	finished    bool
}
type Manager struct {
	mu           sync.Mutex
	opt          Options
	settings     Settings
	jobs         map[string]*Job
	snapshots    map[string]Snapshot
	works        map[string]*work
	versions     map[string]uint64
	queue        []*work
	running      int
	wake         chan struct{}
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	closed       bool
	storageError string
	rateMu       sync.Mutex
	nextRequest  time.Time
}

func New(opt Options) (*Manager, error) {
	if opt.Store == nil || opt.Run == nil {
		return nil, fmt.Errorf("采集需要持久化存储和采集器")
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{opt: opt, settings: DefaultSettings(), jobs: map[string]*Job{}, snapshots: map[string]Snapshot{}, works: map[string]*work{}, versions: map[string]uint64{}, wake: make(chan struct{}, 1), ctx: ctx, cancel: cancel}
	records, err := opt.Store.CollectionLoad()
	if err != nil {
		cancel()
		return nil, err
	}
	changed := map[string][]byte{}
	for key, raw := range records {
		switch {
		case key == "settings":
			if json.Unmarshal(raw, &m.settings) != nil || m.settings.Validate() != nil {
				cancel()
				return nil, fmt.Errorf("invalid persisted collection settings")
			}
		case strings.HasPrefix(key, "job/"):
			var j Job
			if err = json.Unmarshal(raw, &j); err != nil {
				cancel()
				return nil, err
			}
			m.jobs[j.ID] = &j
		case strings.HasPrefix(key, "snapshot/"):
			var s Snapshot
			if err = json.Unmarshal(raw, &s); err != nil {
				cancel()
				return nil, err
			}
			m.snapshots[s.Target.key()] = s
		}
	}
	for key, raw := range records {
		if strings.HasPrefix(key, "item/") {
			var i Item
			if err = json.Unmarshal(raw, &i); err != nil {
				cancel()
				return nil, err
			}
			id := strings.Split(key, "/")[1]
			if j := m.jobs[id]; j != nil {
				j.Items = append(j.Items, &i)
			}
		}
	}
	for _, j := range m.jobs {
		if j.Status == "queued" || j.Status == "running" || j.Status == "cancelling" {
			for _, i := range j.Items {
				if !terminal(i.Status) {
					i.Status = "interrupted"
					i.Error = "服务重启，未自动重放"
					i.FinishedAt = time.Now().Unix()
					changed[itemKey(j.ID, i.Target)] = encode(i)
				}
			}
			m.count(j)
			j.Status = "interrupted"
			j.FinishedAt = time.Now().Unix()
			changed["job/"+j.ID] = jobJSON(j)
		}
	}
	for k, s := range m.snapshots {
		if s.State == "queued" || s.State == "running" || s.State == "backoff" {
			s.State = "interrupted"
			s.Error = "上次采集被服务重启中断"
			s.NextRetryAt = 0
			m.snapshots[k] = s
			changed["snapshot/"+k] = encode(s)
		}
	}
	if err = opt.Store.CollectionSave(changed, nil); err != nil {
		cancel()
		return nil, err
	}
	m.wg.Add(1)
	go m.loop()
	return m, nil
}
func encode(v any) []byte                { b, _ := json.Marshal(v); return b }
func jobJSON(j *Job) []byte              { c := *j; c.Items = nil; return encode(c) }
func itemKey(id string, t Target) string { return "item/" + id + "/" + t.key() }
func terminal(s string) bool {
	return s == "success" || s == "failed" || s == "skipped" || s == "cancelled" || s == "interrupted"
}
func (m *Manager) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}
func (m *Manager) save(rows map[string][]byte, deletes []string) error {
	err := m.opt.Store.CollectionSave(rows, deletes)
	if err != nil {
		m.storageError = "采集记录写入失败，请检查数据库"
		return fmt.Errorf("%s", m.storageError)
	} else {
		m.storageError = ""
	}
	return err
}
func (m *Manager) count(j *Job) {
	j.Total = len(j.Items)
	j.Done = 0
	j.Running = 0
	j.Success = 0
	j.Failed = 0
	j.Skipped = 0
	j.Cancelled = 0
	for _, i := range j.Items {
		switch i.Status {
		case "success":
			j.Success++
		case "failed", "interrupted":
			j.Failed++
		case "skipped":
			j.Skipped++
		case "cancelled":
			j.Cancelled++
		case "running", "backoff":
			j.Running++
		}
		if terminal(i.Status) {
			j.Done++
		}
	}
	if j.Done == j.Total {
		if j.Status == "cancelling" {
			j.Status = "cancelled"
		} else if j.Failed > 0 {
			j.Status = "failed"
		} else {
			j.Status = "completed"
		}
		j.FinishedAt = time.Now().Unix()
	} else if j.Running > 0 {
		if j.Status != "cancelling" {
			j.Status = "running"
		}
	}
}
func (m *Manager) Submit(req Request) (Job, error) {
	done, admitted := lifecycle.Default.Enter()
	if !admitted {
		return Job{}, fmt.Errorf("服务正在排空")
	}
	defer done()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Job{}, fmt.Errorf("采集器已停止")
	}
	if len(m.works) > 10000 {
		return Job{}, fmt.Errorf("采集队列已满")
	}
	active := 0
	for _, j := range m.jobs {
		if !terminalJob(j.Status) {
			active++
		}
	}
	if active >= 20 {
		return Job{}, fmt.Errorf("最多允许 20 个进行中的采集任务")
	}
	if req.Source == "" {
		req.Source = "manual"
	}
	targets := req.Targets
	if targets == nil {
		if len(req.UIDs) == 0 && m.opt.UIDs != nil {
			req.UIDs = m.opt.UIDs()
		}
		for _, uid := range req.UIDs {
			for _, r := range req.Resources {
				targets = append(targets, Target{uid, r})
			}
		}
	}
	if len(targets) == 0 || len(targets) > 10000 {
		return Job{}, fmt.Errorf("请选择账号和资源，每次最多 10000 项")
	}
	seen := map[string]bool{}
	unique := []Target{}
	for _, t := range targets {
		valid := false
		for _, r := range Resources {
			if t.Resource == r {
				valid = true
			}
		}
		if !valid || t.UID == "" || strings.ContainsAny(t.UID, "/\\\x00") {
			return Job{}, fmt.Errorf("无效账号或资源")
		}
		if !seen[t.key()] {
			unique = append(unique, t)
			seen[t.key()] = true
		}
	}
	additional := 0
	for _, t := range unique {
		w := m.works[t.key()]
		if w == nil || w.version != m.versions[t.key()] {
			additional++
		}
	}
	if len(m.queue)+m.running+additional > 10000 {
		return Job{}, fmt.Errorf("采集队列已满，请稍后重试")
	}
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return Job{}, err
	}
	j := &Job{ID: hex.EncodeToString(buf), Source: req.Source, Status: "queued", CreatedAt: time.Now().Unix(), IncludeDisabled: req.IncludeDisabled}
	rows := map[string][]byte{}
	for _, t := range unique {
		i := &Item{Target: t, Status: "queued"}
		j.Items = append(j.Items, i)
		if m.opt.Eligible != nil && !m.opt.Eligible(t.UID, req.IncludeDisabled) {
			i.Status = "skipped"
			i.Error = "账号不存在、已禁用或不适用"
			i.FinishedAt = time.Now().Unix()
		}
		rows[itemKey(j.ID, t)] = encode(i)
	}
	m.count(j)
	rows["job/"+j.ID] = jobJSON(j)
	if err := m.save(rows, nil); err != nil {
		return Job{}, err
	}
	m.jobs[j.ID] = j
	for _, i := range j.Items {
		if terminal(i.Status) {
			continue
		}
		key := i.Target.key()
		w := m.works[key]
		if w == nil || w.version != m.versions[key] {
			previous := w
			w = &work{upgradeDone: lifecycle.Default.Track(), previous: previous, target: i.Target, subscribers: map[string]*Item{}, manual: req.Source == "manual", allow: req.IncludeDisabled, version: m.versions[key]}
			m.works[key] = w
			m.queue = append(m.queue, w)
		}
		w.subscribers[j.ID] = i
		if !w.running {
			w.allow = w.allow || req.IncludeDisabled
		}
		w.manual = w.manual || req.Source == "manual"
		if w.running {
			i.Status = "running"
		}
		s := m.snapshots[key]
		s.Target = i.Target
		if !w.running {
			s.State = "queued"
		}
		m.snapshots[key] = s
	}
	m.count(j)
	updates := map[string][]byte{"job/" + j.ID: jobJSON(j)}
	for _, i := range j.Items {
		updates[itemKey(j.ID, i.Target)] = encode(i)
		if s, ok := m.snapshots[i.Target.key()]; ok {
			updates["snapshot/"+i.Target.key()] = encode(s)
		}
	}
	_ = m.save(updates, nil)
	m.prune()
	m.poke()
	return cloneJob(j, false), nil
}
func terminalJob(s string) bool { return s != "queued" && s != "running" && s != "cancelling" }
func (m *Manager) prune() {
	jobs := m.list()
	rows := map[string][]byte{}
	del := []string{}
	for n, j := range jobs {
		if n < historyLimit || !terminalJob(j.Status) {
			continue
		}
		del = append(del, "job/"+j.ID)
		for _, i := range m.jobs[j.ID].Items {
			del = append(del, itemKey(j.ID, i.Target))
		}
		delete(m.jobs, j.ID)
	}
	if len(del) > 0 {
		_ = m.save(rows, del)
	}
}
func cloneJob(j *Job, detail bool) Job {
	c := *j
	c.Items = nil
	if detail {
		for _, i := range j.Items {
			v := *i
			c.Items = append(c.Items, &v)
		}
	}
	return c
}
func (m *Manager) list() []Job {
	out := []Job{}
	for _, j := range m.jobs {
		out = append(out, cloneJob(j, false))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt == out[j].CreatedAt {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt > out[j].CreatedAt
	})
	return out
}
func (m *Manager) List() ([]Job, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.list(), m.storageError
}
func (m *Manager) Job(id string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Job{}, false
	}
	return cloneJob(j, true), true
}
func (m *Manager) Snapshot(uid, r string) Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.snapshots[(Target{uid, r}).key()]
	s.Target = Target{uid, r}
	if s.State == "" {
		s.State = "unknown"
	}
	s.Stale = s.Invalidated || s.LastSuccess == 0 || time.Now().Unix()-s.LastSuccess >= int64(m.settings.Intervals[r])
	s.Data = append(json.RawMessage(nil), s.Data...)
	return s
}
func (m *Manager) Settings() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.settings
	s.Intervals = map[string]int{}
	for k, v := range m.settings.Intervals {
		s.Intervals[k] = v
	}
	return s
}

// Forget invalidates all snapshots on credential replacement/deletion. Any
// response already in flight belongs to the previous account generation.
func (m *Manager) Forget(uid string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	del := []string{}
	for _, r := range Resources {
		k := (Target{uid, r}).key()
		m.versions[k]++
		delete(m.snapshots, k)
		del = append(del, "snapshot/"+k)
	}
	_ = m.save(nil, del)
}

func (m *Manager) Invalidate(uid string, resources []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := map[string][]byte{}
	for _, r := range resources {
		t := Target{uid, r}
		key := t.key()
		m.versions[key]++
		snap := m.snapshots[key]
		snap.Target = t
		snap.Invalidated = true
		m.snapshots[key] = snap
		rows["snapshot/"+key] = encode(snap)
	}
	_ = m.save(rows, nil)
}
func (m *Manager) Configure(s Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	intervals := map[string]int{}
	for k, v := range s.Intervals {
		intervals[k] = v
	}
	s.Intervals = intervals
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.save(map[string][]byte{"settings": encode(s)}, nil); err != nil {
		return err
	}
	m.settings = s
	m.poke()
	return nil
}

// WaitRequest throttles every actual HTTP call, including usage pages and retries.
func (m *Manager) WaitRequest(ctx context.Context) error {
	m.mu.Lock()
	rate := m.settings.RequestsPerSecond
	m.mu.Unlock()
	m.rateMu.Lock()
	now := time.Now()
	at := m.nextRequest
	if at.Before(now) {
		at = now
	}
	m.nextRequest = at.Add(time.Second / time.Duration(rate))
	m.rateMu.Unlock()
	t := time.NewTimer(time.Until(at))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func (m *Manager) Cancel(id string) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	if j == nil {
		return Job{}, fmt.Errorf("任务不存在")
	}
	if terminalJob(j.Status) {
		return cloneJob(j, false), nil
	}
	rows := map[string][]byte{}
	j.Status = "cancelling"
	for _, i := range j.Items {
		if i.Status != "queued" {
			continue
		}
		i.Status = "cancelled"
		i.FinishedAt = time.Now().Unix()
		rows[itemKey(id, i.Target)] = encode(i)
		if w := m.works[i.Target.key()]; w != nil {
			delete(w.subscribers, id)
		}
	}
	m.count(j)
	rows["job/"+id] = jobJSON(j)
	err := m.save(rows, nil)
	m.poke()
	return cloneJob(j, false), err
}
func (m *Manager) Retry(id string) (Job, error) {
	j, ok := m.Job(id)
	if !ok {
		return Job{}, fmt.Errorf("任务不存在")
	}
	if !terminalJob(j.Status) {
		return Job{}, fmt.Errorf("请等待任务结束")
	}
	req := Request{Source: "manual", IncludeDisabled: j.IncludeDisabled, Targets: []Target{}}
	for _, i := range j.Items {
		if i.Status == "failed" || i.Status == "interrupted" {
			req.Targets = append(req.Targets, i.Target)
		}
	}
	return m.Submit(req)
}
func (m *Manager) Close() {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		m.cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
	m.mu.Lock()
	for _, w := range m.queue {
		if w.upgradeDone != nil {
			w.upgradeDone()
		}
	}
	m.mu.Unlock()
}
func (m *Manager) loop() {
	defer m.wg.Done()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.Schedule("scheduled")
		case <-m.wake:
		}
		m.mu.Lock()
		for !m.closed && m.running < m.settings.Concurrency {
			index := -1
			for n, w := range m.queue {
				if w.previous != nil && !w.previous.finished {
					continue
				}
				if index < 0 || w.manual && !m.queue[index].manual {
					index = n
				}
			}
			if index < 0 {
				break
			}
			w := m.queue[index]
			m.queue = append(m.queue[:index], m.queue[index+1:]...)
			if len(w.subscribers) == 0 {
				if w.upgradeDone != nil {
					w.upgradeDone()
				}
				w.finished = true
				if m.works[w.target.key()] == w {
					delete(m.works, w.target.key())
				}
				if w.version == m.versions[w.target.key()] {
					s := m.snapshots[w.target.key()]
					s.State = "cancelled"
					m.snapshots[w.target.key()] = s
					_ = m.save(map[string][]byte{"snapshot/" + w.target.key(): encode(s)}, nil)
				}
				continue
			}
			w.running = true
			w.previous = nil
			for _, i := range w.subscribers {
				i.Status = "running"
			}
			m.running++
			m.wg.Add(1)
			go m.execute(w)
		}
		m.mu.Unlock()
	}
}
func (m *Manager) update(w *work, state string, attempt int, f *Failure) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := map[string][]byte{}
	now := time.Now().Unix()
	s := m.snapshots[w.target.key()]
	s.State = state
	s.LastAttempt = now
	s.NextRetryAt = 0
	s.Error = ""
	s.ErrorKind = ""
	if f != nil {
		s.Error = f.Message
		s.ErrorKind = f.Kind
		if state == "backoff" {
			s.NextRetryAt = time.Now().Add(f.After).Unix()
		}
	}
	if w.version == m.versions[w.target.key()] {
		m.snapshots[w.target.key()] = s
		rows["snapshot/"+w.target.key()] = encode(s)
	}
	for id, i := range w.subscribers {
		i.Status = state
		i.Error = s.Error
		i.ErrorKind = s.ErrorKind
		i.Attempts = attempt
		i.NextRetryAt = s.NextRetryAt
		if i.StartedAt == 0 {
			i.StartedAt = now
		}
		rows[itemKey(id, i.Target)] = encode(i)
		m.count(m.jobs[id])
		rows["job/"+id] = jobJSON(m.jobs[id])
	}
	_ = m.save(rows, nil)
}
func (m *Manager) execute(w *work) {
	if w.upgradeDone != nil {
		defer w.upgradeDone()
	}
	defer m.wg.Done()
	var data json.RawMessage
	var err error
	attempt := 0
	ctx, cancel := context.WithTimeout(m.ctx, 2*time.Minute)
	defer cancel()
	for attempt < 3 {
		attempt++
		m.update(w, "running", attempt, nil)
		m.mu.Lock()
		valid := w.version == m.versions[w.target.key()]
		m.mu.Unlock()
		if !valid {
			err = &Failure{Kind: "superseded", Message: "账号凭证已替换，旧任务未执行", Skip: true}
			break
		}
		if m.opt.Eligible != nil && !m.opt.Eligible(w.target.UID, w.allow) {
			err = &Failure{Kind: "ineligible", Message: "账号已删除或禁用，未查询", Skip: true}
			break
		}
		data, err = m.run(ctx, w.target)
		if err == nil {
			break
		}
		var f *Failure
		if !errors.As(err, &f) || !f.Retryable || attempt == 3 {
			break
		}
		if f.After <= 0 {
			f.After = time.Duration(1<<attempt) * time.Second
		}
		m.update(w, "backoff", attempt, f)
		t := time.NewTimer(f.After)
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-t.C:
		}
		t.Stop()
		if ctx.Err() != nil {
			break
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.running--
	w.finished = true
	if m.works[w.target.key()] == w {
		delete(m.works, w.target.key())
	}
	rows := map[string][]byte{}
	s := m.snapshots[w.target.key()]
	s.NextRetryAt = 0
	s.Error = ""
	s.ErrorKind = ""
	s.State = "success"
	if w.version != m.versions[w.target.key()] {
		err = &Failure{Kind: "superseded", Message: "账号已删除或凭证已替换，丢弃旧结果", Skip: true}
	}
	if err == nil {
		s.Invalidated = false
		s.Data = data
		s.LastSuccess = time.Now().Unix()
	} else {
		s.State = "failed"
		s.Error = "采集失败"
		s.ErrorKind = "internal"
		var f *Failure
		if errors.As(err, &f) {
			s.Error = f.Message
			s.ErrorKind = f.Kind
			if f.Skip {
				s.State = "skipped"
			}
		}
		if ctx.Err() != nil {
			s.ErrorKind = "timeout"
			s.Error = "采集超时或服务停止"
		}
		if m.ctx.Err() != nil {
			s.State = "interrupted"
		}
	}
	if w.version == m.versions[w.target.key()] {
		m.snapshots[w.target.key()] = s
		rows["snapshot/"+w.target.key()] = encode(s)
	}
	for id, i := range w.subscribers {
		i.Status = s.State
		i.Error = s.Error
		i.ErrorKind = s.ErrorKind
		i.NextRetryAt = 0
		i.FinishedAt = time.Now().Unix()
		rows[itemKey(id, i.Target)] = encode(i)
		j := m.jobs[id]
		m.count(j)
		rows["job/"+id] = jobJSON(j)
	}
	_ = m.save(rows, nil)
	m.poke()
}

func (m *Manager) run(ctx context.Context, t Target) (data json.RawMessage, err error) {
	defer func() {
		if recover() != nil {
			err = &Failure{Kind: "internal", Message: "采集器内部错误"}
		}
	}()
	data, err = m.opt.Run(ctx, t, m.Snapshot(t.UID, t.Resource))
	if err == nil && len(data) > 0 && !json.Valid(data) {
		err = &Failure{Kind: "invalid_response", Message: "采集器返回无效快照"}
	}
	return
}
func (m *Manager) Schedule(source string) {
	s := m.Settings()
	if !s.Automatic || m.opt.UIDs == nil {
		return
	}
	req := Request{Source: source, Targets: []Target{}}
	for _, uid := range m.opt.UIDs() {
		if m.opt.Eligible != nil && !m.opt.Eligible(uid, false) {
			continue
		}
		for _, r := range Resources {
			snap := m.Snapshot(uid, r)
			if snap.Stale && snap.State != "queued" && snap.State != "running" && snap.State != "backoff" && time.Now().Unix()-snap.LastAttempt >= int64(s.Intervals[r]) {
				req.Targets = append(req.Targets, Target{uid, r})
			}
		}
	}
	// Older/unknown snapshots are serviced first; new activity cannot starve them.
	sort.SliceStable(req.Targets, func(i, j int) bool {
		return m.Snapshot(req.Targets[i].UID, req.Targets[i].Resource).LastSuccess < m.Snapshot(req.Targets[j].UID, req.Targets[j].Resource).LastSuccess
	})
	if len(req.Targets) > 0 {
		if len(req.Targets) > 10000 {
			req.Targets = req.Targets[:10000]
		}
		_, _ = m.Submit(req)
	}
}
