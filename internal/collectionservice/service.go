// Package collectionservice adapts account-scoped upstream APIs to the shared
// collection executor. No collector signs in, claims rewards, or creates chats.
package collectionservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/collection"
	"workbuddy2api/internal/metricsstore"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

type Service struct {
	Manager  *collection.Manager
	Pool     *pool.Pool
	Upstream *upstream.Client
	Metrics  metricsstore.Backend
	// Shared with account import/delete: stale HTTP responses cannot recreate
	// deleted credential files or overwrite replacement accounts.
	AccountLock *sync.Mutex
}

func New(store collection.Store, p *pool.Pool, up *upstream.Client, metrics metricsstore.Backend, lock *sync.Mutex) (*Service, error) {
	if lock == nil {
		lock = &sync.Mutex{}
	}
	s := &Service{Pool: p, Upstream: up, Metrics: metrics, AccountLock: lock}
	m, err := collection.New(collection.Options{Store: store, Run: s.run, UIDs: func() []string {
		out := []string{}
		for _, st := range p.List() {
			out = append(out, st.UID)
		}
		return out
	}, Eligible: func(uid string, allow bool) bool {
		a := p.AuthByUID(uid)
		return a != nil && (allow || !p.IsDisabled(uid))
	}})
	if err != nil {
		return nil, err
	}
	s.Manager = m
	return s, nil
}
func frozen(c auth.Credentials) *auth.Auth {
	return &auth.Auth{UID: c.UID, AccessToken: c.AccessToken, RefreshToken: c.RefreshToken, ExpiresAt: c.ExpiresAt, Domain: c.Domain, EnterpriseID: c.EnterpriseID, Nickname: c.Nickname, FilePath: c.FilePath}
}
func same(a, b auth.Credentials) bool {
	return a.AccessToken == b.AccessToken && a.RefreshToken == b.RefreshToken && a.Domain == b.Domain
}
func obsolete() error {
	return &collection.Failure{Kind: "superseded", Message: "账号已删除或凭证已更新，丢弃旧响应", Skip: true}
}
func (s *Service) current(a *auth.Auth, c auth.Credentials) bool {
	return s.Pool.AuthByUID(c.UID) == a && same(a.Snapshot(), c)
}
func marshal(v any) (json.RawMessage, error) { b, e := json.Marshal(v); return b, e }
func (s *Service) read(ctx context.Context, fn func() error) error {
	if err := s.Manager.WaitRequest(ctx); err != nil {
		return err
	}
	return fn()
}
func (s *Service) refresh(ctx context.Context, a *auth.Auth, rejectedToken string) error {
	before := a.Snapshot()
	a.Lock()
	defer a.Unlock()
	// A concurrent urgent recovery may already have replaced this token bundle.
	if !same(before, a.Snapshot()) {
		return nil
	}
	if rejectedToken != "" && before.AccessToken != rejectedToken {
		return nil
	}
	if rejectedToken == "" && !a.NeedsRefresh(10*time.Minute) {
		return nil
	}
	if before.RefreshToken == "" {
		return &collection.Failure{Kind: "credentials", Message: "缺少 refresh token"}
	}
	copy := frozen(before)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s.read(ctx, func() error { return s.Upstream.RefreshTokenContext(ctx, copy) }); err != nil {
		return classify(err)
	}
	s.AccountLock.Lock()
	defer s.AccountLock.Unlock()
	if !s.current(a, before) {
		return obsolete()
	}
	after := copy.Snapshot()
	a.ApplyRefresh(after.AccessToken, after.RefreshToken, after.Domain, after.ExpiresAt)
	if a.FilePath != "" {
		if err := a.SaveAtomic(); err != nil {
			return &collection.Failure{Kind: "persistence", Message: "凭证已更新，但授权文件保存失败"}
		}
	}
	return nil
}
func (s *Service) run(ctx context.Context, t collection.Target, previous collection.Snapshot) (json.RawMessage, error) {
	a := s.Pool.AuthByUID(t.UID)
	if a == nil {
		return nil, obsolete()
	}
	initial := a.Snapshot()
	data, err := s.runOnce(ctx, t, previous)
	var f *collection.Failure
	if errors.As(err, &f) && f.Kind == "unauthorized" && !s.Pool.IsDisabled(t.UID) {
		if recovery := s.refresh(ctx, a, initial.AccessToken); recovery != nil {
			return nil, recovery
		}
		return s.runOnce(ctx, t, previous)
	}
	return data, err
}
func (s *Service) runOnce(ctx context.Context, t collection.Target, previous collection.Snapshot) (json.RawMessage, error) {
	a := s.Pool.AuthByUID(t.UID)
	if a == nil {
		return nil, obsolete()
	}
	if t.Resource == "credential" {
		if !s.Pool.IsDisabled(t.UID) && a.NeedsRefresh(10*time.Minute) {
			if err := s.refresh(ctx, a, ""); err != nil {
				return nil, err
			}
		}
		c := a.Snapshot()
		if s.Pool.AuthByUID(t.UID) != a {
			return nil, obsolete()
		}
		return marshal(map[string]any{"expires_at": c.ExpiresAt, "has_access_token": c.AccessToken != "", "has_refresh_token": c.RefreshToken != "", "expired": c.ExpiresAt > 0 && c.ExpiresAt <= time.Now().Unix(), "inspection_only": s.Pool.IsDisabled(t.UID)})
	}
	if t.Resource == "tasks" && a.Region() == auth.RegionGlobal {
		return nil, &collection.Failure{Kind: "unsupported", Message: "海外版无此成长任务体系", Skip: true}
	}
	if a.NeedsRefresh(10*time.Minute) && !s.Pool.IsDisabled(t.UID) {
		if err := s.refresh(ctx, a, ""); err != nil {
			return nil, err
		}
	}
	initial := a.Snapshot()
	if initial.AccessToken == "" {
		return nil, &collection.Failure{Kind: "credentials", Message: "缺少 access token"}
	}
	copy := frozen(initial)
	var value any
	var err error
	switch t.Resource {
	case "balance":
		var v upstream.ResourceUsage
		err = s.read(ctx, func() error { var e error; v, e = s.Upstream.UserResourceDetailsContext(ctx, copy); return e })
		value = v
		if err == nil {
			a.Lock()
			s.AccountLock.Lock()
			if !s.current(a, initial) {
				err = obsolete()
			} else {
				s.Pool.SetCreditDetail(t.UID, pool.CreditDetail{Remaining: v.Remaining, CapacitySize: v.CapacitySize, CapacityRemain: v.CapacityRemain, CapacityUsed: v.CapacityUsed, CycleCapacitySize: v.CycleCapacitySize, CycleCapacityRemain: v.CycleCapacityRemain, CycleCapacityUsed: v.CycleCapacityUsed})
			}
			s.AccountLock.Unlock()
			a.Unlock()
		}
	case "models":
		err = s.read(ctx, func() error {
			var v []upstream.ModelInfo
			var e error
			v, e = s.Upstream.FetchModelsContext(ctx, copy)
			value = v
			return e
		})
	case "tasks":
		err = s.read(ctx, func() error {
			var v []upstream.Task
			var e error
			v, e = s.Upstream.ListTasksContext(ctx, copy)
			value = v
			return e
		})
	case "usage":
		value, err = s.usage(ctx, a, copy, initial, previous)
	default:
		return nil, &collection.Failure{Kind: "unsupported", Message: "不支持的资源"}
	}
	if err != nil {
		return nil, classify(err)
	}
	if !s.current(a, initial) {
		return nil, obsolete()
	}
	return marshal(value)
}

