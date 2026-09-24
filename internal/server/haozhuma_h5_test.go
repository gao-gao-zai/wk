package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	h5 "workbuddy2api/internal/haozhumah5"
)

// newFakeH5Server 起 fake H5（项目搜索/对接码列表）。与 haozhumah5 包内
// 的 fake 语义一致但独立维护——handler 测试只需要固定响应。
// validSession 非空时校验值（不匹配 = 会话失效路径），空 = 只要有
// PHPSESSID 就放行。
func newFakeH5Server(t *testing.T, validSession string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := r.Cookie("PHPSESSID")
		if err != nil || got.Value == "" || (validSession != "" && got.Value != validSession) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"code":-1,"data":null,"msg":"请选择正确的API"}`))
			return
		}
		switch r.URL.Path {
		case "/api.php":
			switch r.URL.Query().Get("type") {
			case "30":
				w.Write([]byte(`{"code":1,"data":[
  {"name":"【52283】腾讯科技[限对接] [70da814e38b466b6]","sid":"70da814e38b466b6"},
  {"name":"【61904】腾讯科技3次[限对接] [1972972d69bd0453]","sid":"1972972d69bd0453"}
],"msg":"Success"}`))
			case "8":
				w.Write([]byte(`{"code":1,"data":[
  {"sid":"52283","mc":"[52283]腾讯科技[限对接]","uid":"52283-WW9L2J4WOL","yhj":"16.500","zxky":"可用数量:23","yyy":"移动|电信|联通|","sheng":"","haoduan":"未知号段","hd":"198|193|","time":"2026-09-22 23:24:18","zd":"1"}
],"msg":"Success"}`))
			default:
				w.Write([]byte(`{"code":-1,"data":null,"msg":"unknown type"}`))
			}
		case "/time.php":
			w.Write([]byte(time.Now().Format(time.RFC3339)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := h5.BaseURL
	u, _ := url.Parse(srv.URL)
	h5.BaseURL = u.String()
	t.Cleanup(func() { h5.BaseURL = old })
	return srv
}

// TestHaozhumaH5SessionPaste 粘贴 PHPSESSID 闭环：验证（真调 type=30）
// → 落盘 → 运行时 client 热更（含保活启动）。坏会话拒绝且不落盘。
func TestHaozhumaH5SessionPaste(t *testing.T) {
	newFakeH5Server(t, "good-session")
	seed := `{"sms":{"haozhuma":{"user":"u","pass":"p","sid":"52283"}}}`
	ah, _, cfgPath := newWebuiTestHandler(t, seed)

	// 空 session：显式清空语义，200；配置里本来就没有 h5_session。
	req := httptest.NewRequest(http.MethodPost, "/admin/account/sms/haozhuma/h5-session",
		strings.NewReader(`{"session":""}`))
	rec := httptest.NewRecorder()
	ah.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("clear-empty should be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	raw, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(raw), "h5_session") {
		t.Fatalf("empty clear should not write h5_session: %s", raw)
	}

	// 坏会话（fake 只认 good-session）：验证失败 400，不落盘、不建 client。
	req = httptest.NewRequest(http.MethodPost, "/admin/account/sms/haozhuma/h5-session",
		strings.NewReader(`{"session":"bad-session"}`))
	rec = httptest.NewRecorder()
	ah.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("bad session should be 400, got %d", rec.Code)
	}
	raw, _ = os.ReadFile(cfgPath)
	if strings.Contains(string(raw), "bad-session") {
		t.Fatalf("bad session must not persist: %s", raw)
	}
	if ah.Handler.cfg.HaozhumaH5 != nil {
		t.Fatal("failed paste must not create runtime client")
	}

	// 好会话：验证 → 落盘 → 运行时注入（转正 + 保活）。
	req = httptest.NewRequest(http.MethodPost, "/admin/account/sms/haozhuma/h5-session",
		strings.NewReader(`{"session":"good-session"}`))
	rec = httptest.NewRecorder()
	ah.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("good session paste: %d: %s", rec.Code, rec.Body.String())
	}
	raw, _ = os.ReadFile(cfgPath)
	if !strings.Contains(string(raw), `"h5_session": "good-session"`) {
		t.Fatalf("h5_session not persisted: %s", raw)
	}
	if ah.Handler.cfg.HaozhumaH5 == nil {
		t.Fatal("runtime H5 client should be created after paste")
	}
	if !ah.Handler.cfg.HaozhumaH5.Info().Has {
		t.Fatal("runtime H5 client should hold the session")
	}
	ah.Handler.cfg.HaozhumaH5.Close()

	// 回显：has=true，值本体不出现。
	req = httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	rec = httptest.NewRecorder()
	ah.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("config echo: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "good-session") {
		t.Fatalf("PHPSESSID leaked in echo: %s", rec.Body.String())
	}
	var echo struct {
		SMS struct {
			Haozhuma struct {
				H5 struct {
					Has  bool `json:"has"`
					Conf bool `json:"configured"`
				} `json:"h5"`
			} `json:"haozhuma"`
		} `json:"sms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &echo); err != nil {
		t.Fatal(err)
	}
	if !echo.SMS.Haozhuma.H5.Has || !echo.SMS.Haozhuma.H5.Conf {
		t.Fatalf("h5 echo = %+v, want has+configured", echo.SMS.Haozhuma.H5)
	}
}

