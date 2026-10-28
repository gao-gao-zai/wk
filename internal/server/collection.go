package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/collection"
	"workbuddy2api/internal/upstream"
)

func (h *Handler) registerCollection() {
	h.mux.HandleFunc("GET /admin/collection", h.withFrontend(h.collectionOverview))
	h.mux.HandleFunc("PUT /admin/collection/settings", h.withFrontend(h.collectionSettings))
	h.mux.HandleFunc("POST /admin/collection/jobs", h.withFrontend(h.collectionSubmit))
	h.mux.HandleFunc("GET /admin/collection/jobs/{id}", h.withFrontend(h.collectionJob))
	h.mux.HandleFunc("POST /admin/collection/jobs/{id}/cancel", h.withFrontend(h.collectionCancel))
	h.mux.HandleFunc("POST /admin/collection/jobs/{id}/retry", h.withFrontend(h.collectionRetry))
	h.mux.HandleFunc("GET /admin/collection/snapshots", h.withFrontend(h.collectionSnapshots))
}
func (h *Handler) collectionReady(w http.ResponseWriter) bool {
	w.Header().Set("Cache-Control", "no-store")
	if h.cfg.Collection == nil {
		writeJSON(w, 503, map[string]string{"error": "采集存储不可用，未启动在线采集"})
		return false
	}
	return true
}
func (h *Handler) collectionOverview(w http.ResponseWriter, r *http.Request) {
	if !h.collectionReady(w) {
		return
	}
	jobs, err := h.cfg.Collection.List()
	writeJSON(w, 200, map[string]any{"jobs": jobs, "settings": h.cfg.Collection.Settings(), "storage_error": err, "now": time.Now().Unix()})
}
func decodeCollection(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		writeJSON(w, 400, map[string]string{"error": "无效采集参数"})
		return false
	}
	return true
}
func (h *Handler) collectionSettings(w http.ResponseWriter, r *http.Request) {
	if !h.collectionReady(w) {
		return
	}
	var s collection.Settings
	if !decodeCollection(w, r, &s) {
		return
	}
	if err := h.cfg.Collection.Configure(s); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "settings": s})
}
func (h *Handler) collectionSubmit(w http.ResponseWriter, r *http.Request) {
	if !h.collectionReady(w) {
		return
	}
	var req collection.Request
	if !decodeCollection(w, r, &req) {
		return
	}
	req.Source = "manual"
	j, err := h.cfg.Collection.Submit(req)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 202, map[string]any{"ok": true, "job": j, "job_id": j.ID})
}
func (h *Handler) collectionJob(w http.ResponseWriter, r *http.Request) {
	if !h.collectionReady(w) {
		return
	}
	j, ok := h.cfg.Collection.Job(r.PathValue("id"))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "任务不存在"})
		return
	}
	status := r.URL.Query().Get("status")
	items := j.Items
	j.Items = nil
	filtered := []*collection.Item{}
	for _, i := range items {
		if status == "" || i.Status == status {
			filtered = append(filtered, i)
		}
	}
	sort.Slice(filtered, func(a, b int) bool {
		if filtered[a].UID == filtered[b].UID {
			return filtered[a].Resource < filtered[b].Resource
		}
		return filtered[a].UID < filtered[b].UID
	})
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page > 1000000 {
		writeJSON(w, 400, map[string]string{"error": "页码超出范围"})
		return
	}
	if page < 1 {
		page = 1
	}
	size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if size < 1 || size > 100 {
		size = 25
	}
	offset := (page - 1) * size
	if offset > len(filtered) {
		offset = len(filtered)
	}
	end := offset + size
	if end > len(filtered) {
		end = len(filtered)
	}
	writeJSON(w, 200, map[string]any{"job": j, "items": filtered[offset:end], "total_items": len(filtered), "page": page})
}
func (h *Handler) collectionCancel(w http.ResponseWriter, r *http.Request) {
	if !h.collectionReady(w) {
		return
	}
	j, e := h.cfg.Collection.Cancel(r.PathValue("id"))
	if e != nil {
		writeJSON(w, 400, map[string]string{"error": e.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "job": j})
}
func (h *Handler) collectionRetry(w http.ResponseWriter, r *http.Request) {
	if !h.collectionReady(w) {
		return
	}
	j, e := h.cfg.Collection.Retry(r.PathValue("id"))
	if e != nil {
		writeJSON(w, 400, map[string]string{"error": e.Error()})
		return
	}
	writeJSON(w, 202, map[string]any{"ok": true, "job": j, "job_id": j.ID})
}
func (h *Handler) collectionSnapshots(w http.ResponseWriter, r *http.Request) {
	if !h.collectionReady(w) {
		return
	}
	uids := strings.Split(r.URL.Query().Get("uids"), ",")
	if len(uids) > 100 {
		writeJSON(w, 400, map[string]string{"error": "每次最多查询 100 个账号"})
		return
	}
	out := []collection.Snapshot{}
	for _, uid := range uids {
		if h.cfg.Pool.AuthByUID(uid) == nil {
			continue
		}
		for _, resource := range collection.Resources {
			s := h.cfg.Collection.Snapshot(uid, resource)
			if r.URL.Query().Get("data") != "1" {
				s.Data = nil
			}
			out = append(out, s)
		}
	}
	writeJSON(w, 200, map[string]any{"snapshots": out})
}

// Cached models are partitioned by account (therefore region and entitlements),
// and filtered by the authenticated API key's allowed account set.
func (h *Handler) collectedModels(allow map[string]bool) []upstream.ModelInfo {
	out := []upstream.ModelInfo{}
	seen := map[string]bool{}
	for _, st := range h.cfg.Pool.List() {
		if st.Disabled || allow != nil && !allow[st.UID] {
			continue
		}
		snap := h.cfg.Collection.Snapshot(st.UID, "models")
		var infos []upstream.ModelInfo
		if json.Unmarshal(snap.Data, &infos) != nil {
			continue
		}
		for _, i := range infos {
			if !seen[i.ID] {
				seen[i.ID] = true
				out = append(out, i)
			}
		}
	}
	return out
}
func (h *Handler) collectAfterAction(uid string) {
	if h.cfg.Collection != nil && h.cfg.Pool.AuthByUID(uid) != nil {
		h.cfg.Collection.Invalidate(uid, []string{"balance", "tasks"})
		_, _ = h.cfg.Collection.Submit(collection.Request{UIDs: []string{uid}, Resources: []string{"balance", "tasks"}, Source: "action"})
	}
}
