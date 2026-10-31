package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"workbuddy2api/internal/haozhuma"
)

func profileSettings(s haozhuma.Settings, provider string) haozhuma.Settings {
	s = s.Normalized()
	s.Provider, s.Mode, s.Fallback = provider, provider, ""
	return s.Normalized()
}

func platformReady(s haozhuma.Settings, provider string) bool {
	s = profileSettings(s, provider)
	if provider == "ecode" {
		return s.Token != "" && s.EcodeKeyword != ""
	}
	return s.Sid != "" && (s.Token != "" || (s.User != "" && s.Pass != ""))
}

func (h *Handler) buildTaskSMS(ctx context.Context, mode string) (*haozhuma.Client, error) {
	s := h.currentSMSSettings().Normalized()
	if mode == "" {
		mode = "auto"
	}
	hao, ec := platformReady(s, "haozhuma"), platformReady(s, "ecode")
	provider := mode
	if mode == "auto" || mode == "both" {
		if hao {
			provider = "haozhuma"
		} else {
			provider = "ecode"
		}
		if mode == "both" && (!hao || !ec) {
			return nil, errors.New("请先分别配置豪猪和 E码云，再选择双平台轮换")
		}
	}
	s = profileSettings(s, provider)
	s.Mode = mode
	if provider == "haozhuma" && ec && (mode == "auto" || mode == "both") {
		s.Fallback = s.Credentials.Ecode.Token
	}
	var previous *haozhuma.Client
	if h.cfg.AutoEnroll != nil {
		previous = h.cfg.AutoEnroll.CurrentSMSClient()
	}
	c, err := haozhuma.Build(ctx, s, haozhumaBase, previous, false)
	// 豪猪登录故障不能阻断自动模式下已经独立配置的 E码云。
	if err != nil && mode == "auto" && ec && provider == "haozhuma" {
		s = profileSettings(s, "ecode")
		s.Mode = "auto"
		return haozhuma.Build(ctx, s, haozhumaBase, previous, false)
	}
	return c, err
}

