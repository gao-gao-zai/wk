package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/pool"
)

// TestResponsesEndpointDisabledByConfig：DisableResponses=true 时两个
// Responses 路由都 404（含错误体），而不是 401/400——关闭语义是"端点不存在"。
// 构造的 handler 无凭据，newTestHandler 会注入测试 key，鉴权先过、再撞开关。
func TestResponsesEndpointDisabledByConfig(t *testing.T) {
	h := newTestHandler(t, Config{DisableResponses: true})
	for _, path := range []string{"/v1/responses", "/responses"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"glm-5.3","input":"hi"}`))
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404 (body: %s)", path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "endpoint_disabled") {
			t.Errorf("%s error body missing endpoint_disabled code: %s", path, rec.Body.String())
		}
	}
}

// TestResponsesEndpointEnabledByDefault：Config{} 零值 = 端点开（单测与
// 未显式配置的部署都依赖这一默认；DisableResponses 反转语义的原因）。
// 请求会走完 responses 前置校验并进入 chat 路径（空 Pool 会 5xx），
// 但绝不会是 404 endpoint_disabled —— 这正是断言点。
func TestResponsesEndpointEnabledByDefault(t *testing.T) {
	h := newTestHandler(t, Config{Pool: pool.New(t.TempDir())})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"glm-5.3","input":"hi"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// 不带上游时到不了 200，但必须不是 404 —— 证明端点本身开着。
	if rec.Code == http.StatusNotFound && strings.Contains(rec.Body.String(), "endpoint_disabled") {
		t.Fatalf("responses endpoint disabled by zero-value config: %d %s", rec.Code, rec.Body.String())
	}
}

// TestResponsesToggleRuntimeSwitch：WebUI 保存 features.responses_api=false
// 后运行时立刻 404，再打开立即恢复（无需重启）。
func TestResponsesToggleRuntimeSwitch(t *testing.T) {
	h, _ := adminConfigTestEnv(t)

	// 关闭。
	doAdminConfig(t, h, http.MethodPost, map[string]any{
		"checkin_hours":   []int{9},
		"keepalive_hours": []int{22},
		"features":        map[string]any{"responses_api": false},
	})
	if h.responsesAPIEnabled() {
		t.Fatal("responses should be disabled after save")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"glm-5.3","input":"hi"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled responses = %d, want 404", rec.Code)
	}

	// 重新打开：直接断言运行时开关（重新打开后发请求会走进 chat 路径，
	// 需要完整 Pool/Upstream 环境，此处不重复 EnabledByDefault 已覆盖的语义）。
	doAdminConfig(t, h, http.MethodPost, map[string]any{
		"checkin_hours":   []int{9},
		"keepalive_hours": []int{22},
		"features":        map[string]any{"responses_api": true},
	})
	if !h.responsesAPIEnabled() {
		t.Fatal("responses should be re-enabled after save")
	}
}

// TestAdminConfigGetResponsesAPIDefaultsOn：老配置文件没有 responses_api
// 字段时，GET 返回 true（默认开），前端开关不会误显示"关"。
func TestAdminConfigGetResponsesAPIDefaultsOn(t *testing.T) {
	h, _ := adminConfigTestEnv(t) // seed 里没有 responses_api 字段
	out := doAdminConfig(t, h, http.MethodGet, nil)
	features, ok := out["features"].(map[string]any)
	if !ok {
		t.Fatalf("features missing: %v", out)
	}
	if features["responses_api"] != true {
		t.Errorf("responses_api = %v, want true (default when absent)", features["responses_api"])
	}
}
