package promptguard

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"workbuddy2api/internal/lifecycle"
)

var (
	errBudget      = errors.New("diagnostic budget exhausted")
	errChanged     = errors.New("diagnostic configuration changed")
	errPersistence = errors.New("diagnostic persistence failed")
	errProbe       = errors.New("diagnostic probe inconclusive")
)

func (m *Manager) Start(id, kind string, automatic bool) (Job, error) {
	return m.start(id, kind, automatic, nil)
}

func (m *Manager) start(id, kind string, automatic bool, override *AccountScope) (Job, error) {
	done, admitted := lifecycle.Default.Enter()
	if !admitted {
		return Job{}, errors.New("服务正在排空")
	}
	defer done()
	if kind != "locate" && kind != "verify" {
		return Job{}, errors.New("未知诊断操作")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.state.Cases[id]
	if c == nil {
		return Job{}, errors.New("记录不存在")
	}
	if override != nil && c.AccountScope != nil {
		return Job{}, errors.New("已有原请求范围的记录不能扩大诊断选号范围")
	}
	if m.closed || m.active && !automatic {
		return Job{}, errors.New("已有诊断正在运行，请稍后重试")
	}
	if c.Job != nil && (c.Job.Status == "running" || c.Job.Status == "queued") {
		return Job{}, errors.New("该记录已在诊断队列中")
	}
	if m.factory == nil && m.sessionFactory == nil {
		return Job{}, errors.New("诊断上游未配置")
	}
	if automatic && !m.state.Config.Settings.AutoLocate {
		return Job{}, errors.New("自动定位已关闭")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	job := Job{Kind: kind, Status: "running", Automatic: automatic, Budget: m.state.Config.Settings.ProbeBudget, Revision: m.state.Config.Revision, Started: time.Now().UTC(), Trace: []Trace{}}
	job.AccountScope = copyJSON(c.AccountScope)
	if m.sessionFactory != nil {
		job.CurrentStep = "select_account"
	}
	if override != nil {
		job.AccountScope = copyJSON(override)
	}
	if kind == "verify" {
		job.Budget = 2
	}
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
	sessions := m.sessionFactory
	m.wg.Add(1)
	lifecycle.Go(func() { defer m.wg.Done(); defer cancel(); m.launch(ctx, item, factory, sessions) })
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
	if m.sessionFactory != nil {
		item.Job.CurrentStep = "select_account"
	}
	item.Job.Started = time.Now().UTC()
	item.Job.Revision = m.state.Config.Revision
	item.Job.Budget = m.state.Config.Settings.ProbeBudget
	item.Job.AccountScope = copyJSON(item.AccountScope)
	if m.saveLocked() != nil {
		item.Job.Status = "interrupted"
		cancel()
		return
	}
	m.active = true
	m.cancel[item.ID] = cancel
	copied := copyJSON(*item)
	factory := m.factory
	sessions := m.sessionFactory
	m.wg.Add(1)
	lifecycle.Go(func() { defer m.wg.Done(); defer cancel(); m.launch(ctx, copied, factory, sessions) })
}

func (m *Manager) run(ctx context.Context, item Case, probe Probe, release func()) {
	status, note := "inconclusive", "诊断未获得确定结果"
	endReason := "probe_inconclusive"
	candidate := ""
	minimal := false
	verified := false
	defer func() {
		if release != nil {
			release()
		}
		m.mu.Lock()
		m.active = false
		delete(m.cancel, item.ID)
		if c := m.state.Cases[item.ID]; c != nil {
			if ctx.Err() != nil {
				status = "cancelled"
				endReason, note = "cancelled", "诊断已取消，后续检查未执行"
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					endReason, note = "timeout", "诊断超过 10 分钟时限，后续检查未执行"
				}
			}
			c.Job.Status = status
			c.Job.Note = note
			c.Job.EndReason = endReason
			c.Job.CurrentStep = ""
			now := time.Now().UTC()
			c.Job.Finished = &now
			if candidate != "" {
				c.Candidate = candidate
				c.CandidateMinimal = minimal
				c.CandidateRevision = item.Job.Revision
				c.CandidateLocatedAt = &now
			}
			if verified && ctx.Err() == nil && c.Job.Revision == m.state.Config.Revision && !c.LastSeen.After(c.Job.Started) {
				c.Resolved = true
				c.VerifiedRevision = c.Job.Revision
			} else if verified {
				c.Resolved = false
				c.Job.Status = "inconclusive"
				c.Job.EndReason = "config_or_case_changed"
				c.Job.Note = "提示词通过，但规则或记录已变化，请重新验证"
			}
			_ = m.saveLocked()
		}
		m.mu.Unlock()
		m.next()
	}()
	calls := 0
	type cachedResult struct {
		result     ProbeResult
		traceIndex int
	}
	cache := map[[32]byte]cachedResult{}
	stopNote := func(err error, result ProbeResult) {
		switch {
		case errors.Is(err, errBudget):
			endReason, note = "budget_exhausted", "请求预算已用尽；尚未完成的检查不会继续发送"
		case errors.Is(err, errChanged):
			endReason, note = "config_changed", "规则或诊断配置已变化，本次检查已停止，请重新启动"
		case errors.Is(err, errPersistence):
			endReason, note = "persistence_failed", "诊断结果无法保存，已停止后续检查，请检查存储状态"
		default:
			endReason, note = "probe_inconclusive", "检查未完成："+probeReason(result)
		}
	}
	baseline := item.Outbound
	m.mu.Lock()
	prepare := m.prepare
	m.mu.Unlock()
	if item.Job.Kind == "locate" && prepare != nil {
		var e error
		baseline, e = prepare(item)
		if e != nil {
			endReason = "preparation_failed"
			note = "当前系统提示词转换失败，请检查替换规则"
			return
		}
		m.mu.Lock()
		m.state.Cases[item.ID].Diagnostic = Clone(baseline)
		e = m.saveLocked()
		m.mu.Unlock()
		if e != nil {
			endReason = "persistence_failed"
			note = "无法保存本次诊断的完整系统提示词"
			return
		}
	}
	test := func(step string, msgs []Message, rewrite bool) (ProbeResult, error) {
		m.mu.Lock()
		stale := m.state.Config.Revision != item.Job.Revision
		m.mu.Unlock()
		if stale {
			return ProbeResult{}, errChanged
		}
		if ctx.Err() != nil {
			return ProbeResult{}, ctx.Err()
		}
		encoded, _ := json.Marshal([]any{msgs, rewrite})
		key := sha256.Sum256(encoded)
		if hit, ok := cache[key]; ok {
			r := hit.result
			attempted := false
			r.RequestAttempted = &attempted
			m.mu.Lock()
			c := m.state.Cases[item.ID]
			c.Job.CacheHits++
			c.Job.Trace = append(c.Job.Trace, Trace{Step: step, Characters: utf8.RuneCountInString(Text(msgs)), CachedFrom: hit.traceIndex, ProbeResult: r})
			err := m.saveLocked()
			m.mu.Unlock()
			if err != nil {
				return r, errPersistence
			}
			return r, nil
		}
		if calls >= item.Job.Budget {
			return ProbeResult{}, errBudget
		}
		if calls > 0 {
			timer := time.NewTimer(300 * time.Millisecond)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ProbeResult{}, ctx.Err()
			}
		}
		calls++
		m.mu.Lock()
		m.state.Cases[item.ID].Job.CurrentStep = step
		e := m.saveLocked()
		m.mu.Unlock()
		if e != nil {
			return ProbeResult{}, errPersistence
		}
		started := time.Now()
		r := safeReason(probe(ctx, Clone(msgs), rewrite))
		m.mu.Lock()
		c := m.state.Cases[item.ID]
		c.Job.Calls = calls
		c.Job.CurrentStep = ""
		c.Job.Trace = append(c.Job.Trace, Trace{Step: step, Characters: utf8.RuneCountInString(Text(msgs)), DurationMS: time.Since(started).Milliseconds(), ProbeResult: r})
		if r.Outcome != "inconclusive" {
			cache[key] = cachedResult{result: r, traceIndex: len(c.Job.Trace)}
		}
		e = m.saveLocked()
		m.mu.Unlock()
		if e != nil {
			return r, errPersistence
		}
		if r.Outcome == "inconclusive" {
			return r, errProbe
		}
		return r, nil
	}
	control := []Message{{Role: "system", Content: "You are a helpful assistant."}}
	if r, e := test("control", control, false); e != nil || r.Outcome != "pass" {
		stopNote(e, r)
		if e == nil || errors.Is(e, errProbe) {
			endReason = "control_failed"
			note = "对照检查未完成：" + probeReason(r) + "。后续完整提示词检查未执行，不能判断替换是否有效"
		}
		return
	}
	if item.Job.Kind == "verify" {
		if r, e := test("verify_full", item.Original, true); e != nil {
			stopNote(e, r)
		} else {
			if r.Outcome == "pass" {
				status = "passed"
				note = "当前替换后的完整系统提示词已通过上游验证"
				verified = true
				endReason = "verified"
			} else {
				status = "blocked"
				note = "完整系统提示词仍触发 11128，请继续设置替换规则"
				endReason = "still_blocked"
			}
		}
		return
	}
	if r, e := test("reproduce_full", baseline, false); e != nil || r.Outcome != "blocked" {
		if e != nil {
			stopNote(e, r)
		} else {
			endReason = "not_reproduced"
			note = "当前完整系统提示词未复现 11128，本次未进行分段定位；可验证当前替换"
		}
		return
	}
	chunks, render := splitMessages(baseline)
	if len(chunks) == 0 {
		endReason = "no_text"
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
					stopNote(e, r)
					note += "；保留本次已复现片段，尚未完成最小化检查"
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
	endReason = "located"
	note = "已定位可复现的句子或句子组合；请设置替换后验证完整提示词"
}

func probeReason(r ProbeResult) string {
	switch r.Reason {
	case "no_available_account":
		return "原请求允许范围内没有可用账号，未发送上游请求"
	case "selected_account_unavailable":
		return "本次选定账号已不可用，未中途换号"
	case "account_scope_changed":
		return "本次选定账号已不在原请求允许范围，已停止诊断"
	case "selection_persistence_failed":
		return "无法保存本次选号信息，未发送上游请求"
	case "account_busy_or_unavailable":
		return "原账号忙碌或不可用，等待 3 秒仍未取得执行槽位，未发送上游请求"
	case "account_needs_refresh":
		return "原账号凭据需要刷新，未发送上游请求"
	case "upstream_unavailable":
		return "诊断上游未配置，未发送上游请求"
	case "invalid_prompt":
		return "系统提示词无法转换，未发送上游请求"
	case "transport_error":
		return "连接上游失败，未获得有效 HTTP 响应"
	case "request_timeout":
		return "上游请求超时，未获得完整结果"
	case "rate_limit":
		return "上游限流，请稍后重试"
	case "upstream_auth_error":
		return "上游认证或权限检查失败"
	case "incomplete_or_invalid_stream":
		return "上游响应流不完整或包含错误，不能作为通过依据"
	case "11128":
		return "中性对照也触发 11128，无法将问题归因于原始提示词"
	default:
		return "未获得确定的上游检查结果，请查看步骤详情"
	}
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
