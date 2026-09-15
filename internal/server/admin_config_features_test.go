package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// adminConfigTestEnv 组装一个带临时 config.json 的 handler。
// 返回的 getter 每次重新读盘，验证"落盘持久化"（而不是只看内存副本）。
func adminConfigTestEnv(t *testing.T) (authedHandler, string) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	seed := `{
  "listen": "127.0.0.1:7863",
  "schedule": {"checkin_hours": [9, 21], "keepalive_hours": [22]},
  "features": {"sanitize_blacklist_fingerprints": true, "passthrough": false, "codex_compat": true},
  "billing": {"input_credits_per_1k_tokens": 0.01, "output_credits_per_1k_tokens": 0.05, "cached_input_credits_per_1k_tokens": 0.001}
}`
	if err := os.WriteFile(configPath, []byte(seed), 0600); err != nil {
		t.Fatal(err)
	}
	// CreditPolicy/Features 的运行时副本模拟 main 的注入：handler 构造时
	// 从（已加载的）启动配置拷贝。测试不走 main，所以这里手动给。
	h := newTestHandler(t, Config{
		ConfigPath: configPath,
		CreditPolicy: CreditPolicy{InputPer1K: 0.01, OutputPer1K: 0.05, CachedInputPer1K: 0.001},
	})
	return h, configPath
}