type usageResult struct {
	Watermark int64 `json:"watermark"`
	From      int64 `json:"from"`
	Pages     int   `json:"pages"`
	Records   int   `json:"records"`
}

func (s *Service) usage(ctx context.Context, a, copy *auth.Auth, c auth.Credentials, prev collection.Snapshot) (usageResult, error) {
	if s.Metrics == nil {
		return usageResult{}, &collection.Failure{Kind: "storage", Message: "请求账单存储不可用"}
	}
	var old usageResult
	_ = json.Unmarshal(prev.Data, &old)
	end := time.Now()
	start := end.Add(-48 * time.Hour)
	// Overlap absorbs delayed billing and clock skew; watermark advances only
	// after every page was read and reconciled successfully.
	if old.Watermark > 0 {
		candidate := time.Unix(old.Watermark, 0).Add(-2 * time.Hour)
		if candidate.After(start) {
			start = candidate
		}
	}
	out := usageResult{Watermark: end.Unix(), From: start.Unix()}
	seen := map[string]bool{}
	for page := 1; page <= 500; page++ {
		var rows []upstream.RequestUsage
		var total int
		err := s.read(ctx, func() error {
			var e error
			rows, total, e = s.Upstream.UserRequestUsageContext(ctx, copy, start, end, page, 200)
			return e
		})
		if err != nil {
			return out, err
		}
		out.Pages = page
		if !s.current(a, c) {
			return out, obsolete()
		}
		newRows := 0
		for _, r := range rows {
			if r.RequestID == "" || seen[r.RequestID] {
				continue
			}
			seen[r.RequestID] = true
			newRows++
			if err = s.Metrics.ReconcileRequestCredit(r.RequestID, r.Credit); err != nil {
				return out, &collection.Failure{Kind: "storage", Message: "账单写入失败，未推进水位"}
			}
			out.Records++
		}
		if page*200 >= total && len(rows) < 200 || total > 0 && page*200 >= total {
			return out, nil
		}
		if len(rows) == 0 || newRows == 0 {
			return out, &collection.Failure{Kind: "pagination", Message: "上游分页未前进，未推进水位"}
		}
	}
	return out, &collection.Failure{Kind: "pagination", Message: "分页超过安全上限 500 页，未推进水位"}
}
func classify(err error) error {
	var f *collection.Failure
	if errors.As(err, &f) {
		return f
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &collection.Failure{Kind: "timeout", Message: "上游查询超时", Retryable: !errors.Is(err, context.Canceled)}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return &collection.Failure{Kind: "network", Message: "上游连接提前关闭", Retryable: true}
	}
	var ue *upstream.Error
	if errors.As(err, &ue) {
		kind := ue.Kind.String()
		msg := fmt.Sprintf("上游 %s（HTTP %d）", kind, ue.Status)
		retry := ue.Kind == upstream.ErrSoftRate || ue.Kind == upstream.ErrServer || ue.Kind == upstream.ErrNotFound
		switch ue.Kind {
		case upstream.ErrUnauthorized:
			msg = "上游鉴权失败，请采集凭证或重新授权"
		case upstream.ErrSessionDead, upstream.ErrAccountDisabled:
			msg = "上游明确拒绝账号或登录会话，请重新授权/检查账号"
		}
		return &collection.Failure{Kind: kind, Message: msg, Retryable: retry, After: ue.RetryAfter}
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return &collection.Failure{Kind: "network", Message: "上游网络连接失败", Retryable: true}
	}
	if errors.Is(err, upstream.ErrNoProxyNode) {
		return &collection.Failure{Kind: "proxy", Message: "没有可用请求代理", Retryable: true}
	}
	// Raw upstream bodies may contain sensitive fields. Persist only categories.
	return &collection.Failure{Kind: "invalid_response", Message: "上游响应无效或读取失败"}
}
