package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAdminConfigRequestLogRetention 请求日志保留策略的 WebUI 闭环：
// GET 回显、POST 校验 + 落盘 + 运行时推送。
func TestAdminConfigRequestLogRetention(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	seed := `{"api_key":"k","request_log_retention_days":3,"request_log_retention_rows":20000}`
	if err := os.WriteFile(cfgPath, []byte(seed), 0600); err != nil {
		t.Fatal(err)
	}
	var pushedDays, pushedRows int
	h := newTestHandler(t, Config{
		ConfigPath: cfgPath,
		SetRequestLogRetention: func(days, rows int) {
			pushedDays, pushedRows = days, rows
		},
	})

	// GET：回显当前策略。
	req := httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET status %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Retention struct {
			Days int `json:"days"`
			Rows int `json:"rows"`
		} `json:"request_log_retention"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Retention.Days != 3 || got.Retention.Rows != 20000 {
		t.Fatalf("GET retention=%+v, want {3 20000}", got.Retention)
	}

	// POST：非法值拒绝（负数/超范围）。
	for _, bad := range []map[string]*int{
		{"days": intPtr(-1)},
		{"days": intPtr(4000)},
		{"rows": intPtr(50)}, // < 100 下限
		{"rows": intPtr(-5)},
		{"rows": intPtr(2000000)},
	} {
		body, _ := json.Marshal(map[string]any{
			"checkin_hours":         []int{3},
			"keepalive_hours":       []int{9},
			"request_log_retention": bad,
		})
		req := httptest.NewRequest(http.MethodPost, "/admin/config", strings.NewReader(string(body)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Fatalf("retention=%v: want 400, got %d", bad, rec.Code)
		}
	}

	// POST：只改 days（rows 增量保留文件当前值）。
	body, _ := json.Marshal(map[string]any{
		"checkin_hours":   []int{3},
		"keepalive_hours": []int{9},
		"request_log_retention": map[string]any{
			"days": 7, // rows 不给 → 沿用 20000
		},
	})
	req = httptest.NewRequest(http.MethodPost, "/admin/config", strings.NewReader(string(body)))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("POST status %d: %s", rec.Code, rec.Body.String())
	}
	if pushedDays != 7 || pushedRows != 20000 {
		t.Fatalf("pushed days=%d rows=%d, want 7/20000 (unset fields keep current)", pushedDays, pushedRows)
	}
	// 落盘检查。
	raw, _ := os.ReadFile(cfgPath)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["request_log_retention_days"] != float64(7) || doc["request_log_retention_rows"] != float64(20000) {
		t.Fatalf("persisted days=%v rows=%v, want 7/20000", doc["request_log_retention_days"], doc["request_log_retention_rows"])
	}

	// POST：days=0 显式清零（= 不限时间，仅按条数）。
	body, _ = json.Marshal(map[string]any{
		"checkin_hours":   []int{3},
		"keepalive_hours": []int{9},
		"request_log_retention": map[string]any{
			"days": 0, "rows": 50000,
		},
	})
	req = httptest.NewRequest(http.MethodPost, "/admin/config", strings.NewReader(string(body)))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("POST(0) status %d: %s", rec.Code, rec.Body.String())
	}
	if pushedDays != 0 || pushedRows != 50000 {
		t.Fatalf("pushed days=%d rows=%d, want 0/50000", pushedDays, pushedRows)
	}
}

// TestAdminConfigRetentionNoHook 未注入回调（无持久化存储）时只落盘，
// 响应带 restart_required 标记。
func TestAdminConfigRetentionNoHook(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"api_key":"k"}`), 0600); err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(t, Config{ConfigPath: cfgPath})

	body, _ := json.Marshal(map[string]any{
		"checkin_hours":   []int{3},
		"keepalive_hours": []int{9},
		"request_log_retention": map[string]any{
			"days": 30, "rows": 100000,
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/config", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Updated map[string]any `json:"updated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if _, ok := resp.Updated["request_log_retention_restart_required"]; !ok {
		t.Fatalf("updated=%v, want restart_required marker", resp.Updated)
	}
}

func intPtr(v int) *int { return &v }
