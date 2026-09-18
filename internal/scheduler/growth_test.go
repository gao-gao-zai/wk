package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// fastActivity 关闭活跃上报账号间限速。
func fastActivity(t *testing.T) {
	t.Helper()
	old := activityAccountDelay
	activityAccountDelay = 0
	t.Cleanup(func() { activityAccountDelay = old })
}

// newGrowthScheduler 构造活跃上报依赖齐全的调度器。
func newGrowthScheduler(srv *httptest.Server, uids ...string) (*Scheduler, *pool.Pool, *upstream.Client) {
	p := pool.New("")
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	up := &upstream.Client{
		HTTP:            srv.Client(),
		ChatBaseCN:      srv.URL,
		BillingBaseCN:   srv.URL,
		ChatBaseGlobal:  srv.URL,
		BillingBaseGlob: srv.URL,
	}
	return New(Config{Pool: p, Upstream: up}), p, up
}

// activityStreakStub 模拟 /v2/report（200 成功）+ /activity/growth/streak（days 可配）。
type activityStreakStub struct {
	reportCalls atomic.Int32
	streakHits  atomic.Int32
	reportBody  atomic.Value // string
}

func (s *activityStreakStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/report":
			buf := make([]byte, 4096)
			n, _ := r.Body.Read(buf)
			s.reportBody.Store(string(buf[:n]))
			s.reportCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{}}`))
		case "/activity/growth/streak":
			s.streakHits.Add(1)
			w.Write([]byte(`{"code":0,"data":{"streak":{"days":3}}}`))
		default:
			http.Error(w, "not found", 404)
		}
	})
}

func TestRunActivityReportsEachAccount(t *testing.T) {
	fastActivity(t)
	stub := &activityStreakStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s, _, _ := newGrowthScheduler(srv, "u1", "u2")
	s.RunActivityNow(context.Background())

	if stub.reportCalls.Load() != 2 {
		t.Errorf("report calls=%d want 2", stub.reportCalls.Load())
	}
	if stub.streakHits.Load() != 2 {
		t.Errorf("streak self-check hits=%d want 2", stub.streakHits.Load())
	}
}

// TestRunActivityReportEventShape 上报事件体必须是数组且事件带 userId 与 eventCode。
func TestRunActivityReportEventShape(t *testing.T) {
	fastActivity(t)
	stub := &activityStreakStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s, _, _ := newGrowthScheduler(srv, "u1")
	s.RunActivityNow(context.Background())

	body, _ := stub.reportBody.Load().(string)
	if !strings.Contains(body, `"eventCode":"chat_request_send"`) {
		t.Errorf("report body missing chat_request_send: %.200s", body)
	}
	if !strings.Contains(body, `"userId":"u1"`) {
		t.Errorf("report body missing userId: %.200s", body)
	}
	if !strings.HasPrefix(strings.TrimSpace(body), "[") {
		t.Errorf("report body must be an array: %.80s", body)
	}
}

// TestRunActivitySkipsDisabled 禁用账号不发任何调用。
func TestRunActivitySkipsDisabled(t *testing.T) {
	fastActivity(t)
	stub := &activityStreakStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s, p, _ := newGrowthScheduler(srv, "u1")
	p.Disable("u1", "test")
	s.RunActivityNow(context.Background())

	if stub.reportCalls.Load() != 0 {
		t.Errorf("disabled account should not report: %d", stub.reportCalls.Load())
	}
}

// TestRunActivitySelfCheckSilentDrop streak.days=0 → 自检返回 true（可疑）。
func TestRunActivitySelfCheckSilentDrop(t *testing.T) {
	fastActivity(t)
	var reportOK atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/report":
			reportOK.Store(true)
			w.Write([]byte(`{"code":0,"data":{}}`))
		case "/activity/growth/streak":
			w.Write([]byte(`{"code":0,"data":{"streak":{"days":0}}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	s, _, _ := newGrowthScheduler(srv, "u1")
	s.RunActivityNow(context.Background())

	if !reportOK.Load() {
		t.Fatal("report should have been sent")
	}
	// days=0：自检内部只打 warn 日志，不影响主流程——验证不 panic、无返回值异常即可。
}

// ---------------------------------------------------------------------------
// streak 兑换 + 抽奖
// ---------------------------------------------------------------------------

