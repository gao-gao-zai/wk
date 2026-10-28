package promptguard

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCaptureDedupIgnoresAccountButPreservesPromptAndModel(t *testing.T) {
	m := memory(t)
	a, _ := m.Capture("model-a", "account-a", messages("original"), messages("outbound"))
	b, _ := m.Capture("model-a", "account-b", messages("original"), messages("outbound"))
	if a != b {
		t.Fatal("account changed dedup identity")
	}
	item, _ := m.Get(a)
	if item.Occurrences != 2 || item.UID != "account-b" {
		t.Fatalf("bad merged record: %+v", item)
	}
	variants := []struct {
		model              string
		original, outbound []Message
	}{
		{"model-b", messages("original"), messages("outbound")},
		{"model-a", messages("original with a new timestamp"), messages("outbound")},
		{"model-a", messages("original"), messages("a new replacement")},
		{"model-a", []Message{{Role: "developer", Content: "original"}}, messages("outbound")},
	}
	for _, v := range variants {
		id, _ := m.Capture(v.model, "account-a", v.original, v.outbound)
		if id == a {
			t.Fatal("different prompt/model boundaries merged")
		}
	}
	if len(m.List()) != 5 {
		t.Fatal("unexpected unique record count")
	}
}

func TestUnavailableControlDoesNotSendFullPromptOrReplaceHistory(t *testing.T) {
	m := memory(t)
	m.SetProbeFactory(func(Case) Probe {
		return func(_ context.Context, msg []Message, _ bool) ProbeResult {
			if strings.Contains(Text(msg), "Bad.") {
				return ProbeResult{Outcome: "blocked", HTTP: 400}
			}
			return ProbeResult{Outcome: "pass", HTTP: 200}
		}
	})
	id, _ := m.Capture("model", "account", messages("Bad."), messages("Bad."))
	_, _ = m.Start(id, "locate", false)
	before := waitJob(t, m, id)
	if before.CandidateLocatedAt == nil || before.CandidateRevision == 0 {
		t.Fatal("candidate provenance missing")
	}
	checks := 0
	m.SetProbeFactory(func(Case) Probe {
		return func(_ context.Context, _ []Message, rewrite bool) ProbeResult {
			checks++
			if rewrite {
				return ProbeResult{Outcome: "pass", HTTP: 200}
			}
			attempted := false
			return ProbeResult{Outcome: "inconclusive", Reason: "account_busy_or_unavailable", RequestAttempted: &attempted}
		}
	})
	job, err := m.Start(id, "verify", false)
	if err != nil || job.Budget != 2 {
		t.Fatalf("wrong verification budget: %+v %v", job, err)
	}
	after := waitJob(t, m, id)
	if checks != 1 || after.Job.Calls != 1 || len(after.Job.Trace) != 1 || after.Job.EndReason != "control_failed" || after.Resolved {
		t.Fatalf("control did not stop verification: %+v", after.Job)
	}
	if after.Job.Trace[0].RequestAttempted == nil || *after.Job.Trace[0].RequestAttempted {
		t.Fatal("local failure counted as network request")
	}
	if !strings.Contains(after.Job.Note, "未发送上游请求") || !strings.Contains(after.Job.Note, "后续完整提示词检查未执行") {
		t.Fatal(after.Job.Note)
	}
	if after.Candidate != before.Candidate || !after.CandidateLocatedAt.Equal(*before.CandidateLocatedAt) {
		t.Fatal("failed verification changed candidate history")
	}
}

func TestDiagnosticMemoizesRepeatedSelectionsWithinJobOnly(t *testing.T) {
	m := memory(t)
	cfg := m.Config()
	cfg.Settings.ProbeBudget = 4
	_ = m.Update(cfg)
	factoryCalls := 0
	m.SetProbeFactory(func(Case) Probe {
		seen := map[string]bool{}
		return func(_ context.Context, msg []Message, rewrite bool) ProbeResult {
			encoded, _ := json.Marshal([]any{msg, rewrite})
			if seen[string(encoded)] {
				return ProbeResult{Outcome: "inconclusive", Reason: "duplicate_network_request"}
			}
			seen[string(encoded)] = true
			factoryCalls++
			attempted := true
			if strings.Contains(Text(msg), "A.") && strings.Contains(Text(msg), "B.") {
				return ProbeResult{Outcome: "blocked", HTTP: 400, RequestAttempted: &attempted}
			}
			return ProbeResult{Outcome: "pass", HTTP: 200, RequestAttempted: &attempted}
		}
	})
	id, _ := m.Capture("model", "account", messages("A. B."), messages("A. B."))
	for round := 1; round <= 2; round++ {
		_, _ = m.Start(id, "locate", false)
		item := waitJob(t, m, id)
		if item.Job.Status != "located" || item.Job.Calls != 4 || item.Job.CacheHits != 2 || len(item.Job.Trace) != 6 || !item.CandidateMinimal {
			t.Fatalf("memoized diagnosis failed: %+v", item.Job)
		}
		if factoryCalls != round*4 {
			t.Fatal("cache reused across manual jobs")
		}
		for _, row := range item.Job.Trace {
			if row.CachedFrom > 0 && (row.RequestAttempted == nil || *row.RequestAttempted) {
				t.Fatal("cached result counted as new request")
			}
		}
	}
}

func TestMemoizationKeepsRewritePolicySeparate(t *testing.T) {
	m := memory(t)
	checks := 0
	m.SetProbeFactory(func(Case) Probe {
		return func(_ context.Context, _ []Message, rewrite bool) ProbeResult {
			checks++
			if rewrite {
				return ProbeResult{Outcome: "blocked", HTTP: 400, Reason: "11128"}
			}
			return ProbeResult{Outcome: "pass", HTTP: 200}
		}
	})
	id, _ := m.Capture("model", "account", messages("You are a helpful assistant."), messages("You are a helpful assistant."))
	_, _ = m.Start(id, "verify", false)
	item := waitJob(t, m, id)
	if checks != 2 || item.Job.Status != "blocked" || item.Job.CacheHits != 0 || item.Job.EndReason != "still_blocked" {
		t.Fatalf("rewrite policy lost: %+v", item.Job)
	}
}
