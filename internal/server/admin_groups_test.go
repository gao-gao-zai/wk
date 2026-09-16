package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/groups"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// newGroupTestHandler 构造带分组存储的 handler：2 个账号（u1 在 vip 组、
// u2 只在 default），一组密钥 + 管理员密钥。
func newGroupTestHandler(t *testing.T) (*Handler, string, string) {
	t.Helper()
	dir := t.TempDir()
	gs, err := groups.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := gs.Create("vip"); err != nil {
		t.Fatal(err)
	}

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "t1", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "t2", ExpiresAt: 9999999999})
	if err := gs.SetAccountGroups("u1", []string{"vip"}); err != nil {
		t.Fatal(err)
	}
	if err := gs.SetAccountGroups("u2", []string{groups.DefaultGroup}); err != nil {
		t.Fatal(err)
	}

	vipKey, err := gs.CreateKey("vip 密钥", "vip")
	if err != nil {
		t.Fatal(err)
	}
	defKey, err := gs.CreateKey("default 密钥", groups.DefaultGroup)
	if err != nil {
		t.Fatal(err)
	}

	up := upstream.New()
	h := NewHandler(Config{
		Pool:     p,
		Upstream: up,
		APIKey:   "admin-key",
		Groups:   gs,
	})
	return h, vipKey.Key, defKey.Key
}

// TestGroupKeyScopedAccountSelection 分组密钥只能选到本组账号；
// 管理员密钥不受限。用 /status 的 accounts 无法直接断言选号，改用
// chatCompletions 的选号路径（上游不可达 → 503，但 st.uid 记录选了谁）。
// 这里用更直接的单元断言：allowSet 的过滤语义。
func TestGroupKeyScopedAccountSelection(t *testing.T) {
	h, vipKey, defKey := newGroupTestHandler(t)

	// vip 密钥 → 只允许 u1。
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+vipKey)
	scope, ok := h.resolveScope(req)
	if !ok || scope.kind != "group" || scope.group != "vip" {
		t.Fatalf("vip key scope = %+v ok=%v", scope, ok)
	}
	allow := h.allowSet(scope)
	if len(allow) != 1 || !allow["u1"] {
		t.Fatalf("vip allow = %v, want {u1}", allow)
	}

	// default 密钥 → 只允许 u2。
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+defKey)
	scope, ok = h.resolveScope(req)
	if !ok || scope.kind != "group" || scope.group != "default" {
		t.Fatalf("default key scope = %+v ok=%v", scope, ok)
	}
	allow = h.allowSet(scope)
	if len(allow) != 1 || !allow["u2"] {
		t.Fatalf("default allow = %v, want {u2}", allow)
	}

	// 管理员密钥 → 不过滤（nil）。
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer admin-key")
	scope, ok = h.resolveScope(req)
	if !ok || scope.kind != "admin" || h.allowSet(scope) != nil {
		t.Fatalf("admin scope = %+v ok=%v", scope, ok)
	}

	// 错误密钥 → 不通过。
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	if _, ok := h.resolveScope(req); ok {
		t.Fatal("wrong key must not resolve")
	}
}

