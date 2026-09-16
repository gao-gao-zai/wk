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

// TestAdminConfigHaozhumaSid 豪猪项目 ID 的 WebUI 闭环：
// GET 回显 sms.haozhuma.sid；POST 校验（纯数字、非空）+ 落盘 + 运行时
// 推给 AutoEnroller.SetSid。
func TestAdminConfigHaozhumaSid(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	seed := `{
  "sms": {
    "haozhuma": {
      "user": "u", "pass": "p", "sid": "52283",
      "uid": "keep-me", "isp": "1,2"
    }
  }
}`
	if err := os.WriteFile(cfgPath, []byte(seed), 0600); err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(t, Config{ConfigPath: cfgPath})
	// 手动组装最小 AutoEnroller（绕过 SMSLogin/HaozhumaClient 依赖）。
	h.cfg.AutoEnroll = NewAutoEnroller(nil, nil, "52283", nil, nil, nil)
	h.cfg.SetHaozhumaSid = h.cfg.AutoEnroll.SetSid

	// GET：回显当前 sid。
	req := httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET status %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		SMS struct {
			Haozhuma struct {
				Sid string `json:"sid"`
			} `json:"haozhuma"`
		} `json:"sms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.SMS.Haozhuma.Sid != "52283" {
		t.Fatalf("GET sid=%q, want 52283", got.SMS.Haozhuma.Sid)
	}

	// POST：非法值拒绝（schedule 是必填字段，补上合法值隔离变量）。
	for _, bad := range []string{"", "abc", "52-283", "  "} {
		body, _ := json.Marshal(map[string]any{
			"checkin_hours":   []int{3},
			"keepalive_hours": []int{9},
			"sms":             map[string]any{"haozhuma": map[string]any{"sid": bad}},
		})
		req := httptest.NewRequest(http.MethodPost, "/admin/config", strings.NewReader(string(body)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Fatalf("sid=%q: want 400, got %d", bad, rec.Code)
		}
	}

	// POST：合法值 → 运行时切换 + 落盘（其余 haozhuma 字段保留）。
	body, _ := json.Marshal(map[string]any{
		"checkin_hours":   []int{3},
		"keepalive_hours": []int{9},
		"sms":             map[string]any{"haozhuma": map[string]any{"sid": "68899"}},
	})
	req = httptest.NewRequest(http.MethodPost, "/admin/config", strings.NewReader(string(body)))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("POST status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Updated map[string]any `json:"updated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Updated["haozhuma_sid"] != "68899" {
		t.Fatalf("updated=%v, want haozhuma_sid=68899", resp.Updated)
	}
	if h.cfg.AutoEnroll.Sid() != "68899" {
		t.Fatalf("runtime sid=%q, want 68899 (SetSid must fire)", h.cfg.AutoEnroll.Sid())
	}
	// 落盘检查：sid 更新，user/uid/isp 原样。
	raw, _ := os.ReadFile(cfgPath)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	sms := doc["sms"].(map[string]any)["haozhuma"].(map[string]any)
	if sms["sid"] != "68899" {
		t.Fatalf("persisted sid=%v, want 68899", sms["sid"])
	}
	if sms["uid"] != "keep-me" || sms["user"] != "u" || sms["isp"] != "1,2" {
		t.Fatalf("persisted haozhuma lost fields: %v", sms)
	}
}

// TestSetSidThreadSafe SetSid 与 Sid/currentSid 并发不冲突（go test -race 覆盖）。
func TestSetSidThreadSafe(t *testing.T) {
	en := NewAutoEnroller(nil, nil, "1", nil, nil, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			en.SetSid("52")
			_ = en.Sid()
		}
	}()
	for i := 0; i < 200; i++ {
		en.SetSid("52283")
		_ = en.currentSid()
	}
	<-done
	if s := en.Sid(); s != "52283" && s != "52" {
		t.Fatalf("sid=%q", s)
	}
}
