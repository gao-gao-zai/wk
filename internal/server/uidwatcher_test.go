package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	h5 "workbuddy2api/internal/haozhumah5"
)

// fakeWatchH5 可编程的 fake H5：type=8 返回 uidTable 里的当前值，
// type=4 记录加码，type=41 记录移码，/time.php 恒通。consume() 模拟
// 真实市场——号码被取走后可取库存下降（真实 H5 的 zxky 就是这么动的）。
type fakeWatchH5 struct {
	t        *testing.T
	srv      *httptest.Server
	hexSID   string
	mu       chanMut
	uidTable map[string]string // uid -> "price|stock"
	added    atomic.Int32
	removed  atomic.Int32
}

// chanMut 用 mutex 字段（embedded 值类型，不拷贝使用）。
type chanMut struct{ ch chan struct{} }

func (m *chanMut) lock() {
	if m.ch == nil {
		m.ch = make(chan struct{}, 1)
		m.ch <- struct{}{}
	}
	<-m.ch
}
func (m *chanMut) unlock() { m.ch <- struct{}{} }

// consume 市场侧扣库存：watcher 触发加号消耗的号要从市场库存扣掉，
// 否则"补货判定基线 = 观察 - 自身消耗"在 fake 上永远不成立。
func (f *fakeWatchH5) consume(uid string, n int) {
	f.mu.lock()
	if meta, ok := f.uidTable[uid]; ok {
		cur, _ := strconv.Atoi(stockOf(meta))
		next := cur - n
		if next < 0 {
			next = 0
		}
		f.uidTable[uid] = price(meta) + "|" + strconv.Itoa(next)
	}
	f.mu.unlock()
}