func doAdminConfig(t *testing.T, h authedHandler, method string, body any) map[string]any {
	t.Helper()
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, "/admin/config", nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req = httptest.NewRequest(method, "/admin/config", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%s /admin/config = %d: %s", method, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

// TestAdminConfigGetExposesFeaturesAndBilling：GET 返回 WebUI 可改字段，
// 前端据此渲染开关与费率表单。
func TestAdminConfigGetExposesFeaturesAndBilling(t *testing.T) {
	h, _ := adminConfigTestEnv(t)
	out := doAdminConfig(t, h, http.MethodGet, nil)

	features, ok := out["features"].(map[string]any)
	if !ok {
		t.Fatalf("features missing in GET /admin/config: %v", out)
	}
	if features["codex_compat"] != true {
		t.Errorf("codex_compat = %v, want true (seeded)", features["codex_compat"])
	}
	if features["sanitize_blacklist_fingerprints"] != true {
		t.Errorf("sanitize = %v, want true", features["sanitize_blacklist_fingerprints"])
	}
	billing, ok := out["billing"].(map[string]any)
	if !ok {
		t.Fatalf("billing missing in GET /admin/config: %v", out)
	}
	if billing["input_credits_per_1k_tokens"] != 0.01 {
		t.Errorf("input rate = %v, want 0.01", billing["input_credits_per_1k_tokens"])
	}
}

// TestSaveAdminConfigFeaturesRuntimeAndPersist：保存特性开关后
// (a) 运行时副本立即翻转（requestPassthrough 反映）；
// (b) config.json 落盘持久化（重启后仍生效）；
// (c) upstream 回调被调用（通过注入的 spy 验证）。
func TestSaveAdminConfigFeaturesRuntimeAndPersist(t *testing.T) {
	h, configPath := adminConfigTestEnv(t)
	sanitizeSpy := false
	codexSpy := false
	h.featuresMu.Lock()
	h.sanitizeHooks.setSanitize = func(v bool) { sanitizeSpy = v }
	h.sanitizeHooks.setCodex = func(v bool) { codexSpy = v }
	h.featuresMu.Unlock()

	out := doAdminConfig(t, h, http.MethodPost, map[string]any{
		"checkin_hours":   []int{9, 21},
		"keepalive_hours": []int{22},
		"features": map[string]any{
			"passthrough": true,
			"codex_compat": false,
		},
	})

	updated, _ := out["updated"].(map[string]any)
	if updated["passthrough"] != true {
		t.Errorf("updated.passthrough = %v, want true", updated["passthrough"])
	}
	if h.currentPassthrough() != true {
		t.Error("runtime passthrough not updated")
	}
	if codexSpy != false {
		t.Error("codex setter was not called with false")
	}
	if sanitizeSpy != false {
		t.Error("sanitize setter should not fire when field omitted")
	}

	// 落盘验证：重新读文件，确认 features 合并正确（未提供的 sanitize 保持 true）。
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Features struct {
			SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
			Passthrough                   bool `json:"passthrough"`
			CodexCompat                   bool `json:"codex_compat"`
		} `json:"features"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.Features.SanitizeBlacklistFingerprints {
		t.Error("omitted sanitize flag must persist as-is (true)")
	}
	if !doc.Features.Passthrough {
		t.Error("passthrough not persisted")
	}
	if doc.Features.CodexCompat {
		t.Error("codex_compat should be persisted as false")
	}
}

// TestSaveAdminConfigBillingRuntimeAndPersist：费率保存后运行时副本即时更新
// 且落盘；负数/NaN 被拒。
func TestSaveAdminConfigBillingRuntimeAndPersist(t *testing.T) {
	h, configPath := adminConfigTestEnv(t)

	doAdminConfig(t, h, http.MethodPost, map[string]any{
		"checkin_hours":   []int{9},
		"keepalive_hours": []int{22},
		"billing": map[string]any{
			"input_credits_per_1k_tokens":  0.02,
			"output_credits_per_1k_tokens": 0.08,
		},
	})

	policy := h.currentCreditPolicy()
	if policy.InputPer1K != 0.02 || policy.OutputPer1K != 0.08 {
		t.Errorf("runtime policy = %+v, want input=0.02 output=0.08", policy)
	}
	if policy.CachedInputPer1K != 0.001 {
		t.Errorf("omitted cached rate must keep seeded value, got %v", policy.CachedInputPer1K)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Billing struct {
			Input  float64 `json:"input_credits_per_1k_tokens"`
			Output float64 `json:"output_credits_per_1k_tokens"`
			Cached float64 `json:"cached_input_credits_per_1k_tokens"`
		} `json:"billing"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Billing.Input != 0.02 || doc.Billing.Output != 0.08 || doc.Billing.Cached != 0.001 {
		t.Errorf("persisted billing = %+v", doc.Billing)
	}
}

// TestSaveAdminConfigRejectsInvalidBilling：负费率返回 400，且不落盘、
// 不改运行时副本。
func TestSaveAdminConfigRejectsInvalidBilling(t *testing.T) {
	h, configPath := adminConfigTestEnv(t)
	before := h.currentCreditPolicy()

	req := httptest.NewRequest(http.MethodPost, "/admin/config", bytes.NewReader([]byte(
		`{"checkin_hours":[9],"keepalive_hours":[22],"billing":{"input_credits_per_1k_tokens":-1}}`,
	)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("negative billing rate accepted: %d %s", rec.Code, rec.Body.String())
	}

	if after := h.currentCreditPolicy(); after != before {
		t.Errorf("runtime policy changed on rejected request: %+v", after)
	}
	raw, _ := os.ReadFile(configPath)
	var doc struct {
		Billing struct {
			Input float64 `json:"input_credits_per_1k_tokens"`
		} `json:"billing"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Billing.Input != 0.01 {
		t.Errorf("rejected request still mutated config file: %v", doc.Billing.Input)
	}
}

// TestSaveAdminConfigScheduleOnlyBackwardCompat：老前端只发 schedule 时
// features/billing 落盘值保持不变（可选字段语义）。
func TestSaveAdminConfigScheduleOnlyBackwardCompat(t *testing.T) {
	h, configPath := adminConfigTestEnv(t)
	doAdminConfig(t, h, http.MethodPost, map[string]any{
		"checkin_hours":   []int{10},
		"keepalive_hours": []int{23},
	})
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Features struct {
			CodexCompat bool `json:"codex_compat"`
		} `json:"features"`
		Billing struct {
			Input float64 `json:"input_credits_per_1k_tokens"`
		} `json:"billing"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.Features.CodexCompat || doc.Billing.Input != 0.01 {
		t.Errorf("schedule-only save clobbered features/billing: %+v", doc)
	}
}
