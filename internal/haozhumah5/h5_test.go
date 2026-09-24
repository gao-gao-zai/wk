package haozhumah5

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newFakeH5 起 fake H5 服务器并接管 BaseURL。
// 响应由测试用变量定制；未定制的路径返回 code:-1（模拟会话失效文案）。
func newFakeH5(t *testing.T) *fakeH5 {
	t.Helper()
	f := &fakeH5{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.cookie != "" {
			got, err := r.Cookie("PHPSESSID")
			if err != nil || got.Value != f.cookie {
				// 未带有效会话：豪猪实测返回 200 + code:-1 "请选择正确的API"
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"code":-1,"data":null,"msg":"请选择正确的API"}`)
				return
			}
		}
		switch r.URL.Path {
		case "/api.php":
			q := r.URL.Query()
			switch q.Get("type") {
			case "30":
				fmt.Fprint(w, f.type30)
			case "8":
				fmt.Fprint(w, f.type8)
			case "4":
				// type=4 加入对接码。重复加入实测返回
				// code:-1 msg="已添加过了..."。
				if f.added == nil {
					f.added = map[string]bool{}
				}
				djm := q.Get("djm")
				if djm == "" {
					fmt.Fprint(w, `{"code":-1,"data":null,"msg":"缺少对接码"}`)
					return
				}
				if f.added[djm] {
					fmt.Fprintf(w, `{"code":"-1","data":null,"msg":"已添加过了,如果找不到可以通过底部搜索查询"}`)
					return
				}
				f.added[djm] = true
				fmt.Fprint(w, `{"code":1,"data":null,"msg":"添加成功"}`)
			case "3":
				// type=3 我的对接码。成功返回 code=200（与 30/8 的 1 不同）。
				if len(f.added) == 0 {
					fmt.Fprint(w, `{"code":200,"data":[],"msg":"成功拉取列表"}`)
					return
				}
				var items []string
				for djm := range f.added {
					items = append(items, fmt.Sprintf(
						`{"mc":"[52283]腾讯科技[限对接]","uid":"%s","yhj":"16.500","zxky":"可用数量:23","yyy":"移动|","sheng":"","haoduan":"未知号段","time":"2026-09-22 23:24:18"}`,
						djm))
				}
				fmt.Fprintf(w, `{"code":200,"data":[%s],"msg":"成功拉取列表"}`, strings.Join(items, ","))
			default:
				fmt.Fprint(w, `{"code":-1,"data":null,"msg":"unknown type"}`)
			}
		case "/time.php":
			fmt.Fprint(w, time.Now().Format(time.RFC3339))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	old := BaseURL
	u, _ := url.Parse(f.srv.URL)
	BaseURL = u.String()
	t.Cleanup(func() { BaseURL = old })
	return f
}

type fakeH5 struct {
	srv    *httptest.Server
	cookie string
	type30 string
	type8  string
	added  map[string]bool
}

const sampleType30 = `{"code":1,"data":[
  {"name":"【52283】腾讯科技[限对接] [70da814e38b466b6]","sid":"70da814e38b466b6"},
  {"name":"【61904】腾讯科技3次[限对接] [1972972d69bd0453]","sid":"1972972d69bd0453"},
  {"name":"无前缀项目 [abcdef0123456789]","sid":"abcdef0123456789"}
],"msg":"Success"}`

const sampleType8 = `{"code":1,"data":[
  {"sid":"52283","mc":"[52283]腾讯科技[限对接]","uid":"52283-WW9L2J4WOL","yhj":"16.500","zxky":"可用数量:23","yyy":"移动|电信|联通|","sheng":"","haoduan":"未知号段","hd":"198|193|191|","time":"2026-09-22 23:24:18","zd":"1"},
  {"sid":"52283","mc":"[52283]腾讯科技[限对接]","uid":"52283-AAAAAAAAAA","yhj":"2.200","zxky":"可用数量:0","yyy":"移动|","sheng":"江苏|贵州","haoduan":"虚拟号段","hd":null,"time":"2026-09-22 23:24:18","zd":"0"},
  {"sid":"52283","mc":"","uid":"","yhj":"0","zxky":"","yyy":"","hd":null,"zd":null}
],"msg":"Success"}`

func TestProjectsParse(t *testing.T) {
	f := newFakeH5(t)
	f.cookie = "sess-1"
	f.type30 = sampleType30
	c := New("sess-1")
	defer c.Close()

	got, err := c.Projects(context.Background(), "腾讯科技")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("projects = %d, want 3", len(got))
	}
	if got[0].SID != "70da814e38b466b6" || got[0].ProjectID != "52283" {
		t.Fatalf("first = %+v, want sid 70da.../id 52283", got[0])
	}
	// 无【ID】前缀的项目 project_id 为空但其余字段可用。
	if got[2].ProjectID != "" || got[2].SID != "abcdef0123456789" {
		t.Fatalf("no-prefix project = %+v", got[2])
	}
}

func TestUIDsParseAndSort(t *testing.T) {
	f := newFakeH5(t)
	f.cookie = "sess-1"
	f.type8 = sampleType8
	c := New("sess-1")
	defer c.Close()

	got, err := c.UIDs(context.Background(), "70da814e38b466b6")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 { // 空 uid 的脏数据行被剔除
		t.Fatalf("uids = %d, want 2: %+v", len(got), got)
	}
	first := got[0]
	// 置顶（zd=1）优先，其次库存降序。
	if first.UID != "52283-WW9L2J4WOL" {
		t.Fatalf("first uid = %s, want pinned 52283-WW9L2J4WOL", first.UID)
	}
	if first.Price != 16.5 || first.Stock != 23 {
		t.Fatalf("price/stock = %v/%d, want 16.5/23", first.Price, first.Stock)
	}
	if len(first.ISPs) != 3 || first.ISPs[0] != "移动" {
		t.Fatalf("isps = %v", first.ISPs)
	}
	if len(first.Segments) != 3 || first.Segments[0] != "198" {
		t.Fatalf("segments = %v", first.Segments)
	}
	if !first.Pinned {
		t.Fatal("first should be pinned")
	}
	second := got[1]
	if second.Stock != 0 || len(second.Provinces) != 2 || second.SegmentType != "虚拟号段" {
		t.Fatalf("second = %+v", second)
	}
	if second.Segments != nil {
		t.Fatalf("hd=null should give nil segments, got %v", second.Segments)
	}
}

func TestSessionExpired(t *testing.T) {
	f := newFakeH5(t)
	f.cookie = "other-session" // 服务器认这个，客户端带别的
	f.type30 = sampleType30
	c := New("sess-1")
	defer c.Close()

	_, err := c.Projects(context.Background(), "腾讯")
	if err == nil || !strings.Contains(err.Error(), "会话已失效") {
		t.Fatalf("want ErrSessionExpired, got %v", err)
	}
	// 状态回显：has=true, expired=true；session 值不被清空。
	info := c.Info()
	if !info.Has || !info.Expired {
		t.Fatalf("info = %+v, want has+expired", info)
	}
	// 重贴新会话后恢复。
	c.Session("other-session")
	info = c.Info()
	if info.Expired {
		t.Fatalf("after re-paste, expired should reset: %+v", info)
	}
	if _, err := c.Projects(context.Background(), "腾讯"); err != nil {
		t.Fatalf("after re-paste should work: %v", err)
	}
}

func TestNoSession(t *testing.T) {
	newFakeH5(t)
	c := New("")
	defer c.Close()
	if _, err := c.Projects(context.Background(), "x"); err != ErrNoSession {
		t.Fatalf("want ErrNoSession, got %v", err)
	}
	if err := c.Ping(context.Background()); err != ErrNoSession {
		t.Fatalf("ping: want ErrNoSession, got %v", err)
	}
}

func TestCache(t *testing.T) {
	f := newFakeH5(t)
	f.cookie = "sess-1"
	f.type30 = sampleType30
	c := New("sess-1")
	defer c.Close()

	if _, err := c.Projects(context.Background(), "腾讯"); err != nil {
		t.Fatal(err)
	}
	// 上游改响应，5 分钟内仍回缓存。
	f.type30 = `{"code":1,"data":[],"msg":"Success"}`
	got, err := c.Projects(context.Background(), "腾讯")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("cached result should be 3, got %d", len(got))
	}
	// 过期后重新打上游。
	c.cacheMu.Lock()
	for k := range c.cache {
		e := c.cache[k]
		e.at = time.Now().Add(-cacheTTL - time.Minute)
		c.cache[k] = e
	}
	c.cacheMu.Unlock()
	got, err = c.Projects(context.Background(), "腾讯")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expired cache should re-fetch, got %d", len(got))
	}
	// 换会话清缓存。
	c.Session("sess-1")
	f.type30 = sampleType30
	if _, err := c.Projects(context.Background(), "腾讯"); err != nil {
		t.Fatal(err)
	}
	f.type30 = `{"code":1,"data":[],"msg":"Success"}`
	c.Session("sess-1") // 同值也清（保守：换账号场景必须清）
	got, err = c.Projects(context.Background(), "腾讯")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("session change should clear cache, got %d", len(got))
	}
}

func TestPing(t *testing.T) {
	f := newFakeH5(t)
	f.cookie = "sess-1"
	c := New("sess-1")
	defer c.Close()
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.Info().LastSeen.IsZero() {
		t.Fatal("lastSeen should update after ping")
	}
}

func TestParseHelpers(t *testing.T) {
	if id := parseProjectID("【52283】腾讯科技[限对接] [70da]"); id != "52283" {
		t.Fatalf("parseProjectID = %q", id)
	}
	if id := parseProjectID("【5228a】混合"); id != "" {
		t.Fatalf("non-digit id should be empty, got %q", id)
	}
	if id := parseProjectID("无前缀"); id != "" {
		t.Fatalf("no prefix should be empty, got %q", id)
	}
	if n := parseStock("可用数量:23"); n != 23 {
		t.Fatalf("parseStock = %d", n)
	}
	if n := parseStock(""); n != -1 {
		t.Fatalf("empty stock = %d, want -1", n)
	}
	if segs := splitAny("198|193|"); len(segs) != 2 {
		t.Fatalf("splitAny(string) = %v", segs)
	}
	if segs := splitAny([]any{"170", 1.0, " "}); len(segs) != 1 {
		t.Fatalf("splitAny(slice) = %v", segs)
	}
	if segs := splitAny(nil); segs != nil {
		t.Fatalf("splitAny(nil) = %v", segs)
	}
}

// TestRedactURL 网络错误不得包含完整 URL（含 query 关键词）。
func TestRedactURL(t *testing.T) {
	f := newFakeH5(t)
	f.cookie = "sess-1"
	c := New("sess-1")
	defer c.Close()
	f.srv.Close() // 端口下线
	_, err := c.Projects(context.Background(), "腾讯SECRETKW")
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "腾讯SECRETKW") {
		t.Fatalf("keyword leaked in error: %v", err)
	}
	if strings.Contains(err.Error(), "type=30") {
		t.Fatalf("query leaked in error: %v", err)
	}
}

// TestAddUIDAndMyUIDs 加入对接码（type=4）+ 我的列表（type=3, code=200）：
// 加入成功 → 我的列表出现该码；重复加入（上游 code:-1 "已添加过了"）
// 必须当成功吸收，不能报错。
func TestAddUIDAndMyUIDs(t *testing.T) {
	f := newFakeH5(t)
	f.cookie = "sess-1"
	c := New("sess-1")
	defer c.Close()

	// 初始为空
	mine, err := c.MyUIDs(context.Background(), "")
	if err != nil {
		t.Fatalf("MyUIDs empty: %v", err)
	}
	if len(mine) != 0 {
		t.Fatalf("want 0, got %d", len(mine))
	}

	// 加入
	if err := c.AddUID(context.Background(), "52283-WW9L2J4WOL"); err != nil {
		t.Fatalf("AddUID: %v", err)
	}
	// 加入后缓存作废，重拉应出现
	mine, err = c.MyUIDs(context.Background(), "")
	if err != nil {
		t.Fatalf("MyUIDs after add: %v", err)
	}
	if len(mine) != 1 || mine[0].UID != "52283-WW9L2J4WOL" {
		t.Fatalf("want [52283-WW9L2J4WOL], got %+v", mine)
	}
	if mine[0].Price != 16.5 || mine[0].Stock != 23 {
		t.Fatalf("parse fields wrong: %+v", mine[0])
	}

	// 重复加入：上游 code:-1 + "已添加过了"——AddUID 必须当成功
	if err := c.AddUID(context.Background(), "52283-WW9L2J4WOL"); err != nil {
		t.Fatalf("duplicate AddUID must be absorbed as success, got: %v", err)
	}

	// 空码拒绝
	if err := c.AddUID(context.Background(), "  "); err == nil {
		t.Fatal("empty uid must error")
	}
}

// TestAddUIDSessionExpired 会话失效时加入必须返回 ErrSessionExpired。
func TestAddUIDSessionExpired(t *testing.T) {
	f := newFakeH5(t)
	f.cookie = "sess-1"
	c := New("sess-1")
	defer c.Close()
	c.Session("wrong-session") // 会话换掉但 cookie 校验要求 sess-1
	err := c.AddUID(context.Background(), "52283-WW9L2J4WOL")
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("want ErrSessionExpired, got %v", err)
	}
}