// streakBonusStub 模拟连登 + 抽奖 + 礼包/补偿 + heatmap。
type streakBonusStub struct {
	mu            sync.Mutex
	redeemCalls   atomic.Int32
	drawCalls     atomic.Int32
	chances       int
	tier7dStatus  string
	heatmapMissed bool
	makeupCards   int
	makeupCalls   atomic.Int32
	giftCalls     atomic.Int32
}

func (s *streakBonusStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.URL.Path {
		case "/activity/growth/streak":
			heatmap := "[]"
			if s.heatmapMissed {
				yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
				heatmap = `[{"date":"` + yesterday + `","score":0}]`
			}
			w.Write([]byte(`{"code":0,"data":{"streak":{"days":8},"makeup_cards":{"balance":` +
				itoa(s.makeupCards) + `,"max":3},"redemption_status":{"tier_7d_status":"` + s.tier7dStatus +
				`","tier_14d_status":"locked","tier_28d_status":"locked","tiers":[` +
				`{"tier":"7d","days":7,"credit":200,"energy":10,"cards":1,"chances":2},` +
				`{"tier":"14d","days":14,"credit":400,"energy":20,"cards":1,"chances":3}]}},"heatmap":` + heatmap + `}`))
		case "/activity/growth/redeem":
			s.redeemCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{}}`))
		case "/activity/growth/lottery/summary":
			w.Write([]byte(`{"code":0,"data":{"chances":` + itoa(s.chances) + `,"module":{"enabled":true}}}`))
		case "/activity/growth/lottery/draw":
			s.drawCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{"prize":"credit_50"}}`))
		case "/billing/meter/claim-gift":
			s.giftCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{"credit":100}}`))
		case "/billing/meter/claim-compensation":
			w.Write([]byte(`{"code":0,"data":{"credit":0}}`))
		case "/activity/growth/heatmap":
			w.Write([]byte(`{"code":0,"data":{"cells":[]}}`))
		case "/activity/growth/makeup-cards/use":
			s.makeupCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{}}`))
		default:
			http.Error(w, "not found", 404)
		}
	})
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	digits := ""
	for v > 0 {
		digits = string(rune('0'+v%10)) + digits
		v /= 10
	}
	return digits
}

func TestRunStreakBonusRedeemsAndDraws(t *testing.T) {
	stub := &streakBonusStub{tier7dStatus: "unlocked", chances: 2, makeupCards: 1}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s, _, _ := newGrowthScheduler(srv, "u1")
	s.RunStreakBonusNow()

	if stub.redeemCalls.Load() != 1 {
		t.Errorf("redeem calls=%d want 1（7d unlocked，14d locked）", stub.redeemCalls.Load())
	}
	if stub.drawCalls.Load() != 2 {
		t.Errorf("draw calls=%d want 2", stub.drawCalls.Load())
	}
}

func TestRunStreakBonusSkipsClaimedAndLocked(t *testing.T) {
	stub := &streakBonusStub{tier7dStatus: "claimed", chances: 0}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s, _, _ := newGrowthScheduler(srv, "u1")
	s.RunStreakBonusNow()

	if stub.redeemCalls.Load() != 0 {
		t.Errorf("claimed/locked tiers should not be redeemed: %d", stub.redeemCalls.Load())
	}
}

func TestRunCheckinRunsStreakBonus(t *testing.T) {
	stub := &streakBonusStub{tier7dStatus: "unlocked", chances: 0}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			w.Write([]byte(`{"code":0,"data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":50,"CycleCapacityUsed":0}]}}}}`))
		default:
			stub.handler().ServeHTTP(w, r)
		}
	}))
	defer srv.Close()

	s, _, _ := newGrowthScheduler(srv, "u1")
	s.RunCheckinNow()

	if stub.redeemCalls.Load() != 1 {
		t.Errorf("streak bonus should run after checkin: redeem=%d", stub.redeemCalls.Load())
	}
}

// ---------------------------------------------------------------------------
// blackcat
// ---------------------------------------------------------------------------

func TestRunBlackcatOutsideWindowSkips(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", 404)
	}))
	defer srv.Close()

	// 窗口判断在 upstream.InNightWindow；RunBlackcatNow 内部各账号自行判定。
	// 直接验证调度入口在非窗口时段不 panic（上游侧窗口测试见 upstream 包）。
	if upstream.InNightWindow(time.Date(2026, 9, 18, 12, 0, 0, 0, time.Local)) {
		t.Fatal("12:00 should not be in night window")
	}
	s, _, _ := newGrowthScheduler(srv, "u1")
	// tasks list 404 → BlackcatNeed 报错 → 记日志继续，不 panic。
	s.RunBlackcatNow()
}