// TestGroupKeyCannotReachAdmin 分组密钥不能访问管理面；管理员密钥可以。
func TestGroupKeyCannotReachAdmin(t *testing.T) {
	h, vipKey, _ := newGroupTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/admin/groups", nil)
	req.Header.Set("Authorization", "Bearer "+vipKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("group key on /admin/groups = %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/groups", nil)
	req.Header.Set("Authorization", "Bearer admin-key")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin key on /admin/groups = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"vip"`) {
		t.Fatalf("groups list missing vip: %s", rec.Body.String())
	}
}

// TestAdminGroupsAPI 分组 CRUD 的 HTTP 层行为。
func TestAdminGroupsAPI(t *testing.T) {
	h, _, _ := newGroupTestHandler(t)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer admin-key")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 创建。
	if rec := do(http.MethodPost, "/admin/groups", `{"name":"test2"}`); rec.Code != 200 {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	// 重复创建 → 400。
	if rec := do(http.MethodPost, "/admin/groups", `{"name":"test2"}`); rec.Code != 400 {
		t.Fatalf("dup create = %d, want 400", rec.Code)
	}
	// 改名 default → 403。
	if rec := do(http.MethodPut, "/admin/groups/default", `{"name":"x"}`); rec.Code != 403 {
		t.Fatalf("rename default = %d, want 403", rec.Code)
	}
	// 删 default → 403。
	if rec := do(http.MethodDelete, "/admin/groups/default", ""); rec.Code != 403 {
		t.Fatalf("delete default = %d, want 403", rec.Code)
	}
	// vip 组有账号 u1（唯一归属）→ 删除 409。
	if rec := do(http.MethodDelete, "/admin/groups/vip", ""); rec.Code != 409 {
		t.Fatalf("delete vip = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	// 改名 vip → premium，账号归属同步迁移。
	if rec := do(http.MethodPut, "/admin/groups/vip", `{"name":"premium"}`); rec.Code != 200 {
		t.Fatalf("rename vip = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodGet, "/admin/accounts/u1/groups", ""); rec.Code != 200 {
		t.Fatalf("get account groups = %d", rec.Code)
	} else if !strings.Contains(rec.Body.String(), "premium") {
		t.Fatalf("u1 groups after rename = %s", rec.Body.String())
	}
}

// TestAdminKeysAPI 密钥 CRUD 的 HTTP 层 + 账号归属 PUT。
func TestAdminKeysAPI(t *testing.T) {
	h, _, _ := newGroupTestHandler(t)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer admin-key")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 创建密钥（完整回显一次）。
	rec := do(http.MethodPost, "/admin/keys", `{"name":"k1","group":"vip"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "wk-") {
		t.Fatalf("create key = %d: %s", rec.Code, rec.Body.String())
	}
	// 列表脱敏。
	rec = do(http.MethodGet, "/admin/keys", "")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"key":"wk-`) && strings.Count(rec.Body.String(), "wk-") < 0 {
		t.Fatalf("list keys = %d: %s", rec.Code, rec.Body.String())
	}
	// 不存在的分组 → 400。
	if rec := do(http.MethodPost, "/admin/keys", `{"group":"nope"}`); rec.Code != 400 {
		t.Fatalf("key with unknown group = %d, want 400", rec.Code)
	}
	// 账号归属 PUT：u2 挪进 vip（多归属）。
	if rec := do(http.MethodPut, "/admin/accounts/u2/groups", `{"groups":["default","vip"]}`); rec.Code != 200 {
		t.Fatalf("set account groups = %d: %s", rec.Code, rec.Body.String())
	}
	// 不存在的账号 → 404。
	if rec := do(http.MethodPut, "/admin/accounts/ghost/groups", `{"groups":["default"]}`); rec.Code != 404 {
		t.Fatalf("ghost account = %d, want 404", rec.Code)
	}
}

// TestPickForModelAllowScoped Pool 层的分组过滤选号：allow 只含 u1 时
// 永远选不到 u2（多次轮换防偶然）。
func TestPickForModelAllowScoped(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "t1", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "t2", ExpiresAt: 9999999999})
	allow := map[string]bool{"u2": true}
	for i := 0; i < 20; i++ {
		a := p.PickAndAcquireForModelAllow("", nil, allow)
		if a == nil {
			t.Fatal("scoped pick returned nil with a healthy candidate")
		}
		if a.UID != "u2" {
			t.Fatalf("scoped pick = %s, want u2", a.UID)
		}
		p.Release(a.UID)
	}
	// 空 allow map（分组无账号）→ nil，绝不回落全池。
	if a := p.PickAndAcquireForModelAllow("", nil, map[string]bool{}); a != nil {
		t.Fatalf("empty allow pick = %s, want nil", a.UID)
	}
	// nil allow = 全池。
	a := p.PickAndAcquireForModelAllow("", nil, nil)
	if a == nil {
		t.Fatal("nil allow must pick from the full pool")
	}
	p.Release(a.UID)
}