// newFakeH5Server2 同 newFakeH5Server 但返回 URL（paste 测试里第二次接管
// BaseURL 用；Go 测试不能重复 Cleanup 同一变量，这里单独建）。
func newFakeH5Server2(t *testing.T) string {
	srv := newFakeH5Server(t, "")
	return srv.URL
}

// TestHaozhumaProjectsEndpoint 项目搜索端点：未启用 503；启用后返回
// 解析后的项目列表。
func TestHaozhumaProjectsEndpoint(t *testing.T) {
	srv := newFakeH5Server(t, "sess-1")
	ah, _, _ := newWebuiTestHandler(t, `{}`)

	// 未启用：503。
	req := httptest.NewRequest(http.MethodGet, "/admin/account/sms/haozhuma/projects?q=腾讯", nil)
	rec := httptest.NewRecorder()
	ah.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Fatalf("no session: want 503, got %d", rec.Code)
	}

	// 启用（直接注入 client，模拟 main 启动链）。
	u, _ := url.Parse(srv.URL)
	_ = u
	ah.Handler.SetHaozhumaH5(h5.New("sess-1"))
	defer ah.Handler.cfg.HaozhumaH5.Close()

	// 缺 q：400。
	req = httptest.NewRequest(http.MethodGet, "/admin/account/sms/haozhuma/projects", nil)
	rec = httptest.NewRecorder()
	ah.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("missing q: want 400, got %d", rec.Code)
	}

	// 正常搜索。
	req = httptest.NewRequest(http.MethodGet, "/admin/account/sms/haozhuma/projects?q=腾讯科技", nil)
	rec = httptest.NewRecorder()
	ah.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("projects: %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Projects []struct {
			SID       string `json:"sid"`
			ProjectID string `json:"project_id"`
			Name      string `json:"name"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Projects) != 2 {
		t.Fatalf("projects = %d, want 2", len(resp.Projects))
	}
	if resp.Projects[0].SID != "70da814e38b466b6" || resp.Projects[0].ProjectID != "52283" {
		t.Fatalf("first project = %+v", resp.Projects[0])
	}
}

// TestHaozhumaUIDsEndpoint 对接码列表端点：启用后返回解析+排序后的
// 列表；会话失效路径（直接构造无会话 client）返回 410。
func TestHaozhumaUIDsEndpoint(t *testing.T) {
	newFakeH5Server(t, "sess-1")
	ah, _, _ := newWebuiTestHandler(t, `{}`)
	ah.Handler.SetHaozhumaH5(h5.New("sess-1"))
	defer ah.Handler.cfg.HaozhumaH5.Close()

	// 缺 sid：400。
	req := httptest.NewRequest(http.MethodGet, "/admin/account/sms/haozhuma/uids", nil)
	rec := httptest.NewRecorder()
	ah.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("missing sid: want 400, got %d", rec.Code)
	}

	// 正常列表。
	req = httptest.NewRequest(http.MethodGet, "/admin/account/sms/haozhuma/uids?sid=70da814e38b466b6", nil)
	rec = httptest.NewRecorder()
	ah.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("uids: %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		UIDs []struct {
			UID    string   `json:"uid"`
			Price  float64  `json:"price"`
			Stock  int      `json:"stock"`
			ISPs   []string `json:"isps"`
			Pinned bool     `json:"pinned"`
		} `json:"uids"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.UIDs) != 1 {
		t.Fatalf("uids = %d, want 1", len(resp.UIDs))
	}
	u0 := resp.UIDs[0]
	if u0.UID != "52283-WW9L2J4WOL" || u0.Price != 16.5 || u0.Stock != 23 || !u0.Pinned {
		t.Fatalf("uid item = %+v", u0)
	}
	if len(u0.ISPs) != 3 {
		t.Fatalf("isps = %v", u0.ISPs)
	}

	// 会话失效：client 换成 fake 不认的会话值 → 上游报"请选择正确的API"
	// → 410（前端据此弹"重新粘贴"并退化手填）。
	ah.Handler.cfg.HaozhumaH5.Session("expired-session")
	req = httptest.NewRequest(http.MethodGet, "/admin/account/sms/haozhuma/uids?sid=70da814e38b466b6", nil)
	rec = httptest.NewRecorder()
	ah.ServeHTTP(rec, req)
	if rec.Code != 410 {
		t.Fatalf("expired session: want 410, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHaozhumaH5NoSessionLeak 粘贴的 PHPSESSID 绝不能出现在任何响应体
// （包括错误响应）里。
func TestHaozhumaH5NoSessionLeak(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	du, _ := url.Parse(dead.URL)
	dead.Close()
	old := h5.BaseURL
	h5.BaseURL = du.String()
	t.Cleanup(func() { h5.BaseURL = old })

	ah, _, _ := newWebuiTestHandler(t, `{}`)
	req := httptest.NewRequest(http.MethodPost, "/admin/account/sms/haozhuma/h5-session",
		strings.NewReader(`{"session":"SECRET-PHPSESSID-XYZ"}`))
	rec := httptest.NewRecorder()
	ah.ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Fatal("verify against dead server should fail")
	}
	if strings.Contains(rec.Body.String(), "SECRET-PHPSESSID-XYZ") {
		t.Fatalf("session leaked: %s", rec.Body.String())
	}
	// 落盘也不该发生（验证失败）。
	if ah.Handler.cfg.HaozhumaH5 != nil {
		t.Fatal("failed paste must not create runtime client")
	}
}
