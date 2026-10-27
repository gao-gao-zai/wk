package promptguard

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

var errStop = errors.New("diagnostic stopped")

func (m *Manager) Start(id, kind string, automatic bool) (Job, error) {
	if kind != "locate" && kind != "verify" {
		return Job{}, errors.New("未知诊断操作")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.state.Cases[id]
	if c == nil {
		return Job{}, errors.New("记录不存在")
	}
	if m.closed || m.active && !automatic {
		return Job{}, errors.New("已有诊断正在运行，请稍后重试")
	}
	if c.Job != nil && (c.Job.Status == "running" || c.Job.Status == "queued") {
		return Job{}, errors.New("该记录已在诊断队列中")
	}
	if m.factory == nil {
		return Job{}, errors.New("诊断上游未配置")
	}
	if automatic && !m.state.Config.Settings.AutoLocate {
		return Job{}, errors.New("自动定位已关闭")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	job := Job{Kind: kind, Status: "running", Automatic: automatic, Budget: m.state.Config.Settings.ProbeBudget, Revision: m.state.Config.Revision, Started: time.Now().UTC(), Trace: []Trace{}}
	if m.active {
		job.Status = "queued"
	}
	previous := c.Job
	previousResolved := c.Resolved
	c.Job = &job
	if kind == "verify" {
		c.Resolved = false
	}
	if err := m.saveLocked(); err != nil {
		c.Job = previous
		c.Resolved = previousResolved
		cancel()
		return Job{}, err
	}
	if job.Status == "queued" {
		cancel()
		return copyJSON(job), nil
	}
	m.active = true
	m.cancel[id] = cancel
	item := copyJSON(*c)
	factory := m.factory
	m.wg.Add(1)
	go func() { defer m.wg.Done(); defer cancel(); m.run(ctx, item, factory(item)) }()
	return copyJSON(job), nil
}
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cancel := m.cancel[id]; cancel != nil {
		cancel()
		return nil
	}
	if c := m.state.Cases[id]; c != nil && c.Job != nil && c.Job.Status == "queued" {
		c.Job.Status = "cancelled"
		c.Job.Note = "已取消排队"
		return m.saveLocked()
	}
	return errors.New("该记录没有正在运行的诊断")
}

func (m *Manager) next() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.active || !m.state.Config.Settings.AutoLocate {
		return
	}
	var item *Case
	for _, c := range m.state.Cases {
		if c.Job != nil && c.Job.Status == "queued" && (item == nil || c.Job.Started.Before(item.Job.Started)) {
			item = c
		}
	}
	if item == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	item.Job.Status = "running"
	item.Job.Started = time.Now().UTC()
	item.Job.Revision = m.state.Config.Revision
	item.Job.Budget = m.state.Config.Settings.ProbeBudget
	if m.saveLocked() != nil {
		item.Job.Status = "interrupted"
		cancel()
		return
	}
	m.active = true
	m.cancel[item.ID] = cancel
	copied := copyJSON(*item)
	factory := m.factory
	m.wg.Add(1)
	go func() { defer m.wg.Done(); defer cancel(); m.run(ctx, copied, factory(copied)) }()
}