func (h *Handler) smsProfileProbe(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if provider != "haozhuma" && provider != "ecode" {
		writeJSON(w, 400, map[string]string{"error": "未知接码平台"})
		return
	}
	s := profileSettings(h.currentSMSSettings(), provider)
	if r.Method == "POST" {
		var p struct {
			User    *string `json:"user"`
			Pass    *string `json:"pass"`
			Token   *string `json:"token"`
			Base    *string `json:"ecode_base"`
			Keyword *string `json:"ecode_keyword"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&p); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求格式不正确"})
			return
		}
		if p.User != nil {
			s.User = strings.TrimSpace(*p.User)
		}
		if p.Pass != nil {
			s.Pass = *p.Pass
		}
		if p.Token != nil {
			s.Token = strings.TrimSpace(*p.Token)
			s.User, s.Pass = "", ""
		}
		if p.Base != nil {
			s.EcodeBase = strings.TrimSpace(*p.Base)
		}
		if p.Keyword != nil {
			s.EcodeKeyword = strings.TrimSpace(*p.Keyword)
		}
		// 草稿覆盖的是明确的平台，空字段省略时在服务端复用已保存的值。
		s.Credentials = nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	c, err := haozhuma.Build(ctx, s, haozhumaBase, nil, false)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	summary, err := c.Summary(ctx)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "provider": provider, "balance": summary.Balance, "occupied": summary.Occupied, "checked_at": time.Now().Format(time.RFC3339)})
}

func fetchPatchPresent(p *smsPatch) bool {
	if p == nil {
		return false
	}
	s := p.Haozhuma
	return s.Sid != nil || s.Author != nil || s.UID != nil || s.UIDs != nil || s.ISP != nil
}

// Keep errors.Is(ErrBusyAuth) compatibility while reporting the actual blocker.
type smsBusyReason struct{ reason string }

func (e *smsBusyReason) Error() string { return e.reason }
func (e *smsBusyReason) Unwrap() error { return ErrBusyAuth }

// smsBusyLocked requires a.mu. A failed release is not a running task.
func (a *AutoEnroller) smsBusyLocked() error {
	switch {
	case a.running:
		return ErrBusyAuth
	case a.clientPreparing:
		return &smsBusyReason{"正在清理遗留号码并准备接码客户端，请稍后再试"}
	case a.fetchLease:
		return &smsBusyReason{"监控正在使用取号参数，请等待本轮收尾后再试"}
	case len(a.held) > 0:
		return &smsBusyReason{fmt.Sprintf("仍有 %d 个遗留号码未释放，清理完成前不可更换接码配置", len(a.held))}
	default:
		return nil
	}
}

// prepareMonitorClient must reclaim with the original client and its phone routes
// before replacing it. The preparation lease also fences manual tasks and edits.
func (a *AutoEnroller) prepareMonitorClient(ctx context.Context, build func(context.Context) (*haozhuma.Client, error)) error {
	a.mu.Lock()
	if a.running || a.fetchLease || a.clientPreparing {
		err := a.smsBusyLocked()
		a.mu.Unlock()
		return err
	}
	if len(a.held) > 0 && a.hzm == nil {
		a.mu.Unlock()
		return errors.New("遗留号码缺少原接码客户端，无法安全清理，请检查原接码账号")
	}
	a.clientPreparing = true
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.clientPreparing = false; a.mu.Unlock() }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if phones := a.heldPhones(); len(phones) > 0 {
		a.logf("监控启动前重试清理 %d 个遗留号码", len(phones))
		a.reclaimContext(ctx, phones, true)
		if err := ctx.Err(); err != nil {
			return err
		}
		if left := a.Status().Held; left > 0 {
			return fmt.Errorf("仍有 %d 个遗留号码未释放，监控将在下一轮自动重试清理；持续失败请在豪猪后台核对占用号码", left)
		}
	}
	c, err := build(ctx)
	if err != nil {
		return err
	}
	if c == nil {
		return errors.New("接码客户端未配置")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	a.hzm = c
	if project := c.Project(); project != "" {
		a.sid = project
	}
	return nil
}

func (a *AutoEnroller) applyFetchConfig(s haozhuma.Settings, persist func() error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.smsBusyLocked(); err != nil {
		return err
	}
	if err := persist(); err != nil {
		return err
	}
	if a.hzm == nil || a.hzm.Provider != "ecode" {
		a.sid = s.Sid
	}
	if a.hzm != nil && a.hzm.Provider != "ecode" {
		a.hzm.Author, a.hzm.ISP = s.Author, s.ISP
		a.hzm.SetUID(s.UID)
		a.hzm.SetUIDs(s.UIDs)
	}
	return nil
}

// 监控借用参数期间禁止配置发布和手动任务插入，收尾后才释放租约。
func (a *AutoEnroller) borrowMonitorFetch(sid, uid string) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.smsBusyLocked(); err != nil {
		return nil, err
	}
	if a.hzm == nil {
		return nil, errors.New("豪猪接码客户端未配置")
	}
	c := a.hzm
	oldSid, oldUID, oldUIDs := a.sid, c.UID(), c.UIDs()
	a.fetchLease = true
	a.sid = sid
	c.SetUID("")
	c.SetUIDs([]string{uid})
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.sid = oldSid
		c.SetUID(oldUID)
		c.SetUIDs(oldUIDs)
		a.fetchLease = false
	}, nil
}

func (h *Handler) setMonitorEnabled(enabled bool) error {
	w := h.cfg.UIDWatcher
	if w == nil {
		return errors.New("豪猪监控尚未配置")
	}
	w.mu.Lock()
	cfg := w.cfg
	cfg.Projects = append([]WatchProject(nil), cfg.Projects...)
	cfg.Groups = append([]string(nil), cfg.Groups...)
	w.mu.Unlock()
	cfg.Enabled = enabled
	clean, err := cfg.Validate()
	if err != nil {
		return err
	}
	if err = h.persistWatchConfig(clean); err != nil {
		return err
	}
	w.Reconfigure(clean)
	return nil
}
func (h *Handler) monitorEnabledPatch(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Enabled *bool `json:"enabled"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&p) != nil || p.Enabled == nil {
		writeJSON(w, 400, map[string]string{"error": "请提供 enabled"})
		return
	}
	if err := h.setMonitorEnabled(*p.Enabled); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "watch": h.cfg.UIDWatcher.Status()})
}
func (h *Handler) syncUIDSelection(ctx context.Context, before, after []string) []string {
	h5 := h.H5Client()
	mine, err := h5.MyUIDs(ctx, "")
	if err != nil {
		return []string{"读取豪猪账户失败：" + err.Error()}
	}
	have, want := map[string]bool{}, map[string]bool{}
	for _, u := range mine {
		have[u.UID] = true
	}
	for _, u := range after {
		want[u] = true
	}
	for _, u := range mine {
		if want[u.UID] && u.State == "已暂停" {
			return []string{"对接码 " + u.UID + " 已暂停，请先在豪猪网站确认是否恢复"}
		}
	}
	var failures []string
	for _, u := range after {
		if !have[u] {
			if err := h5.AddUID(ctx, u); err != nil {
				failures = append(failures, "加入 "+u+" 失败："+err.Error())
			}
		}
	}
	if len(failures) > 0 {
		return failures
	}
	var remove []string
	for _, u := range before {
		if !want[u] && have[u] {
			remove = append(remove, u)
		}
	}
	if len(remove) > 0 {
		if err := h5.RemoveUIDs(ctx, remove); err != nil {
			failures = append(failures, "移除旧码失败："+err.Error())
		}
	}
	return failures
}
