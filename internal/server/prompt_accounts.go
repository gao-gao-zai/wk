package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"

	"workbuddy2api/internal/promptguard"
	"workbuddy2api/internal/upstream"
)

func capturedPromptScope(scope requestScope, allow map[string]bool) promptguard.AccountScope {
	if scope.kind == "admin" {
		return promptguard.AccountScope{Kind: "all"}
	}
	result := promptguard.AccountScope{Kind: "group", Group: scope.group, Accounts: []string{}}
	for uid, ok := range allow {
		if ok {
			result.Accounts = append(result.Accounts, uid)
		}
	}
	sort.Strings(result.Accounts)
	return result
}

func (h *Handler) manualPromptScope(kind, group string) (promptguard.AccountScope, error) {
	if kind == "all" && group == "" {
		return promptguard.AccountScope{Kind: "all"}, nil
	}
	if kind != "group" || group == "" || h.cfg.Groups == nil {
		return promptguard.AccountScope{}, errors.New("请选择有效的诊断账号分组")
	}
	exists := false
	for _, g := range h.cfg.Groups.ListDetailed() {
		if g.Name == group {
			exists = true
			break
		}
	}
	if !exists {
		return promptguard.AccountScope{}, errors.New("所选账号分组不存在")
	}
	return capturedPromptScope(requestScope{kind: "group", group: group}, h.cfg.Groups.AccountsInGroup(group)), nil
}

func (h *Handler) promptAccountAllow(item promptguard.Case) (map[string]bool, string, string) {
	scope := item.AccountScope
	if item.Job != nil && item.Job.AccountScope != nil {
		scope = item.Job.AccountScope
	}
	if scope == nil {
		return map[string]bool{item.UID: true}, "legacy", ""
	}
	if scope.Kind == "all" {
		return nil, "all", ""
	}
	allow := map[string]bool{}
	if scope.Kind != "group" || scope.Group == "" || h.cfg.Groups == nil {
		return allow, "group", scope.Group
	}
	current := h.cfg.Groups.AccountsInGroup(scope.Group)
	for _, uid := range scope.Accounts {
		if current[uid] {
			allow[uid] = true
		}
	}
	return allow, "group", scope.Group
}

// Select once, reserve one account slot for the complete job, and release it
// before the manager starts the next queued job. There is no mid-job rotation.
func (h *Handler) promptSession(parent context.Context, item promptguard.Case) promptguard.ProbeSession {
	started := time.Now()
	allow, kind, group := h.promptAccountAllow(item)
	selection := promptguard.AccountSelection{ScopeKind: kind, Group: group, Reason: "no_available_account"}
	failed := func(reason string) promptguard.ProbeSession {
		selection.Reason = reason
		selection.WaitMS = time.Since(started).Milliseconds()
		return promptguard.ProbeSession{Selection: selection, Probe: func(context.Context, []promptguard.Message, bool) promptguard.ProbeResult {
			attempted := false
			return promptguard.ProbeResult{Outcome: "inconclusive", Reason: reason, RequestAttempted: &attempted}
		}}
	}
	if h.cfg.Pool == nil || h.cfg.Upstream == nil {
		return failed("upstream_unavailable")
	}
	waitCtx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	for {
		allow, _, _ = h.promptAccountAllow(item)
		selection.ScopeCount = 0
		for _, uid := range h.cfg.Pool.UIDs() {
			if allow == nil || allow[uid] {
				selection.ScopeCount++
			}
		}
		ready := map[string]bool{}
		for _, uid := range h.cfg.Pool.AvailableUIDs() {
			if allow != nil && !allow[uid] {
				continue
			}
			a := h.cfg.Pool.AuthByUID(uid)
			if a != nil && !a.NeedsRefresh(h.cfg.RefreshSkew) && h.cfg.Pool.HealthyForModel(uid, item.Model) {
				ready[uid] = true
			}
		}
		selection.AvailableCount = len(ready)
		if waitCtx.Err() != nil {
			return failed("no_available_account")
		}
		acct := h.cfg.Pool.PickAndAcquireForModelAllow(item.Model, nil, ready)
		if acct != nil {
			current, _, _ := h.promptAccountAllow(item)
			if (current != nil && !current[acct.UID]) || !h.cfg.Pool.HealthyForModel(acct.UID, item.Model) || acct.NeedsRefresh(h.cfg.RefreshSkew) {
				h.cfg.Pool.Release(acct.UID)
			} else {
				selection.UID = acct.UID
				selection.Nickname = acct.Snapshot().Nickname
				selection.Reason = "weighted_available"
				selection.WaitMS = time.Since(started).Milliseconds()
				if kind == "legacy" {
					selection.Reason = "legacy_original_only"
				}
				var once sync.Once
				close := func() { once.Do(func() { h.cfg.Pool.Release(acct.UID) }) }
				probe := func(ctx context.Context, messages []promptguard.Message, rewrite bool) (result promptguard.ProbeResult) {
					attempted := false
					defer func() { result.RequestAttempted = &attempted }()
					unknown := func(status int, reason string) promptguard.ProbeResult {
						return promptguard.ProbeResult{Outcome: "inconclusive", HTTP: status, Reason: reason}
					}
					current, _, _ := h.promptAccountAllow(item)
					if current != nil && !current[acct.UID] {
						return unknown(0, "account_scope_changed")
					}
					if ctx.Err() != nil || !h.cfg.Pool.HealthyForModel(acct.UID, item.Model) {
						return unknown(0, "selected_account_unavailable")
					}
					a := h.cfg.Pool.AuthByUID(acct.UID)
					if a == nil {
						return unknown(0, "selected_account_unavailable")
					}
					if a.NeedsRefresh(h.cfg.RefreshSkew) {
						return unknown(0, "account_needs_refresh")
					}
					requestCtx, cancel := context.WithTimeout(upstream.DiagnosticContext(ctx), 45*time.Second)
					defer cancel()
					doc := probeDocument(item.Model, messages)
					var body []byte
					var err error
					if rewrite {
						body, err = h.cfg.Upstream.PrepareChatBody(doc)
					} else {
						body, err = json.Marshal(doc)
					}
					if err != nil {
						return unknown(0, "invalid_prompt")
					}
					attempted = true
					rc, status, raw, _, err := h.cfg.Upstream.ChatStreamPreparedWithPolicy(requestCtx, a, body, upstream.StreamPolicy{Total: 45 * time.Second, Idle: 20 * time.Second})
					if err != nil {
						if errors.Is(err, context.DeadlineExceeded) {
							return unknown(status, "request_timeout")
						}
						return unknown(status, "transport_error")
					}
					if rc != nil {
						defer rc.Close()
					}
					if promptguard.IsBlocked(status, raw) {
						return promptguard.ProbeResult{Outcome: "blocked", HTTP: status, Reason: "11128"}
					}
					if status != http.StatusOK || rc == nil {
						if status == 429 {
							return unknown(status, "rate_limit")
						}
						if status == 401 || status == 403 {
							return unknown(status, "upstream_auth_error")
						}
						return unknown(status, "upstream_error")
					}
					return classifyPromptStream(rc)
				}
				return promptguard.ProbeSession{Probe: probe, Close: close, Selection: selection}
			}
		}
		select {
		case <-waitCtx.Done():
			return failed("no_available_account")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