func newFakeWatchH5(t *testing.T, hexSID string, table map[string]string) *fakeWatchH5 {
	f := &fakeWatchH5{t: t, hexSID: hexSID, uidTable: table}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api.php":
			switch r.URL.Query().Get("type") {
			case "8":
				if r.URL.Query().Get("sid") != f.hexSID {
					w.Write([]byte(`{"code":-1,"data":null,"msg":"sid 不匹配"}`))
					return
				}
				f.mu.lock()
				rows := make([]string, 0, len(f.uidTable))
				for uid, meta := range f.uidTable {
					rows = append(rows, fmt.Sprintf(
						`{"sid":"52283","mc":"腾讯科技","uid":%q,"yhj":%q,"zxky":"可用数量:%s","yyy":"移动|","sheng":"","haoduan":"未知号段","hd":"198|","time":"2026-09-24 00:00:00","zd":"0"}`,
						uid, price(meta), stockOf(meta)))
				}
				f.mu.unlock()
				w.Write([]byte(`{"code":1,"data":[` + strings.Join(rows, ",") + `],"msg":"Success"}`))
			case "4":
				f.added.Add(1)
				w.Write([]byte(`{"code":1,"data":null,"msg":"添加成功"}`))
			case "41":
				f.removed.Add(int32(len(strings.Split(r.URL.Query().Get("uid"), ","))))
				w.Write([]byte(`{"code":200,"data":null,"msg":"对接码状态:已删除"}`))
			default:
				w.Write([]byte(`{"code":-1,"data":null,"msg":"unknown type"}`))
			}
		case "/time.php":
			w.Write([]byte(time.Now().Format(time.RFC3339)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// price/stockOf：meta 格式 "price|stock"，如 "0.88|23"。
func price(meta string) string { return strings.SplitN(meta, "|", 2)[0] }
func stockOf(meta string) string {
	parts := strings.SplitN(meta, "|", 2)
	if len(parts) < 2 {
		return "0"
	}
	return parts[1]
}

func (f *fakeWatchH5) client() *h5.Client {
	// 与 newFakeH5Server 同一模式：包级 BaseURL 指向 fake（t.Cleanup 恢复）。
	old := h5.BaseURL
	u, _ := url.Parse(f.srv.URL)
	h5.BaseURL = u.String()
	f.t.Cleanup(func() { h5.BaseURL = old })
	return h5.New("sess-1")
}

// watchTestEnv 一次搭齐：fake H5 + fake 豪猪（取号失败即可，我们只测
// watcher 的判定/记账）+ AutoEnroller + UIDWatcher。
type watchTestEnv struct {
	fh   *fakeWatchH5
	fz   *fakeHZM
	en   *AutoEnroller
	w    *UIDWatcher
	tmp  string
}

func newWatchEnv(t *testing.T, table map[string]string) *watchTestEnv {
	t.Helper()
	fh := newFakeWatchH5(t, "a1b2c3d4e5f60718", table)
	fz := newFakeHZM(t)
	// getPhone 直接成功一个号；getMessage 立即返回短信（sms 字段带码）。
	fz.msgResp.Store(`{"code":"0","msg":"ok","sms":"【腾讯科技】您的验证码是 123456，5 分钟内有效"}`)
	en := NewAutoEnroller(successSMSManager(t), fz.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil, nil)
	en.SetRetryDelay(50 * time.Millisecond)
	// 收码轮询提速：默认 5s×18 次在测试里等不起。
	en.pollInterval = 20 * time.Millisecond
	en.pollCount = 3
	tmp := t.TempDir()
	w := NewUIDWatcher(WatchConfig{}, fh.client(), en, filepath.Join(tmp, "watcher-state.json"))
	return &watchTestEnv{fh: fh, fz: fz, en: en, w: w, tmp: tmp}
}

func (e *watchTestEnv) proj(maxPrice float64, minStock int) WatchProject {
	return WatchProject{
		Sid: "52283", HexSID: "a1b2c3d4e5f60718", Name: "腾讯科技",
		MaxPrice: maxPrice, MinStock: minStock, Enabled: true,
	}
}

// waitFor 轮询断言（条件或超时）。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("超时（%s）：%s", timeout, msg)
}

// TestWatcherNewUIDTriggersAndCharges 新码出现 → 触发加号 → 记账扣额度。
// 成功 1 个号（fake 链路全通）→ 额度 10 - 0.88 = 9.12。
func TestWatcherNewUIDTriggersAndCharges(t *testing.T) {
	e := newWatchEnv(t, map[string]string{"52283-CHEAP": "0.88|23"})
	e.w.Reconfigure(WatchConfig{Enabled: true, IntervalSeconds: 60, Projects: []WatchProject{e.proj(1.0, 5)}})
	if _, err := e.w.AddBudget(10, nil); err != nil {
		t.Fatal(err)
	}
	go e.w.Run(context.Background())

	waitFor(t, 10*time.Second, func() bool { return e.fz.getPhone.Load() > 0 }, "取号应被触发")
	// 等记账完成（任务结束 + charge）。
	waitFor(t, 10*time.Second, func() bool {
		st := e.w.Status()
		return st.EnrolledTotal >= 1
	}, "成功数与记账")

	st := e.w.Status()
	if st.BudgetRemaining != 10-0.88 {
		t.Fatalf("额度 = %.2f，want 9.12", st.BudgetRemaining)
	}
	if st.SpentTotal != 0.88 {
		t.Fatalf("花费 = %.2f，want 0.88", st.SpentTotal)
	}
	if st.TriggerTotal != 1 {
		t.Fatalf("触发次数 = %d，want 1", st.TriggerTotal)
	}
}

// TestWatcherNoSelfTrigger 自身消耗不算补货：触发消耗的号在市场侧扣
// 库存（真实 H5 行为），下轮观察 = 基线 → 不再触发。
func TestWatcherNoSelfTrigger(t *testing.T) {
	e := newWatchEnv(t, map[string]string{"52283-CHEAP": "0.88|23"})
	e.w.Reconfigure(WatchConfig{Enabled: true, IntervalSeconds: 60, Projects: []WatchProject{e.proj(1.0, 5)}})
	if _, err := e.w.AddBudget(10, nil); err != nil {
		t.Fatal(err)
	}
	go e.w.Run(context.Background())

	// 第一轮触发后记账。
	waitFor(t, 10*time.Second, func() bool { return e.w.Status().TriggerTotal >= 1 }, "第一轮触发")
	waitFor(t, 10*time.Second, func() bool { return e.w.Status().EnrolledTotal >= 1 }, "第一轮成功")
	// 市场侧如实扣减（真实 H5：号被取走，可取库存 -1）。
	e.fh.consume("52283-CHEAP", 1)
	// 手动跑几轮（不等 ticker）：观察 = 基线，不应再触发。
	for i := 0; i < 3; i++ {
		e.w.tick(context.Background(), e.w.cfg)
	}
	if st := e.w.Status(); st.TriggerTotal != 1 {
		t.Fatalf("自身消耗/无变化不应再触发，实际触发 %d 次", st.TriggerTotal)
	}
}

// TestWatcherRestockTriggers 真补货（上游 stock 回升）→ 再次触发。
func TestWatcherRestockTriggers(t *testing.T) {
	e := newWatchEnv(t, map[string]string{"52283-CHEAP": "0.88|23"})
	e.w.Reconfigure(WatchConfig{Enabled: true, IntervalSeconds: 60, Projects: []WatchProject{e.proj(1.0, 5)}})
	if _, err := e.w.AddBudget(10, nil); err != nil {
		t.Fatal(err)
	}
	go e.w.Run(context.Background())

	waitFor(t, 10*time.Second, func() bool { return e.w.Status().TriggerTotal >= 1 }, "第一轮触发")
	waitFor(t, 10*time.Second, func() bool { return e.w.Status().EnrolledTotal >= 1 }, "第一轮成功")

	// 自身消耗先扣（23→22），上游再补货到 30（真增量 30 > 基线 22）。
	e.fh.consume("52283-CHEAP", 1)
	e.fh.mu.lock()
	e.fh.uidTable["52283-CHEAP"] = "0.88|30"
	e.fh.mu.unlock()
	e.w.tick(context.Background(), e.w.cfg)
	if st := e.w.Status(); st.TriggerTotal != 2 {
		t.Fatalf("真补货应触发第二次，实际 %d 次", st.TriggerTotal)
	}
}

// TestWatcherPriceDropIntoRange 降价进入区间：1.65（>1.0 被过滤）→
// 0.88（≤1.0）→ 触发。这是用户核心场景"出现比设定价格更低的对接码"。
func TestWatcherPriceDropIntoRange(t *testing.T) {
	e := newWatchEnv(t, map[string]string{"52283-PRICEY": "1.65|23"})
	e.w.Reconfigure(WatchConfig{Enabled: true, IntervalSeconds: 60, Projects: []WatchProject{e.proj(1.0, 5)}})
	if _, err := e.w.AddBudget(10, nil); err != nil {
		t.Fatal(err)
	}
	// 第一轮：1.65 超价，只建立基线不触发。
	e.w.tick(context.Background(), e.w.cfg)
	if st := e.w.Status(); st.TriggerTotal != 0 {
		t.Fatalf("超价码不应触发，实际 %d 次", st.TriggerTotal)
	}
	// 降价到 0.88 → 触发。
	e.fh.mu.lock()
	e.fh.uidTable["52283-PRICEY"] = "0.88|23"
	e.fh.mu.unlock()
	e.w.tick(context.Background(), e.w.cfg)
	if st := e.w.Status(); st.TriggerTotal != 1 {
		t.Fatalf("降价进入区间应触发，实际 %d 次", st.TriggerTotal)
	}
}

// TestWatcherBudgetExhausted 额度耗尽：不够一次触发 → 不派活 + 暂停原因；
// 充值后恢复。
func TestWatcherBudgetExhausted(t *testing.T) {
	e := newWatchEnv(t, map[string]string{"52283-CHEAP": "0.88|23"})
	e.w.Reconfigure(WatchConfig{Enabled: true, IntervalSeconds: 60, Projects: []WatchProject{e.proj(1.0, 5)}})
	// 额度只给 0.5（不够 0.88）。
	if _, err := e.w.AddBudget(0.5, nil); err != nil {
		t.Fatal(err)
	}
	e.w.tick(context.Background(), e.w.cfg)
	st := e.w.Status()
	if st.TriggerTotal != 0 {
		t.Fatal("额度不足不应派活")
	}
	if st.PausedReason == "" || !strings.Contains(st.PausedReason, "额度耗尽") {
		t.Fatalf("暂停原因 = %q，want 额度耗尽", st.PausedReason)
	}
	if e.fz.getPhone.Load() > 0 {
		t.Fatal("额度不足不应取号")
	}
	// 充值 → 暂停清除 → 下一轮触发。
	if _, err := e.w.AddBudget(10, nil); err != nil {
		t.Fatal(err)
	}
	if st := e.w.Status(); st.PausedReason != "" {
		t.Fatalf("充值后暂停应清除，仍有 %q", st.PausedReason)
	}
	e.w.tick(context.Background(), e.w.cfg)
	if st := e.w.Status(); st.TriggerTotal != 1 {
		t.Fatal("充值后应恢复触发")
	}
}

// TestWatcherSkipsWhenManualRunning 手动任务在跑 → 跳过本轮（不排队）。
// 顺序注意：先占住 AutoEnroller 再 Reconfigure（开启会立即跑第一轮，
// 反过来会把手动任务挡在"已在进行中"上）。
func TestWatcherSkipsWhenManualRunning(t *testing.T) {
	e := newWatchEnv(t, map[string]string{"52283-CHEAP": "0.88|23"})
	if _, err := e.w.AddBudget(10, nil); err != nil {
		t.Fatal(err)
	}
	// 占住 AutoEnroller：一个长时间手动任务。
	e.fz.phoneResp.Store(`{"code":"-1","msg":"没有取到号码，请重新尝试"}`)
	e.en.SetRetryDelay(200 * time.Millisecond)
	if err := e.en.AutoRun(1, 1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return e.en.Status().Running }, "手动任务应已启动")
	// 手动任务运行中开启监控：立即跑的第一轮必须让路（不排队）。
	e.w.Reconfigure(WatchConfig{Enabled: true, IntervalSeconds: 60, Projects: []WatchProject{e.proj(1.0, 5)}})
	waitFor(t, 5*time.Second, func() bool { return e.w.Status().LastTick != "" }, "值班轮应已跑过一次")
	if st := e.w.Status(); st.TriggerTotal != 0 {
		t.Fatal("手动任务运行中不应触发值班轮")
	}
	// 等手动任务自然结束（无号 → 熔断或上限停止）。
	waitFor(t, 30*time.Second, func() bool { return !e.en.Status().Running }, "手动任务应结束")
}

// TestWatcherRestoresFetchOptions 值班轮结束后恢复用户原取号参数。
func TestWatcherRestoresFetchOptions(t *testing.T) {
	e := newWatchEnv(t, map[string]string{"52283-CHEAP": "0.88|23"})
	// 用户的配置：sid=52283、author=my、单码+池、isp。
	e.en.UpdateFetchOptions("my", "52283-USER", []string{"52283-USER", "52283-USER2"}, "1,2")
	e.w.Reconfigure(WatchConfig{Enabled: true, IntervalSeconds: 60, Projects: []WatchProject{e.proj(1.0, 5)}})
	if _, err := e.w.AddBudget(10, nil); err != nil {
		t.Fatal(err)
	}
	e.w.tick(context.Background(), e.w.cfg)
	waitFor(t, 15*time.Second, func() bool {
		sid, _, _, _, _ := e.en.FetchSnapshot()
		return sid == "52283"
	}, "值班轮应执行过")
	// 等任务收尾恢复。
	waitFor(t, 15*time.Second, func() bool {
		_, author, uid, uids, isp := e.en.FetchSnapshot()
		return author == "my" && uid == "52283-USER" && len(uids) == 2 && isp == "1,2"
	}, "取号参数应恢复为用户配置")
}

// TestWatcherStatePersists 状态落盘 + 重建恢复（额度不漂移）。
// 等待条件用**文件内容**（charge 的状态入锁内、persist 在锁外，内存
// 状态先于文件可见——按内存等会撞上竞态窗口）。
func TestWatcherStatePersists(t *testing.T) {
	e := newWatchEnv(t, map[string]string{"52283-CHEAP": "0.88|23"})
	if _, err := e.w.AddBudget(10, nil); err != nil {
		t.Fatal(err)
	}
	e.w.Reconfigure(WatchConfig{Enabled: true, IntervalSeconds: 60, Projects: []WatchProject{e.proj(1.0, 5)}})
	go e.w.Run(context.Background())
	statePath := filepath.Join(e.tmp, "watcher-state.json")
	waitFor(t, 15*time.Second, func() bool {
		raw, err := os.ReadFile(statePath)
		if err != nil {
			return false
		}
		var doc struct {
			BudgetRemaining float64 `json:"budget_remaining"`
			EnrolledTotal   int     `json:"enrolled_total"`
		}
		return json.Unmarshal(raw, &doc) == nil && doc.BudgetRemaining == 9.12 && doc.EnrolledTotal == 1
	}, "记账落盘（budget=9.12 enrolled=1）")

	// 新 watcher 从同一路径加载（模拟重启）。
	e.fh.mu.lock()
	e.fh.uidTable = map[string]string{} // 清空列表：不触发
	e.fh.mu.unlock()
	w2 := NewUIDWatcher(WatchConfig{}, e.fh.client(), e.en, statePath)
	st := w2.Status()
	if st.BudgetRemaining != 9.12 {
		t.Fatalf("重启后额度 = %.2f，want 9.12", st.BudgetRemaining)
	}
	if st.EnrolledTotal != 1 || st.SpentTotal != 0.88 {
		t.Fatalf("重启后统计错位：enrolled=%d spent=%.2f", st.EnrolledTotal, st.SpentTotal)
	}
}

// TestWatchConfigValidate 配置校验。
func TestWatchConfigValidate(t *testing.T) {
	bad := []WatchConfig{
		{Enabled: true},                                   // 无项目
		{Projects: []WatchProject{{Sid: "", HexSID: "a1b2c3d4e5f60718", MaxPrice: 1, MinStock: 1}}},          // 空 sid
		{Projects: []WatchProject{{Sid: "52283", HexSID: "zz", MaxPrice: 1, MinStock: 1}}},                   // 坏 hex
		{Projects: []WatchProject{{Sid: "52283", HexSID: "a1b2c3d4e5f60718", MaxPrice: 0, MinStock: 1}}},    // 坏价
		{Projects: []WatchProject{{Sid: "52283", HexSID: "a1b2c3d4e5f60718", MaxPrice: 1, MinStock: 0}}},    // 坏库存
		{Projects: []WatchProject{
			{Sid: "52283", HexSID: "a1b2c3d4e5f60718", MaxPrice: 1, MinStock: 1},
			{Sid: "52283", HexSID: "a1b2c3d4e5f60718", MaxPrice: 1, MinStock: 1},
		}}, // 重复项目
	}
	for i, c := range bad {
		if _, err := c.Validate(); err == nil {
			t.Errorf("bad[%d] 应报错", i)
		}
	}
	good := WatchConfig{Projects: []WatchProject{{Sid: "52283", HexSID: "a1b2c3d4e5f60718", MaxPrice: 1.5, MinStock: 2}}}
	clean, err := good.Validate()
	if err != nil {
		t.Fatal(err)
	}
	if clean.IntervalSeconds != 0 || len(clean.Projects) != 1 {
		t.Fatalf("clean = %+v", clean)
	}
	// 间隔校验：60 下限 / 3600 上限。
	c30 := WatchConfig{IntervalSeconds: 30, Projects: good.Projects}
	if _, err := c30.Validate(); err == nil {
		t.Fatal("间隔 30s 应报错")
	}
	c7200 := WatchConfig{IntervalSeconds: 7200, Projects: good.Projects}
	if _, err := c7200.Validate(); err == nil {
		t.Fatal("间隔 7200s 应报错")
	}
}

// TestWatcherHotEnable 启动时未启用 → Run 常驻空转 → Reconfigure 开启后
// 立即拉起第一轮（不需要重启进程）。这是 WebUI 开关的路径。
func TestWatcherHotEnable(t *testing.T) {
	e := newWatchEnv(t, map[string]string{"52283-CHEAP": "0.88|23"})
	e.w.Start() // 常驻循环（enabled=false：空转）
	if _, err := e.w.AddBudget(10, nil); err != nil {
		t.Fatal(err)
	}
	// WebUI 开关：Reconfigure 热开启。
	e.w.Reconfigure(WatchConfig{Enabled: true, IntervalSeconds: 60, Projects: []WatchProject{e.proj(1.0, 5)}})
	waitFor(t, 10*time.Second, func() bool { return e.fz.getPhone.Load() > 0 }, "热开启后应立即跑第一轮并触发")
	waitFor(t, 10*time.Second, func() bool { return e.w.Status().EnrolledTotal >= 1 }, "热开启后加号成功")
}

// TestWatcherHotDisable 开启 → 关闭：循环停摆（不再 tick），再开恢复。
func TestWatcherHotDisable(t *testing.T) {
	e := newWatchEnv(t, map[string]string{"52283-CHEAP": "0.88|23"})
	if _, err := e.w.AddBudget(10, nil); err != nil {
		t.Fatal(err)
	}
	e.w.Start()
	e.w.Reconfigure(WatchConfig{Enabled: true, IntervalSeconds: 60, Projects: []WatchProject{e.proj(1.0, 5)}})
	waitFor(t, 10*time.Second, func() bool { return e.w.Status().EnrolledTotal >= 1 }, "第一轮成功")
	// 关闭：之后手动 tick 也不该派活（enabled=false 直接跳过）。
	e.w.Reconfigure(WatchConfig{Enabled: false, IntervalSeconds: 60, Projects: []WatchProject{e.proj(1.0, 5)}})
	waitFor(t, 5*time.Second, func() bool {
		st := e.w.Status()
		for _, l := range st.Logs {
			if strings.Contains(l, "监控已停止") {
				return true
			}
		}
		return false
	}, "应记录监控停止")
	before := e.w.Status().TriggerTotal
	e.fh.consume("52283-CHEAP", 1)
	e.w.tick(context.Background(), e.w.cfg)
	if after := e.w.Status().TriggerTotal; after != before {
		t.Fatalf("关闭后不应再触发（%d → %d）", before, after)
	}
}

// TestWatcherUnavailable 无 H5 / 无 AutoEnroller 时 Run 不 panic，Status
// 报 available=false。
func TestWatcherUnavailable(t *testing.T) {
	w := NewUIDWatcher(WatchConfig{Enabled: true}, nil, nil, "")
	w.Run(context.Background()) // 不 panic 即可
	if st := w.Status(); st.Available {
		t.Fatal("缺依赖时应报不可用")
	}
}

// TestWatcherDeadUIDRevives dead 码（上游删除）重新出现且达标 → 触发
// "重新上架"，且不会连续触发。
func TestWatcherDeadUIDRevives(t *testing.T) {
	e := newWatchEnv(t, map[string]string{"52283-GONE": "0.88|23"})
	e.w.Reconfigure(WatchConfig{Enabled: true, IntervalSeconds: 60, Projects: []WatchProject{e.proj(1.0, 5)}})
	if _, err := e.w.AddBudget(10, nil); err != nil {
		t.Fatal(err)
	}
	// 第一轮：新码触发 + 成功记账（市场侧同步扣减）。
	e.w.tick(context.Background(), e.w.cfg)
	waitFor(t, 10*time.Second, func() bool { return e.w.Status().EnrolledTotal >= 1 }, "第一轮成功")
	e.fh.consume("52283-GONE", 1)

	// 上游删了它 → dead。
	e.fh.mu.lock()
	e.fh.uidTable = map[string]string{}
	e.fh.mu.unlock()
	e.w.tick(context.Background(), e.w.cfg)

	// 重新上架且达标 → "重新上架"触发（第二次）。
	e.fh.mu.lock()
	e.fh.uidTable = map[string]string{"52283-GONE": "0.88|25"}
	e.fh.mu.unlock()
	e.w.tick(context.Background(), e.w.cfg)
	if st := e.w.Status(); st.TriggerTotal != 2 {
		t.Fatalf("重新上架应触发第二次，实际 %d 次", st.TriggerTotal)
	}
	// 第二轮成功后同样扣市场库存。
	waitFor(t, 10*time.Second, func() bool { return e.w.Status().EnrolledTotal >= 2 }, "第二轮成功")
	e.fh.consume("52283-GONE", 1)
	// 再跑一轮：观察 = 基线，不再触发。
	e.w.tick(context.Background(), e.w.cfg)
	if st := e.w.Status(); st.TriggerTotal != 2 {
		t.Fatalf("基线重建后不应再触发，实际 %d 次", st.TriggerTotal)
	}
}

// TestWatcherMultipleCandidatesQueued 同一轮多个候选（新码 3 个）：
// 按价格升序逐个派活，都在本轮消化（≤ 单轮上限）；不吞事件——上限
// 之外的候选留到下轮，基线不同步。
func TestWatcherMultipleCandidatesQueued(t *testing.T) {
	e := newWatchEnv(t, map[string]string{
		"52283-A": "0.50|10",
		"52283-B": "0.30|10",
		"52283-C": "0.40|10",
	})
	e.w.Reconfigure(WatchConfig{Enabled: true, IntervalSeconds: 60, WantPerTrigger: 1, Projects: []WatchProject{e.proj(1.0, 5)}})
	if _, err := e.w.AddBudget(10, nil); err != nil {
		t.Fatal(err)
	}
	e.w.tick(context.Background(), e.w.cfg)
	// 单轮上限 3：三个候选全部按价派活（0.30 → 0.40 → 0.50）。
	waitFor(t, 15*time.Second, func() bool { return e.w.Status().EnrolledTotal >= 3 }, "三个候选都应加号成功")
	st := e.w.Status()
	if st.TriggerTotal != 3 {
		t.Fatalf("应触发 3 次，实际 %d", st.TriggerTotal)
	}
	if st.BudgetRemaining != round2(10-0.3-0.4-0.5) {
		t.Fatalf("额度 = %.2f，want 8.80", st.BudgetRemaining)
	}
	// 每次成功都扣市场库存（fake 侧如实模拟）——这里补扣 3 个。
	e.fh.consume("52283-A", 1)
	e.fh.consume("52283-B", 1)
	e.fh.consume("52283-C", 1)
	// 下轮：观察 = 基线，不再触发。
	e.w.tick(context.Background(), e.w.cfg)
	if st := e.w.Status(); st.TriggerTotal != 3 {
		t.Fatalf("全部消化后不应再触发，实际 %d", st.TriggerTotal)
	}
}

// TestWatcherCandidateOverflowKeepsEvent 候选超过单轮上限：本轮只消化
// 3 个（最便宜的），其余**不吞**——下轮继续触发。
func TestWatcherCandidateOverflowKeepsEvent(t *testing.T) {
	e := newWatchEnv(t, map[string]string{
		"52283-A": "0.10|10",
		"52283-B": "0.20|10",
		"52283-C": "0.30|10",
		"52283-D": "0.40|10",
		"52283-E": "0.50|10",
	})
	e.w.Reconfigure(WatchConfig{Enabled: true, IntervalSeconds: 60, WantPerTrigger: 1, Projects: []WatchProject{e.proj(1.0, 5)}})
	if _, err := e.w.AddBudget(10, nil); err != nil {
		t.Fatal(err)
	}
	e.w.tick(context.Background(), e.w.cfg)
	waitFor(t, 15*time.Second, func() bool { return e.w.Status().EnrolledTotal >= 3 }, "单轮上限 3 个应先消化")
	// 市场扣库存（前 3 个：A B C）。
	e.fh.consume("52283-A", 1)
	e.fh.consume("52283-B", 1)
	e.fh.consume("52283-C", 1)
	// 第 4 轮：D、E 的事件还在（基线没同步），继续按价派活。
	e.w.tick(context.Background(), e.w.cfg)
	waitFor(t, 15*time.Second, func() bool { return e.w.Status().EnrolledTotal >= 5 }, "剩余候选下轮应继续消化")
	st := e.w.Status()
	if st.TriggerTotal != 5 {
		t.Fatalf("两轮应共触发 5 次，实际 %d", st.TriggerTotal)
	}
}

// TestWatcherBadUIDCooldownDiffuse 给出号收不到码（sms 永远空）→
// 本轮 0 成功 → 不扣额度 + 差码冷却（下轮同码不再触发）+ 从账户移除。
func TestWatcherBadUIDCooldownDiffuse(t *testing.T) {
	e := newWatchEnv(t, map[string]string{"52283-BAD": "0.88|23"})
	// 短信永远收不到（豪猪 getMessage 一直无码）——"给得出号接不到码"。
	e.fz.msgResp.Store(`{"code":"0","msg":"ok","sms":""}`)
	e.w.Reconfigure(WatchConfig{Enabled: true, IntervalSeconds: 60, WantPerTrigger: 1, Projects: []WatchProject{e.proj(1.0, 5)}})
	if _, err := e.w.AddBudget(10, nil); err != nil {
		t.Fatal(err)
	}
	e.w.tick(context.Background(), e.w.cfg)
	// 触发一次，任务以 0 成功结束（尝试上限收紧为 want×3=4 次下限）。
	waitFor(t, 30*time.Second, func() bool { return e.w.Status().TriggerTotal >= 1 }, "应触发")
	waitFor(t, 30*time.Second, func() bool {
		st := e.w.Status()
		return !st.Running && st.TriggerTotal >= 1
	}, "差码任务应已结束（0 成功）")
	st := e.w.Status()
	if st.EnrolledTotal != 0 {
		t.Fatalf("收不到码不应有成功，实际 %d", st.EnrolledTotal)
	}
	if st.BudgetRemaining != 10 {
		t.Fatalf("失败不扣额度，实际 %.2f", st.BudgetRemaining)
	}
	// 差码被移出账户。
	if n := e.fh.removed.Load(); n < 1 {
		t.Fatalf("差码应从账户移除，实际移除 %d 次", n)
	}
	// 下轮：同一差码在冷却中，即使价格/库存仍达标也不再触发。
	e.w.tick(context.Background(), e.w.cfg)
	if st := e.w.Status(); st.TriggerTotal != 1 {
		t.Fatalf("冷却中的差码不应再触发，实际 %d 次", st.TriggerTotal)
	}
	// 换个好码出现 → 正常触发（冷却只针对差码，不影响别的）。
	e.fh.mu.lock()
	e.fh.uidTable["52283-GOOD"] = "0.50|10"
	e.fh.mu.unlock()
	e.fz.msgResp.Store(`{"code":"0","msg":"ok","sms":"【腾讯科技】您的验证码是 123456，5 分钟内有效"}`)
	e.w.tick(context.Background(), e.w.cfg)
	waitFor(t, 15*time.Second, func() bool { return e.w.Status().EnrolledTotal >= 1 }, "好码应正常触发加号")
}

// TestWatcherCooldownExpires 差码冷却到期自动恢复资格（不是拉黑）。
func TestWatcherCooldownExpires(t *testing.T) {
	e := newWatchEnv(t, map[string]string{"52283-BAD": "0.88|23"})
	e.fz.msgResp.Store(`{"code":"0","msg":"ok","sms":""}`)
	e.w.Reconfigure(WatchConfig{Enabled: true, IntervalSeconds: 60, WantPerTrigger: 1, Projects: []WatchProject{e.proj(1.0, 5)}})
	if _, err := e.w.AddBudget(10, nil); err != nil {
		t.Fatal(err)
	}
	e.w.tick(context.Background(), e.w.cfg)
	waitFor(t, 30*time.Second, func() bool {
		st := e.w.Status()
		return !st.Running && st.TriggerTotal >= 1
	}, "差码第一轮应结束（0 成功）")
	// 手动把冷却拨回过去（等 24h 不现实）。
	e.w.mu.Lock()
	e.w.state.PerUID["52283-BAD"].CooldownUntil = time.Now().Add(-time.Minute).Format(time.RFC3339)
	e.w.mu.Unlock()
	// 差码"补货"（上游真增量）→ 冷却已过 → 允许再试（真实语义：可能
	// 上游换了号源，值得再给一次机会）。
	e.fh.mu.lock()
	e.fh.uidTable["52283-BAD"] = "0.88|30"
	e.fh.mu.unlock()
	e.fz.msgResp.Store(`{"code":"0","msg":"ok","sms":"【腾讯科技】您的验证码是 123456，5 分钟内有效"}`)
	e.w.tick(context.Background(), e.w.cfg)
	waitFor(t, 15*time.Second, func() bool { return e.w.Status().EnrolledTotal >= 1 }, "冷却到期 + 补货应再次触发")
}

// TestWatcherEventsRecorded 成果面板数据源：触发事件结构化入账（时间/
// 项目/码/事件/价格/成功/花费/时长），持久化且今日汇总正确。
func TestWatcherEventsRecorded(t *testing.T) {
	e := newWatchEnv(t, map[string]string{"52283-CHEAP": "0.88|23"})
	e.w.Reconfigure(WatchConfig{Enabled: true, IntervalSeconds: 60, WantPerTrigger: 1, Projects: []WatchProject{e.proj(1.0, 5)}})
	if _, err := e.w.AddBudget(10, nil); err != nil {
		t.Fatal(err)
	}
	e.w.tick(context.Background(), e.w.cfg)
	waitFor(t, 15*time.Second, func() bool { return len(e.w.Status().Events) >= 1 }, "事件应已入账（含记账）")

	st := e.w.Status()
	if len(st.Events) == 0 {
		t.Fatal("事件流水应有 1 条")
	}
	ev := st.Events[0] // 倒序：最新在前
	if ev.UID != "52283-CHEAP" || ev.Sid != "52283" || ev.Price != 0.88 {
		t.Fatalf("事件字段不对：%+v", ev)
	}
	if ev.OK != 1 || ev.Cost != 0.88 {
		t.Fatalf("成功/花费不对：%+v", ev)
	}
	if ev.Time == "" || ev.Duration < 0 || ev.Want != 1 {
		t.Fatalf("时间/时长/目标不对：%+v", ev)
	}
	if !strings.Contains(ev.Ev, "新码") {
		t.Fatalf("事件类型应为新码：%s", ev.Ev)
	}
	// 今日汇总。
	if st.Today.Triggers != 1 || st.Today.OK != 1 || st.Today.Spent != 0.88 {
		t.Fatalf("今日汇总不对：%+v", st.Today)
	}
	if st.Today.SuccessRate != 100 {
		t.Fatalf("今日成功率 = %d，want 100", st.Today.SuccessRate)
	}
	// 持久化：状态文件里有 events 数组。
	waitFor(t, 5*time.Second, func() bool {
		raw, err := readFileStr(filepath.Join(e.tmp, "watcher-state.json"))
		return err == nil && strings.Contains(raw, `"events"`) && strings.Contains(raw, "52283-CHEAP")
	}, "事件应持久化到状态文件")
}

// TestWatcherStateFileShape 状态文件结构（字段名/类型约定：前端与排查依赖）。
func TestWatcherStateFileShape(t *testing.T) {
	e := newWatchEnv(t, map[string]string{})
	if _, err := e.w.AddBudget(3.336, nil); err != nil { // 两位小数化
		t.Fatal(err)
	}
	e.w.persist()
	raw, err := readFileStr(filepath.Join(e.tmp, "watcher-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"budget_remaining", "spent_total", "enrolled_total", "trigger_total", "paused_reason", "per_uid", "updated_at"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("状态文件缺字段 %s", key)
		}
	}
	if doc["budget_remaining"] != 3.34 {
		t.Errorf("budget_remaining = %v，want 3.34（两位小数）", doc["budget_remaining"])
	}
}

func readFileStr(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}
