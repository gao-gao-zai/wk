package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"workbuddy2api/internal/groups"
)

// —— 分组管理 ——

// adminGroups GET 列表 / POST 新建。
func (h *Handler) adminGroups(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Groups == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "分组存储未启用"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		// 附带每组的账号数与密钥数，管理页直接可渲染。
		own := h.cfg.Groups.SnapshotAccountGroups()
		counts := map[string]int{}
		for _, gs := range own {
			for _, g := range gs {
				counts[g]++
			}
		}
		keyCounts := map[string]int{}
		for _, k := range h.cfg.Groups.ListKeys() {
			if k.Group != "" {
				keyCounts[k.Group]++
			}
		}
		items := make([]map[string]any, 0)
		for _, g := range h.cfg.Groups.List() {
			items = append(items, map[string]any{
				"name":     g,
				"default":  g == groups.DefaultGroup,
				"accounts": counts[g],
				"keys":     keyCounts[g],
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"groups": items})
	case http.MethodPost:
		var req struct {
			Name string `json:"name"`
		}
		if err := jsonDecodeLimited(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不正确"})
			return
		}
		if err := h.cfg.Groups.Create(req.Name); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": req.Name})
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// adminGroup 单分组：PUT 重命名 / DELETE 删除。
func (h *Handler) adminGroup(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Groups == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "分组存储未启用"})
		return
	}
	name := r.PathValue("name")
	switch r.Method {
	case http.MethodPut:
		var req struct {
			Name string `json:"name"`
		}
		if err := jsonDecodeLimited(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不正确"})
			return
		}
		if err := h.cfg.Groups.Rename(name, req.Name); err != nil {
			writeJSON(w, statusForGroupErr(err), map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodDelete:
		if err := h.cfg.Groups.Delete(name); err != nil {
			writeJSON(w, statusForGroupErr(err), map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func statusForGroupErr(err error) int {
	switch {
	case errors.Is(err, groups.ErrImmutable):
		return http.StatusForbidden
	case errors.Is(err, groups.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, groups.ErrGroupInUse):
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

// adminAccountGroups GET 查 / PUT 改账号归属。
func (h *Handler) adminAccountGroups(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Groups == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "分组存储未启用"})
		return
	}
	uid := r.PathValue("uid")
	if h.cfg.Pool == nil || h.cfg.Pool.AuthByUID(uid) == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号不存在"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "groups": h.cfg.Groups.AccountGroups(uid)})
	case http.MethodPut:
		var req struct {
			Groups []string `json:"groups"`
		}
		if err := jsonDecodeLimited(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不正确"})
			return
		}
		if err := h.cfg.Groups.SetAccountGroups(uid, req.Groups); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "groups": h.cfg.Groups.AccountGroups(uid)})
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// —— 密钥管理 ——

// adminKeys GET 列表（脱敏）/ POST 创建（完整密钥只在响应里出现一次）。
func (h *Handler) adminKeys(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Groups == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "分组存储未启用"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"keys": h.cfg.Groups.ListKeys()})
	case http.MethodPost:
		var req struct {
			Name  string `json:"name"`
			Group string `json:"group"`
		}
		if err := jsonDecodeLimited(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不正确"})
			return
		}
		if req.Group == "" {
			req.Group = groups.DefaultGroup
		}
		k, err := h.cfg.Groups.CreateKey(req.Name, req.Group)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "id": k.ID, "key": k.Key, "name": k.Name, "group": k.Group,
			"note": "完整密钥仅此一次回显，请立即保存",
		})
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// adminKey 单密钥：PUT 改备注/改绑分组 / DELETE 删除。
func (h *Handler) adminKey(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Groups == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "分组存储未启用"})
		return
	}
	id := r.PathValue("id")
	switch r.Method {
	case http.MethodPut:
		var req struct {
			Name  string `json:"name"`
			Group string `json:"group"`
		}
		if err := jsonDecodeLimited(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式不正确"})
			return
		}
		if err := h.cfg.Groups.UpdateKey(id, req.Name, req.Group); err != nil {
			writeJSON(w, statusForGroupErr(err), map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodDelete:
		if err := h.cfg.Groups.DeleteKey(id); err != nil {
			writeJSON(w, statusForGroupErr(err), map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// jsonDecodeLimited 限长解码 JSON body。
func jsonDecodeLimited(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(v)
}
