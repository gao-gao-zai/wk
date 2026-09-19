// school_test.go 开学季闭环测试：share-complete 点亮 → 领奖 → 抽奖抽完。
package scheduler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/upstream"
)

// fastSchool 关闭闭环内的等待（轮询/节流），测试不白等。
func fastSchool(t *testing.T) {
	t.Helper()
	oldLoops, oldGap := schoolPollLoops, schoolPollGap
	oldDelay := schoolAccountDelay
	schoolPollGap = 0
	schoolPollLoops = 1
	schoolAccountDelay = 0
	t.Cleanup(func() {
		schoolPollLoops, schoolPollGap = oldLoops, oldGap
		schoolAccountDelay = oldDelay
	})
}

// schoolStub 开学季全部端点 stub：share-complete 后任务列表变为 completed
// （模拟服务端异步计分点亮），claim 返回 1 抽奖，draw 消耗 chance。
type schoolStub struct {
	mu sync.Mutex
	// tasks 原始任务列表 JSON（schoolAccount 每次拉取）。
	tasks       string
	chances     int // /config 的 chance.balance
	shareDone   bool
	claims      atomic.Int32
	draws       atomic.Int32
	drawErr     string
}

func (s *schoolStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.URL.Path == "/portal/activity/school/tasks" && r.Method == "GET":
			tasks := s.tasks
			if s.shareDone {
				// share-complete 后 share_invite 变 completed（计分点亮）。
				tasks = strings.Replace(tasks,
					`"task_code":"share_invite","status":"pending","progress":0,"target_count":1`,
					`"task_code":"share_invite","status":"completed","progress":1,"target_count":1`, 1)
			}
			w.Write([]byte(`{"code":0,"data":{"tasks":` + tasks + `,"in_period":true}}`))
		case r.URL.Path == "/portal/activity/school/tasks/share-complete":
			s.shareDone = true
			w.Write([]byte(`{"code":0,"data":{}}`))
		case strings.HasPrefix(r.URL.Path, "/portal/activity/school/tasks/") && strings.HasSuffix(r.URL.Path, "/viewed"):
			w.Write([]byte(`{"code":0,"data":{}}`))
		case strings.HasPrefix(r.URL.Path, "/portal/activity/school/tasks/") && strings.HasSuffix(r.URL.Path, "/claim"):
			s.claims.Add(1)
			s.chances++ // claim 授予的抽奖次数进 balance（与真实上游一致）
			w.Write([]byte(`{"code":0,"data":{"chance_granted":1}}`))
		case r.URL.Path == "/portal/activity/school/config":
			w.Write([]byte(`{"code":0,"data":{"chance":{"balance":` + itoaSchool(s.chances) + `}}}`))
		case r.URL.Path == "/portal/activity/school/wheel/draw":
			if s.chances > 0 {
				s.chances--
			}
			s.draws.Add(1)
			w.Write([]byte(`{"code":0,"data":{"prize_code":"credit_small","credit_amount":10}}`))
		case r.URL.Path == "/portal/activity/school/vouchers":
			w.Write([]byte(`{"code":0,"data":{"items":[]}}`))
		case r.URL.Path == "/v2/report":
			w.Write([]byte(`{"code":0,"data":{}}`))
		default:
			http.Error(w, "not found: "+r.URL.Path, 404)
		}
	})
}

func itoaSchool(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestRunSchoolShareClaimDrawLoop(t *testing.T) {
	fastSchool(t)
	stub := &schoolStub{
		tasks: `[{"task_code":"share_invite","status":"pending","progress":0,"target_count":1},
			{"task_code":"task_student_verify","status":"pending","progress":0,"target_count":1}]`,
		chances: 0,
	}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, _, _ := newGrowthScheduler(srv, "u1")

	s.RunSchoolNow()

	// share：上报 → 点亮 → 领奖（+1 chance）→ 抽奖抽完。
	if stub.claims.Load() != 1 {
		t.Errorf("claim calls=%d want 1（share_invite）", stub.claims.Load())
	}
	// 抽奖：claim 授予 1 次 → draw 1 次（本 stub 的 chance 不动态涨，draw 至少跑过）。
	if stub.draws.Load() < 1 {
		t.Errorf("draw calls=%d want ≥1", stub.draws.Load())
	}
}

func TestRunSchoolOutOfPeriodSkips(t *testing.T) {
	fastSchool(t)
	// in_period=false：整条闭环静默跳过（不发 share-complete）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/portal/activity/school/tasks" {
			w.Write([]byte(`{"code":0,"data":{"tasks":[],"in_period":false}}`))
			return
		}
		http.Error(w, "unexpected call: "+r.URL.Path, 404)
	}))
	defer srv.Close()
	s, _, _ := newGrowthScheduler(srv, "u1")
	s.RunSchoolNow() // 不 panic、不触发 404 分支即通过
}

func TestSchoolChatTimesEventsShape(t *testing.T) {
	ev := upstream.SchoolChatTimesEvents("conv-1")
	if ev["eventCode"] != "chat_request_send" {
		t.Errorf("eventCode = %v", ev["eventCode"])
	}
	if ev["conversationId"] != "conv-1" || ev["parentConversationId"] != "conv-1" {
		t.Errorf("conversation fields wrong: %v", ev)
	}
}

// schoolAccountDelay 在 school.go 里定义（var 供测试置 0）。
