package promptguard

import (
	"context"
	"errors"
	"slices"
)

// AccountScope contains authorization metadata only, never an API credential.
// A group snapshot can only shrink when checked against current membership.
type AccountScope struct {
	Kind     string   `json:"kind"` // all, group; nil on Case means legacy/unknown
	Group    string   `json:"group,omitempty"`
	Accounts []string `json:"accounts,omitempty"`
}

type AccountSelection struct {
	UID            string `json:"uid,omitempty"`
	Nickname       string `json:"nickname,omitempty"`
	Reason         string `json:"reason"`
	ScopeKind      string `json:"scope_kind"` // all, group, legacy
	Group          string `json:"group,omitempty"`
	ScopeCount     int    `json:"scope_count"`
	AvailableCount int    `json:"available_count"`
	WaitMS         int64  `json:"wait_ms"`
}

type ProbeSession struct {
	Probe     Probe
	Selection AccountSelection
	Close     func()
}
type SessionFactory func(context.Context, Case) ProbeSession

func normalizedScope(scope AccountScope) (AccountScope, error) {
	if scope.Kind != "all" && (scope.Kind != "group" || scope.Group == "") {
		return AccountScope{}, errors.New("无效的诊断选号范围")
	}
	if scope.Kind == "all" {
		return AccountScope{Kind: "all"}, nil
	}
	scope.Accounts = append([]string{}, scope.Accounts...)
	slices.Sort(scope.Accounts)
	scope.Accounts = slices.Compact(scope.Accounts)
	scope.Accounts = slices.DeleteFunc(scope.Accounts, func(uid string) bool { return uid == "" })
	return scope, nil
}

func (m *Manager) SetSessionFactory(factory SessionFactory) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionFactory = factory
	m.factory = nil
}

// A legacy record has no trustworthy original authorization scope. Only an
// explicit administrator choice may widen its default single-account scope.
func (m *Manager) StartWithAccountScope(id, kind string, scope AccountScope) (Job, error) {
	normalized, err := normalizedScope(scope)
	if err != nil {
		return Job{}, err
	}
	return m.start(id, kind, false, &normalized)
}

func (m *Manager) launch(ctx context.Context, item Case, factory ProbeFactory, sessions SessionFactory) {
	if sessions == nil {
		m.run(ctx, item, factory(item), nil)
		return
	}
	session := sessions(ctx, item)
	if session.Close == nil {
		session.Close = func() {}
	}
	m.mu.Lock()
	c := m.state.Cases[item.ID]
	c.Job.Selection = copyJSON(&session.Selection)
	err := m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		session.Close()
		session.Close = func() {}
		session.Probe = func(context.Context, []Message, bool) ProbeResult {
			attempted := false
			return ProbeResult{Outcome: "inconclusive", Reason: "selection_persistence_failed", RequestAttempted: &attempted}
		}
	}
	if session.Probe == nil {
		session.Probe = func(context.Context, []Message, bool) ProbeResult {
			attempted := false
			return ProbeResult{Outcome: "inconclusive", Reason: "upstream_unavailable", RequestAttempted: &attempted}
		}
	}
	m.run(ctx, item, session.Probe, session.Close)
}
