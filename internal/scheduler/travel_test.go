package scheduler

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// travelStub 模拟 growth 域全部端点，记录调用次数与请求参数。
type travelStub struct {
	mu          sync.Mutex
	buddy       string // /buddy/info 的 data 原文
	state       string // /travel/status 的 data 原文
	firstStatus int    // /buddy/first 的 HTTP 状态（200=领养成功）

	infoCalls, statusCalls, departCalls, claimCalls int
	firstCalls, agreementCalls, reportCalls         int
}

func (s *travelStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.URL.Path {
		case "/activity/growth/buddy/info":
			s.infoCalls++
			w.Write([]byte(`{"code":0,"data":{"buddy":` + s.buddy + `}}`))
		case "/activity/growth/buddy/travel/status":
			s.statusCalls++
			w.Write([]byte(`{"code":0,"data":` + s.state + `}`))
		case "/activity/growth/buddy/travel/depart":
			s.departCalls++
			w.Write([]byte(`{"code":0,"data":{}}`))
		case "/activity/growth/buddy/travel/claim":
			s.claimCalls++
			w.Write([]byte(`{"code":0,"data":{"reward_credit":30}}`))
		case "/activity/growth/buddy/first":
			s.firstCalls++
			if s.firstStatus != 0 && s.firstStatus != 200 {
				w.WriteHeader(s.firstStatus)
				w.Write([]byte(`{"code":1000,"msg":"first_buddy task not completed yet"}`))
				return
			}
			w.Write([]byte(`{"code":0,"data":{}}`))
		case "/activity/growth/buddy/agreement":
			s.agreementCalls++
			w.Write([]byte(`{"code":0,"data":{}}`))
		case "/v2/report":
			s.reportCalls++
			w.Write([]byte(`{"code":0,"data":{}}`))
		case "/activity/growth/streak":
			w.Write([]byte(`{"code":0,"data":{"streak":{"days":3}}}`))
		default:
			http.Error(w, "not found", 404)
		}
	})
}

// fastTravel 关闭账号间限速与领养间隔，避免测试白等。
func fastTravel(t *testing.T) {
	t.Helper()
	oldDelay, oldGap := travelAccountDelay, adoptReportGap
	travelAccountDelay, adoptReportGap = 0, 0
	t.Cleanup(func() {
		travelAccountDelay, adoptReportGap = oldDelay, oldGap
	})
}

// newTravelScheduler 构造 travel 相关依赖齐全的调度器。
func newTravelScheduler(t *testing.T, srv *httptest.Server, uids ...string) (*Scheduler, *pool.Pool) {
	t.Helper()
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
	return New(Config{Pool: p, Upstream: up, TravelHours: []int{9, 21}}), p
}

func TestRunTravelNowAdoptsOnNoBuddy(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.agreementCalls != 1 || stub.firstCalls != 1 || stub.reportCalls != 1 {
		t.Errorf("adopt chain: agreement=%d first=%d report=%d want 1/1/1",
			stub.agreementCalls, stub.firstCalls, stub.reportCalls)
	}
}

func TestRunTravelDepartsOnIdle(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: `{"id":7,"name":"喵"}`, state: `{"state":"idle","daily_limit_reached":false}`}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.departCalls != 1 {
		t.Errorf("depart calls=%d want 1", stub.departCalls)
	}
}

func TestRunTravelClaimsOnArrived(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{
		buddy: `{"id":7,"name":"喵"}`,
		state: `{"state":"arrived","record_id":42,"reward_credit":30}`,
	}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.claimCalls != 1 {
		t.Errorf("claim calls=%d want 1", stub.claimCalls)
	}
}

func TestRunTravelSkipsTraveling(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{
		buddy: `{"id":7,"name":"喵"}`,
		state: `{"state":"traveling","record_id":42}`,
	}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.departCalls != 0 || stub.claimCalls != 0 {
		t.Errorf("traveling should skip: depart=%d claim=%d", stub.departCalls, stub.claimCalls)
	}
}

func TestRunTravelSkipsDailyLimit(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{
		buddy: `{"id":7,"name":"喵"}`,
		state: `{"state":"idle","daily_limit_reached":true}`,
	}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.departCalls != 0 {
		t.Errorf("daily limit reached should not depart: %d", stub.departCalls)
	}
}

