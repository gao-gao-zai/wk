package server

import (
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func TestCurrentGrowthLedgerPreservesHistoryWithoutInflatingProgress(t *testing.T) {
	h := &Handler{growthLedger: newGrowthLedger("", nil)}
	h.growthLedger.record("u", "", "chat_5", 100, 5)
	h.growthLedger.record("u", "", "Buddy_App", 100, 5)
	h.growthLedger.record("u", "", "first_buddy", 300, 5)
	a := &auth.Auth{UID: "u"}
	tasks := []upstream.Task{
		{TaskCode: "first_buddy", Claimed: true},
		{TaskCode: "RichMeow_Chat", Claimed: true},
		{TaskCode: "Library_read", Claimed: true},
		{TaskCode: "wb_wechat_oa_subscribe_task"},
		{TaskCode: "skill_share"},
		{TaskCode: "black_cat", Claimed: true}, // stale upstream snapshot
	}
	ov := h.buildLedgerOverview([]*auth.Auth{a}, [][]upstream.Task{tasks})
	s := ov.Accounts[0]
	if s.TotalCount != 5 || s.DoneCount != 3 || s.Status != "partial" {
		t.Fatalf("old claims inflated current progress: %+v", s)
	}
	if s.Credit != 500 || s.Energy != 15 || len(s.Tasks) != 5 {
		t.Fatalf("historical rewards/claims were lost: %+v", s)
	}
	if len(h.growthLedger.byUID["u"].Claims) != 3 {
		t.Fatal("read-only reconciliation changed the persisted claim history")
	}
	tasks[3].Claimed, tasks[4].Claimed = true, true
	ov = h.buildLedgerOverview([]*auth.Auth{a}, [][]upstream.Task{tasks})
	if ov.Accounts[0].DoneCount != 5 || ov.Accounts[0].Status != "done" || ov.DoneAccounts != 1 {
		t.Fatalf("manual current tasks were not counted: %+v", ov)
	}
}