func (m *Manager) run(ctx context.Context, item Case, probe Probe) {
	status, note := "inconclusive", "诊断未获得确定结果"
	candidate := ""
	minimal := false
	verified := false
	defer func() {
		m.mu.Lock()
		m.active = false
		delete(m.cancel, item.ID)
		if c := m.state.Cases[item.ID]; c != nil {
			if ctx.Err() != nil {
				status = "cancelled"
				note = "诊断已取消或超过 10 分钟时限"
			}
			c.Job.Status = status
			c.Job.Note = note
			now := time.Now().UTC()
			c.Job.Finished = &now
			if candidate != "" {
				c.Candidate = candidate
				c.CandidateMinimal = minimal
			}
			if verified && ctx.Err() == nil && c.Job.Revision == m.state.Config.Revision && !c.LastSeen.After(c.Job.Started) {
				c.Resolved = true
				c.VerifiedRevision = c.Job.Revision
			} else if verified {
				c.Resolved = false
				c.Job.Status = "inconclusive"
				c.Job.Note = "提示词通过，但规则或记录已变化，请重新验证"
			}
			_ = m.saveLocked()
		}
		m.mu.Unlock()
		m.next()
	}()
	calls := 0
	baseline := item.Outbound
	m.mu.Lock()
	prepare := m.prepare
	m.mu.Unlock()
	if item.Job.Kind == "locate" && prepare != nil {
		var e error
		baseline, e = prepare(item)
		if e != nil {
			note = "当前系统提示词转换失败，请检查替换规则"
			return
		}
		m.mu.Lock()
		m.state.Cases[item.ID].Diagnostic = Clone(baseline)
		e = m.saveLocked()
		m.mu.Unlock()
		if e != nil {
			note = "无法保存本次诊断的完整系统提示词"
			return
		}
	}
	test := func(step string, msgs []Message, rewrite bool) (ProbeResult, error) {
		m.mu.Lock()
		stale := m.state.Config.Revision != item.Job.Revision
		m.mu.Unlock()
		if stale {
			return ProbeResult{}, errStop
		}
		if ctx.Err() != nil || calls >= item.Job.Budget {
			return ProbeResult{}, errStop
		}
		if calls > 0 {
			timer := time.NewTimer(300 * time.Millisecond)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ProbeResult{}, errStop
			}
		}
		calls++
		r := safeReason(probe(ctx, Clone(msgs), rewrite))
		m.mu.Lock()
		c := m.state.Cases[item.ID]
		c.Job.Calls = calls
		c.Job.Trace = append(c.Job.Trace, Trace{Step: step, Characters: utf8.RuneCountInString(Text(msgs)), ProbeResult: r})
		e := m.saveLocked()
		m.mu.Unlock()
		if e != nil {
			return r, errStop
		}
		if r.Outcome == "inconclusive" {
			return r, errStop
		}
		return r, nil
	}
	control := []Message{{Role: "system", Content: "You are a helpful assistant."}}
	if r, e := test("control", control, false); e != nil || r.Outcome != "pass" {
		note = "对照请求未通过，可能为账号、网络或模型问题，不能归因于提示词"
		return
	}
	if item.Job.Kind == "verify" {
		if r, e := test("verify_full", item.Original, true); e == nil {
			if r.Outcome == "pass" {
				status = "passed"
				note = "当前替换后的完整系统提示词已通过上游验证"
				verified = true
			} else {
				status = "blocked"
				note = "完整系统提示词仍触发 11128，请继续设置替换规则"
			}
		}
		return
	}
	if r, e := test("reproduce_full", baseline, false); e != nil || r.Outcome != "blocked" {
		note = "当前完整系统提示词未稳定复现 11128，暂不能定位；可验证当前替换"
		return
	}
	chunks, render := splitMessages(baseline)
	if len(chunks) == 0 {
		note = "系统提示词没有可定位的文本"
		return
	}
	indices := make([]int, len(chunks))
	for i := range indices {
		indices[i] = i
	}
	current := indices
	n := 2
	for len(current) > 1 {
		if n > len(current) {
			n = len(current)
		}
		reduced := false
		for part := 0; part < n; part++ {
			lo, hi := part*len(current)/n, (part+1)*len(current)/n
			// Test both halves and complements: interacting sentences may only
			// trigger together, and multiple independent triggers can coexist.
			for variant := 0; variant < 2; variant++ {
				selection := append([]int{}, current[lo:hi]...)
				if variant == 1 {
					selection = append(append([]int{}, current[:lo]...), current[hi:]...)
				}
				if len(selection) == 0 || len(selection) == len(current) {
					continue
				}
				r, e := test("bisect", render(selection), false)
				if e != nil {
					candidate = selectedText(chunks, current)
					note = "请求预算用尽或请求不确定；保留当前可复现片段，尚未证明最小"
					return
				}
				if r.Outcome == "blocked" {
					current = selection
					n = 2
					reduced = true
					break
				}
			}
			if reduced {
				break
			}
		}
		if reduced {
			continue
		}
		if n == len(current) {
			break
		}
		n *= 2
	}
	candidate = selectedText(chunks, current)
	minimal = true
	status = "located"
	note = "已定位可复现的句子或句子组合；请设置替换后验证完整提示词"
}

type chunk struct {
	text string
	slot int
}

// Split at sentence/newline boundaries while retaining every byte, content part
// and message position. Punctuation-only pieces stay with their preceding text.
func splitMessages(messages []Message) ([]chunk, func([]int) []Message) {
	var chunks []chunk
	slot := 0
	mapTexts(Clone(messages), func(s string) string {
		start := 0
		for i, r := range s {
			if r == '\n' || r == '.' || r == '!' || r == '?' || r == '。' || r == '！' || r == '？' || r == ';' || r == '；' {
				end := i + utf8.RuneLen(r)
				if strings.TrimSpace(s[start:end]) != "" {
					chunks = append(chunks, chunk{text: s[start:end], slot: slot})
					start = end
				}
			}
		}
		if start < len(s) {
			if len(chunks) > 0 && chunks[len(chunks)-1].slot == slot && strings.TrimSpace(s[start:]) == "" {
				chunks[len(chunks)-1].text += s[start:]
			} else {
				chunks = append(chunks, chunk{text: s[start:], slot: slot})
			}
		}
		slot++
		return s
	})
	render := func(ids []int) []Message {
		parts := map[int]string{}
		for _, i := range ids {
			parts[chunks[i].slot] += chunks[i].text
		}
		out := Clone(messages)
		idx := 0
		mapTexts(out, func(string) string { s := parts[idx]; idx++; return s })
		return out
	}
	return chunks, render
}
func selectedText(chunks []chunk, ids []int) string {
	var b strings.Builder
	previous := -1
	for _, i := range ids {
		if previous >= 0 && previous+1 != i {
			b.WriteString("\n[…]\n")
		}
		b.WriteString(chunks[i].text)
		previous = i
	}
	return b.String()
}
