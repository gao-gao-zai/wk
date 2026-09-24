// reqproxy 管理端点（设计文档 3.7）。
//
// 挂载在 /admin/reqproxy/* 下，与登录代理池的 /admin/proxy/status 完全独立。
// 敏感信息纪律：订阅 URL 脱敏（token=***）、节点凭据（uuid/password）不下发。
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/reqproxy"
)

// reqproxyEnabled 模块未注入时统一 503。
func (h *Handler) reqproxyEnabled(w http.ResponseWriter) bool {
	if h.cfg.ReqProxy == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "请求代理模块未启用（reqproxy 未配置）"})
		return false
	}
	return true
}

// ---- config（enabled / 规则） ----

func (h *Handler) reqproxyConfigGet(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	h.cfg.ReqProxy.View(func(st *reqproxy.State) {
		rules := st.Rules
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled":       st.Enabled,
			"rules":         rules,
			"subscriptions": len(st.Subscriptions),
			"manual_nodes":  len(st.ManualNodes),
			"slots":         len(st.Slots),
			"bindings":      len(st.Bindings),
		})
	})
}

func (h *Handler) reqproxyConfigPut(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	var req struct {
		Enabled *bool           `json:"enabled"`
		Rules   *reqproxy.Rules `json:"rules"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不正确"})
		return
	}
	if req.Rules != nil {
		// 节奏参数越界直接拒绝（Normalize 只兜旧数据，不兜手写 payload）。
		if err := reqproxy.ValidateTTFBRules(*req.Rules); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	wasEnabled := false
	h.cfg.ReqProxy.View(func(st *reqproxy.State) { wasEnabled = st.Enabled })
	h.cfg.ReqProxy.Update(func(st *reqproxy.State) {
		if req.Enabled != nil {
			st.Enabled = *req.Enabled
		}
		if req.Rules != nil {
			rr := *req.Rules
			if rr.IncludeKeywords == nil {
				rr.IncludeKeywords = []string{}
			}
			if rr.ExcludeKeywords == nil {
				rr.ExcludeKeywords = []string{}
			}
			if rr.RegionRules == nil {
				rr.RegionRules = map[string][]string{}
			}
			rr.NormalizeTTFB()
			st.Rules = rr
		}
	})
	// 开关 false→true：立即预热（不等下一轮测速；未测速节点静态筛选
	// 通过即可分配，坏押注由首轮测速后的 fixup 修正）。
	if !wasEnabled && req.Enabled != nil && *req.Enabled {
		go h.cfg.ReqProxy.PrewarmIfDue()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- subscriptions ----

var subTokenPattern = regexp.MustCompile(`([?&])(token|key|auth|password|pass|secret)=[^&]*`)

// maskSubURL 订阅 URL 脱敏：token 类参数值打码。
func maskSubURL(u string) string {
	return subTokenPattern.ReplaceAllString(u, "$1$2=***")
}

func (h *Handler) reqproxySubsGet(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	h.cfg.ReqProxy.View(func(st *reqproxy.State) {
		out := make([]map[string]any, 0, len(st.Subscriptions))
		for _, s := range st.Subscriptions {
			out = append(out, map[string]any{
				"id": s.ID, "name": s.Name, "url": maskSubURL(s.URL),
				"auto_refresh": s.AutoRefresh, "interval": s.Interval,
				"last_refreshed_at": s.LastRefreshedAt,
				"last_status":       s.LastStatus, "last_error": s.LastError,
				"userinfo": s.Userinfo,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": out})
	})
}

func (h *Handler) reqproxySubsPost(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	var req struct {
		Name        string `json:"name"`
		URL         string `json:"url"`
		AutoRefresh bool   `json:"auto_refresh"`
		Interval    string `json:"interval"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不正确"})
		return
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.URL) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name 和 url 必填"})
		return
	}
	if req.Interval == "" {
		req.Interval = "1h"
	}
	sub := reqproxy.NewSubscription(req.Name, req.URL, req.AutoRefresh, req.Interval)
	h.cfg.ReqProxy.Update(func(st *reqproxy.State) {
		st.Subscriptions = append(st.Subscriptions, sub)
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": sub.ID})
}

func (h *Handler) reqproxySubPutDelete(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	id := r.PathValue("id")
	switch r.Method {
	case http.MethodDelete:
		h.cfg.ReqProxy.DeleteSubscription(id)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodPut:
		var req struct {
			Name        *string `json:"name"`
			URL         *string `json:"url"`
			AutoRefresh *bool   `json:"auto_refresh"`
			Interval    *string `json:"interval"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不正确"})
			return
		}
		h.cfg.ReqProxy.Update(func(st *reqproxy.State) {
			for i := range st.Subscriptions {
				if st.Subscriptions[i].ID == id {
					if req.Name != nil {
						st.Subscriptions[i].Name = *req.Name
					}
					if req.URL != nil {
						st.Subscriptions[i].URL = *req.URL
					}
					if req.AutoRefresh != nil {
						st.Subscriptions[i].AutoRefresh = *req.AutoRefresh
					}
					if req.Interval != nil && *req.Interval != "" {
						st.Subscriptions[i].Interval = *req.Interval
					}
				}
			}
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func (h *Handler) reqproxySubRefresh(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	// 任务化：立即返回任务 ID，前端轮询 /admin/reqproxy/jobs 驱动 loading
	id := h.cfg.ReqProxy.RefreshSubscriptionJob(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job_id": id})
}

func (h *Handler) reqproxySubPreview(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil || strings.TrimSpace(req.URL) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url 必填"})
		return
	}
	res, err := reqproxy.FetchSubscription(req.URL)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	// 只回节点概要（名称/协议/地区），不含凭据
	nodes := make([]map[string]any, 0, len(res.Nodes))
	for _, n := range res.Nodes {
		nodes = append(nodes, map[string]any{"name": n.Name, "protocol": n.Protocol, "region": n.Region})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":    len(res.Nodes),
		"nodes":    nodes,
		"userinfo": res.Userinfo,
		"warnings": res.Warnings,
	})
}

// ---- nodes ----

func (h *Handler) reqproxyNodesGet(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	nodes := h.cfg.ReqProxy.NodesSnapshot()
	slots, _ := h.cfg.ReqProxy.Views()
	ttfb := h.cfg.ReqProxy.TTFBResults()
	pointed := map[string]int{}
	for _, s := range slots {
		if s.NodeID != "" {
			pointed[s.NodeID]++
		}
	}
	out := make([]map[string]any, 0, len(nodes))
	loads := h.cfg.ReqProxy.NodeLoads()
	for _, n := range nodes {
		health := h.cfg.ReqProxy.HealthOf(n.ID)
		row := map[string]any{
			"id": n.ID, "name": n.Name, "protocol": n.Protocol,
			"source": n.Source, "region": n.Region,
			"probed":        n.Probed,
			"pointed_slots": pointed[n.ID],
			"requests":      loads[n.ID], // 累计请求数（真实负载）
			"latency_ms":    health.LatencyMs, "checked_at": health.CheckedAt,
			"unhealthy": health.Unhealthy, "unhealthy_until": health.UnhealthyUntil,
			"fail_streak": health.FailStreak, // >0 且非 unhealthy = 测过但失败（UI 区分"未测"与"失败"）
		}
		if health.TTFBChecked.IsZero() {
			if t, ok := ttfb[n.ID]; ok {
				row["ttfb_ms"] = t.TTFBMs
				row["ttfb_error"] = t.Error
				row["ttfb_checked_at"] = t.CheckedAt
				row["ttfb_skipped"] = t.Skipped
			}
		} else {
			row["ttfb_ms"] = health.TTFBMs
			row["ttfb_error"] = health.TTFBError
			row["ttfb_checked_at"] = health.TTFBChecked
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (h *Handler) reqproxyNodesImport(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不正确"})
		return
	}
	added, err := h.cfg.ReqProxy.ImportManualNodes(req.Text)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "added": added})
}

func (h *Handler) reqproxyNodeDelete(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	h.cfg.ReqProxy.DeleteManualNode(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- slots ----

func (h *Handler) reqproxySlotsGet(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	slots, bindings := h.cfg.ReqProxy.Views()
	bySlot := map[string][]string{}
	for _, b := range bindings {
		bySlot[b.SlotID] = append(bySlot[b.SlotID], b.UID)
	}
	nodes := map[string]map[string]any{}
	for _, n := range h.cfg.ReqProxy.NodesSnapshot() {
		health := h.cfg.ReqProxy.HealthOf(n.ID)
		nodes[n.ID] = map[string]any{
			"name": n.Name, "region": n.Region, "protocol": n.Protocol,
			"latency_ms": health.LatencyMs, "unhealthy": health.Unhealthy,
			"ttfb_ms": health.TTFBMs,
		}
	}
	out := make([]map[string]any, 0, len(slots))
	for _, s := range slots {
		entry := map[string]any{
			"id": s.ID, "port": s.Port, "region": s.Region, "since": s.Since,
			"accounts": bySlot[s.ID],
		}
		if s.NodeID != "" {
			entry["node_id"] = s.NodeID
			entry["node"] = nodes[s.NodeID]
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (h *Handler) reqproxySlotPut(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	var req struct {
		NodeID string `json:"node_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不正确"})
		return
	}
	if err := h.cfg.ReqProxy.ManualRetarget(r.PathValue("id"), req.NodeID); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- bindings ----

func (h *Handler) reqproxyBindingsGet(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	_, bindings := h.cfg.ReqProxy.Views()
	writeJSON(w, http.StatusOK, map[string]any{"data": bindings})
}

func (h *Handler) reqproxyBindingPut(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	var req struct {
		SlotID       *string `json:"slot_id"`
		PinnedNodeID *string `json:"pinned_node_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不正确"})
		return
	}
	uid := r.PathValue("uid")
	switch {
	case req.SlotID != nil && *req.SlotID == "":
		h.cfg.ReqProxy.UnbindAccount(uid)
	case req.PinnedNodeID != nil:
		h.cfg.ReqProxy.PinAccount(uid, *req.PinnedNodeID)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "slot_id 或 pinned_node_id 必填其一"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- health ----

func (h *Handler) reqproxyHealthRun(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	if nodeID := r.PathValue("node_id"); nodeID != "" {
		// 同步探测（秒级）：响应里直接带结果，按钮动画与结果同一条时序。
		latency, err := h.cfg.ReqProxy.ProbeNode(nodeID)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "latency_ms": latency})
		return
	}
	// 任务化：立即返回任务 ID，前端轮询驱动 loading（刷新不丢）
	id := h.cfg.ReqProxy.RunHealthOnce()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job_id": id, "note": "全量测速已开始（结束后自动预热+再平衡）"})
}

// reqproxyTTFBRun 用指定分组的账号对全部节点打一次最小对话，量真实首字。
//
// 与连通性测速分开：结果只供查看，不写健康状态、不参与摘除。
// body: {"group": "测速", "model": "glm-5.3", "concurrency": 4, "timeout_seconds": 60}
func (h *Handler) reqproxyTTFBRun(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	var req struct {
		Group          string `json:"group"`
		Model          string `json:"model"`
		Concurrency    int    `json:"concurrency"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil && err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不正确"})
		return
	}
	if req.Concurrency <= 0 {
		req.Concurrency = 4
	}
	if req.Concurrency > 16 {
		req.Concurrency = 16
	}
	if req.TimeoutSeconds <= 0 {
		req.TimeoutSeconds = 60
	}
	if req.TimeoutSeconds > 180 {
		req.TimeoutSeconds = 180
	}
	id, err := h.cfg.ReqProxy.RunTTFB(req.Group, req.Model, req.Concurrency, time.Duration(req.TimeoutSeconds)*time.Second)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job_id": id, "note": "真实首字测速已开始"})
}

// ttfbAccountSource 把分组存储 + 账号池适配成 reqproxy 的测速账号来源。
type ttfbAccountSource struct{ h *Handler }

func (s ttfbAccountSource) AccountsForGroup(group string) []reqproxy.TTFBAccount {
	var allow map[string]bool
	if group != "" && s.h.cfg.Groups != nil {
		allow = s.h.cfg.Groups.AccountsInGroup(group)
	}
	var out []reqproxy.TTFBAccount
	for _, uid := range s.h.cfg.Pool.UIDs() {
		if allow != nil && !allow[uid] {
			continue
		}
		a := s.h.cfg.Pool.AuthByUID(uid)
		if a == nil {
			continue
		}
		out = append(out, reqproxy.TTFBAccount{Auth: a, Region: a.Region()})
	}
	return out
}

// ttfbTokenRefresher 探测前按需刷新账号 token（自动/手动首字测速共用）。
type ttfbTokenRefresher struct{ h *Handler }

func (s ttfbTokenRefresher) EnsureFresh(a *auth.Auth) error {
	if a == nil {
		return fmt.Errorf("nil auth")
	}
	if s.h.cfg.Upstream == nil {
		return fmt.Errorf("upstream client 未配置")
	}
	if !a.NeedsRefresh(s.h.cfg.RefreshSkew) {
		return nil // 未到期：不打刷新接口
	}
	if err := s.h.cfg.Upstream.RefreshToken(a); err != nil {
		return err
	}
	// 刷新成功落盘（与线上请求路径同一份持久化）。
	if err := a.SaveAtomic(); err != nil {
		return fmt.Errorf("刷新成功但落盘失败: %w", err)
	}
	return nil
}

// reqproxyRebalancePost 手动触发负载再平衡（任务化）。
func (h *Handler) reqproxyRebalancePost(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	id, _ := h.cfg.ReqProxy.RebalanceNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job_id": id})
}

// reqproxyJobsGet 任务列表（前端恢复 running 动画 + 展示最近结果）。
func (h *Handler) reqproxyJobsGet(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	limit := 20
	if s := r.URL.Query().Get("limit"); s != "" {
		_, _ = fmt.Sscanf(s, "%d", &limit)
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": h.cfg.ReqProxy.Jobs(limit)})
}

// reqproxyPrewarmPost 手动触发预热（任务化）。
func (h *Handler) reqproxyPrewarmPost(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	id := h.cfg.ReqProxy.PrewarmNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job_id": id})
}

// reqproxyCompactPost 槽位整合（同节点多槽合并、删空槽；任务化）。
func (h *Handler) reqproxyCompactPost(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	id := h.cfg.ReqProxy.CompactNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job_id": id})
}

// reqproxyReassignPost 全量重分配槽位（清空重来；任务化）。
func (h *Handler) reqproxyReassignPost(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	id := h.cfg.ReqProxy.ReassignNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job_id": id})
}

// ---- events ----

func (h *Handler) reqproxyEventsGet(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	since := int64(0)
	if s := r.URL.Query().Get("since"); s != "" {
		_, _ = fmt.Sscanf(s, "%d", &since)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": h.cfg.ReqProxy.EventsSince(since)})
}

// ---- rules preview ----

func (h *Handler) reqproxyRulesPreview(w http.ResponseWriter, r *http.Request) {
	if !h.reqproxyEnabled(w) {
		return
	}
	var req struct {
		Rules *reqproxy.Rules `json:"rules"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不正确"})
		return
	}
	writeJSON(w, http.StatusOK, h.cfg.ReqProxy.PreviewRules(req.Rules))
}