// TestRunTravelAdoptThresholdTriedOncePerDay 门槛未达（400）当日只试一次，后续巡检静默跳过。
func TestRunTravelAdoptThresholdTriedOncePerDay(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null", firstStatus: 400}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()
	s.RunTravelNow()
	s.RunTravelNow()

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.firstCalls != 1 {
		t.Errorf("adopt should only be attempted once per day: first calls=%d", stub.firstCalls)
	}
}

// TestRunTravelAdoptTriedExpiresNextDay 跨自然日后允许重新尝试领养。
func TestRunTravelAdoptTriedExpiresNextDay(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null", firstStatus: 400}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()
	s.markAdoptTried("u1")          // 强制标记为昨天
	s.mu.Lock()
	s.adoptTried["u1"] = "2000-01-01"
	s.mu.Unlock()
	s.RunTravelNow()

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.firstCalls != 2 {
		t.Errorf("adopt should retry next day: first calls=%d want 2", stub.firstCalls)
	}
}

func TestRunTravelSkipsDisabled(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s, p := newTravelScheduler(t, srv, "u1")
	p.Disable("u1", "test")
	s.RunTravelNow()

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.infoCalls != 0 {
		t.Errorf("disabled account should not be queried: %d", stub.infoCalls)
	}
}

func TestTravelDayAlignsCST(t *testing.T) {
	// UTC 17:00 = CST 次日 01:00，应归属下一个 CST 自然日。
	utc := time.Date(2026, 9, 18, 17, 30, 0, 0, time.UTC)
	if got := travelDay(utc); got != "2026-09-19" {
		t.Errorf("travelDay(UTC 17:30)=%s want 2026-09-19", got)
	}
	// UTC 10:00 = CST 同日 18:00。
	utc = time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	if got := travelDay(utc); got != "2026-09-18" {
		t.Errorf("travelDay(UTC 10:00)=%s want 2026-09-18", got)
	}
}

// TestNextWakeMergesTaskKinds 多任务配到同一小时时，nextWake 返回该时刻全部任务类型。
func TestNextWakeMergesTaskKinds(t *testing.T) {
	now := time.Date(2026, 9, 18, 8, 0, 0, 0, time.Local)
	s := New(Config{
		CheckinHours:   []int{9},
		TravelHours:    []int{9},
		ActivityHours:  []int{9},
		KeepaliveHours: []int{22},
		BlackcatHours:  []int{23},
	})
	next, kinds := s.nextWake(now)
	if next.Hour() != 9 {
		t.Fatalf("next hour=%d want 9", next.Hour())
	}
	want := map[taskKind]bool{taskCheckin: true, taskTravel: true, taskActivity: true}
	got := map[taskKind]bool{}
	for _, k := range kinds {
		got[k] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("kinds=%v missing %v", kinds, k)
		}
	}
	if got[taskKeepalive] || got[taskBlackcat] {
		t.Errorf("kinds=%v should not contain keepalive/blackcat at 9:00", kinds)
	}
}

// TestNextWakeAllDisabled 全部禁用 → 零时刻、无任务。
func TestNextWakeAllDisabled(t *testing.T) {
	s := New(Config{
		CheckinDisabled:   true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		BlackcatDisabled:  true,
	})
	next, kinds := s.nextWake(time.Now())
	if !next.IsZero() || len(kinds) != 0 {
		t.Errorf("all disabled: next=%v kinds=%v want zero/nil", next, kinds)
	}
}

// TestReconfigureWakes Reconfigure 更新时点后 nextWake 立即反映新值。
func TestReconfigureWakes(t *testing.T) {
	s := New(Config{CheckinHours: []int{9}, KeepaliveHours: []int{22}})
	// 关闭默认补齐的 travel/activity/blackcat 排程，让 9/22 之外的时点不参与。
	s.Reconfigure(nil, nil, nil, nil, nil, false, true, true, false, true)
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.Local)
	next, _ := s.nextWake(now)
	if next.Hour() != 22 {
		t.Fatalf("initial next hour=%d want 22", next.Hour())
	}
	s.Reconfigure([]int{15}, nil, nil, []int{22}, nil, false, true, true, false, true)
	next, kinds := s.nextWake(now)
	if next.Hour() != 15 {
		t.Errorf("after reconfigure next hour=%d want 15", next.Hour())
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}
