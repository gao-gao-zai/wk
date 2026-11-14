package upstream

import (
	"errors"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestRetiredGrowthTasksNeverAuthenticateOrSendRequests(t *testing.T) {
	c, f := newWebsiteFixture(t)
	a := &auth.Auth{UID: "u", AccessToken: "fake-at", RefreshToken: "fake-rt"}
	retired := []string{
		"chat_5", "Model_chat_GLM5.2", "Buddy_App", "Buddy_App_QQ",
		"automation_1", "template_5", "playbook_prompt", "create_canvas",
		"expert_5", "Expert_team_use_3", "Hp_Appearance", "skill_1",
		"Expert_lighthouse", "black_cat", "task/a b", "",
	}
	for _, code := range retired {
		t.Run(code, func(t *testing.T) {
			// A mixed batch must not partially accept the supported task first.
			if err := c.AcceptTasks(a, []string{"RichMeow_Chat", code}); !errors.Is(err, ErrGrowthTaskUnavailable) {
				t.Fatalf("accept error = %v", err)
			}
			if _, _, err := c.ClaimReward(a, code); !errors.Is(err, ErrGrowthTaskUnavailable) {
				t.Fatalf("claim error = %v", err)
			}
		})
	}
	if err := c.AcceptTasks(a, nil); err != nil {
		t.Fatal(err)
	}
	if need, err := c.BlackcatNeed(a); err != nil || need != 0 {
		t.Fatalf("retired night task: need=%d err=%v", need, err)
	}
	if done, err := c.RunNightChats(a, 1); done != 0 || !errors.Is(err, ErrGrowthTaskUnavailable) {
		t.Fatalf("retired night chat: done=%d err=%v", done, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 0 || len(f.logins) != 0 || f.proxyCalls.Load() != 0 {
		t.Fatal("retired tasks or empty accepts caused authentication, routing or HTTP traffic")
	}
}

func TestCurrentGrowthTasksKeepAcceptAndClaimPaths(t *testing.T) {
	c, f := newWebsiteFixture(t)
	a := &auth.Auth{UID: "u", AccessToken: "fake-at", RefreshToken: "fake-rt"}
	codes := CurrentGrowthTaskCodes()
	if len(codes) != 5 {
		t.Fatalf("expected five current tasks, got %v", codes)
	}
	for _, code := range codes {
		if err := c.AcceptTasks(a, []string{code}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := c.ClaimReward(a, code); err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 2*len(codes) {
		t.Fatalf("current task requests = %v", f.requests)
	}
}
