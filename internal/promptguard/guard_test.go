package promptguard

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func memory(t *testing.T) *Manager {
	t.Helper()
	m, e := Open("", "")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(m.Close)
	return m
}
func messages(text string) []Message { return []Message{{Role: "system", Content: text}} }
func waitJob(t *testing.T, m *Manager, id string) Case {
	t.Helper()
	until := time.Now().Add(20 * time.Second)
	for time.Now().Before(until) {
		c, _ := m.Get(id)
		if c.Job != nil && c.Job.Status != "running" && c.Job.Status != "queued" {
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return Case{}
}
func TestRewriteAndCaptureBoundaries(t *testing.T) {
	m := memory(t)
	c := m.Config()
	c.Rules = []Rule{{Match: "secret", Replacement: "safe", Enabled: true}, {Match: "safe", Replacement: "clean", Enabled: true}, {Match: "unchanged", Replacement: "oops", Enabled: false}}
	if e := m.Update(c); e != nil {
		t.Fatal(e)
	}
	var doc map[string]any
	_ = json.Unmarshal([]byte(`{"messages":[{"role":"developer","content":[{"type":"input_text","text":"secret unchanged"},{"type":"image_url","image_url":{"url":"secret"}}]},{"role":"user","content":"secret"},{"role":"tool","content":"secret"},{"role":"system","content":"secret secret"}]}`), &doc)
	original := Extract(doc)
	m.Apply(doc)
	out := Extract(doc)
	if Text(original) != "secret unchanged\nsecret secret" || Text(out) != "clean unchanged\nclean clean" {
		t.Fatalf("unexpected content: %s %s", Text(original), Text(out))
	}
	b, _ := json.Marshal(doc)
	if !strings.Contains(string(b), `"role":"user","content"`) && !strings.Contains(string(b), `"content":"secret","role":"user"`) {
		t.Fatal("user changed")
	}
	if !strings.Contains(string(b), `"url":"secret"`) {
		t.Fatal("image changed")
	}
	if len(original) != 2 {
		t.Fatal("non-system messages captured")
	}
	for _, code := range []string{`{"code":11128}`, `{"error":{"code":"11128"}}`} {
		if !IsBlocked(400, []byte(code)) {
			t.Fatal(code)
		}
	}
	for _, body := range []string{`{"code":11140}`, `{"code":11101,"message":"11128"}`, `bad 11128`} {
		if IsBlocked(400, []byte(body)) {
			t.Fatal("false blocked code")
		}
	}
	if IsBlocked(200, []byte(`{"code":11128}`)) {
		t.Fatal("status ignored")
	}
}
func TestEncryptedPersistenceDedupAndRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guard.enc")
	m, e := Open(path, "test-admin-secret")
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	c := m.Config()
	c.Rules = []Rule{{Name: "private-rule", Match: "private-original", Replacement: "replacement", Enabled: true}}
	if e = m.Update(c); e != nil {
		t.Fatal(e)
	}
	id, e := m.Capture("model", "account", messages("full-private-original"), messages("full-outbound"))
	if e != nil {
		t.Fatal(e)
	}
	id2, e := m.Capture("model", "account", messages("full-private-original"), messages("full-outbound"))
	if e != nil || id != id2 {
		t.Fatal("not deduplicated")
	}
	if m.Summary().Pending != 1 {
		t.Fatal("bad pending count")
	}
	if list := m.List(); len(list) != 1 || list[0].Original != nil || list[0].Outbound != nil {
		t.Fatal("list leaks full prompts")
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "private-original") || strings.Contains(string(b), "private-rule") || strings.Contains(string(b), "test-admin-secret") {
		t.Fatal("plaintext leaked to store")
	}
	reopened, e := Open(path, "test-admin-secret")
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	item, _ := reopened.Get(id)
	if item.Occurrences != 2 || Text(item.Original) != "full-private-original" {
		t.Fatal("full prompt lost")
	}
	if _, e = Open(path, "wrong-secret"); e == nil {
		t.Fatal("wrong key accepted")
	}
	stale := reopened.Config()
	if e = reopened.Update(stale); e != nil {
		t.Fatal(e)
	}
	if e = reopened.Update(stale); e == nil {
		t.Fatal("stale write accepted")
	}
}
func TestLocateInteractingSentencesAndVerify(t *testing.T) {
	m := memory(t)
	m.SetProbeFactory(func(Case) Probe {
		return func(_ context.Context, msg []Message, rewrite bool) ProbeResult {
			doc := map[string]any{"messages": []any{}}
			for _, v := range msg {
				doc["messages"] = append(doc["messages"].([]any), map[string]any{"role": v.Role, "content": v.Content})
			}
			if rewrite {
				m.Apply(doc)
			}
			s := Text(Extract(doc))
			if strings.Contains(s, "Alpha.") && strings.Contains(s, "Beta.") {
				return ProbeResult{Outcome: "blocked", HTTP: 400}
			}
			return ProbeResult{Outcome: "pass", HTTP: 200}
		}
	})
	id, _ := m.Capture("model", "uid", messages("Noise. Alpha. Beta. Tail."), messages("Noise. Alpha. Beta. Tail."))
	if _, e := m.Start(id, "locate", false); e != nil {
		t.Fatal(e)
	}
	item := waitJob(t, m, id)
	if item.Job.Status != "located" || !item.CandidateMinimal || !strings.Contains(item.Candidate, "Alpha.") || !strings.Contains(item.Candidate, "Beta.") || strings.Contains(item.Candidate, "Noise") || item.Resolved {
		t.Fatalf("interaction not located: %+v", item)
	}
	c := m.Config()
	c.Rules = []Rule{{Match: "Alpha.", Replacement: "First.", Enabled: true}}
	if e := m.Update(c); e != nil {
		t.Fatal(e)
	}
	if _, e := m.Start(id, "verify", false); e != nil {
		t.Fatal(e)
	}
	item = waitJob(t, m, id)
	if !item.Resolved || item.Job.Calls != 2 {
		t.Fatalf("verification failed: %+v", item.Job)
	}
	c = m.Config()
	c.Settings.ProbeBudget = 12
	if e := m.Update(c); e != nil {
		t.Fatal(e)
	}
	if m.Summary().Pending != 0 {
		t.Fatal("budget change invalidated valid replacement")
	}
	c = m.Config()
	c.Rules = nil
	_ = m.Update(c)
	if m.Summary().Pending != 1 {
		t.Fatal("changed rules left resolved record")
	}
}
func TestBudgetAndUncertainControl(t *testing.T) {
	m := memory(t)
	c := m.Config()
	c.Settings.ProbeBudget = 4
	_ = m.Update(c)
	m.SetProbeFactory(func(Case) Probe {
		return func(_ context.Context, msg []Message, _ bool) ProbeResult {
			if Text(msg) == "You are a helpful assistant." {
				return ProbeResult{Outcome: "pass"}
			}
			return ProbeResult{Outcome: "blocked"}
		}
	})
	id, _ := m.Capture("m", "u", messages("A. B. C. D. E. F. G. H."), messages("A. B. C. D. E. F. G. H."))
	_, _ = m.Start(id, "locate", false)
	item := waitJob(t, m, id)
	if item.Job.Calls > 4 || item.CandidateMinimal || item.Job.Status != "inconclusive" || item.Candidate == "" {
		t.Fatalf("bad budget result: %+v", item)
	}
	m.SetProbeFactory(func(Case) Probe {
		return func(context.Context, []Message, bool) ProbeResult {
			return ProbeResult{Outcome: "inconclusive", HTTP: 429, Reason: "rate_limit"}
		}
	})
	_, _ = m.Start(id, "verify", false)
	item = waitJob(t, m, id)
	if item.Job.Calls != 1 || item.Resolved {
		t.Fatal("uncertain control accepted")
	}
}
func TestAutoOffQueueCancellationAndRestart(t *testing.T) {
	m := memory(t)
	var calls atomic.Int32
	m.SetProbeFactory(func(Case) Probe {
		return func(ctx context.Context, _ []Message, _ bool) ProbeResult {
			calls.Add(1)
			<-ctx.Done()
			return ProbeResult{Outcome: "inconclusive"}
		}
	})
	id, _ := m.Capture("m", "u", messages("off"), messages("off"))
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("auto off sent calls")
	}
	c := m.Config()
	c.Settings.AutoLocate = true
	_ = m.Update(c)
	id1, _ := m.Capture("m", "u", messages("one"), messages("one"))
	id2, _ := m.Capture("m", "u", messages("two"), messages("two"))
	queued, _ := m.Get(id2)
	if queued.Job == nil || queued.Job.Status != "queued" {
		t.Fatal("auto job not queued")
	}
	if e := m.Cancel(id2); e != nil {
		t.Fatal(e)
	}
	if e := m.Cancel(id1); e != nil {
		t.Fatal(e)
	}
	_ = waitJob(t, m, id1)
	if _, e := m.Start(id, "verify", false); e != nil {
		t.Fatal(e)
	}
	_ = m.Cancel(id)
	_ = waitJob(t, m, id)
}
func TestSplitPreservesMessageAndPartBytes(t *testing.T) {
	msg := []Message{{Role: "system", Content: " A.\n中。\n  "}, {Role: "developer", Content: []any{map[string]any{"type": "text", "text": "second! end?"}, map[string]any{"type": "image_url", "image_url": "unchanged"}}}}
	chunks, render := splitMessages(msg)
	ids := make([]int, len(chunks))
	for i := range ids {
		ids[i] = i
	}
	a, _ := json.Marshal(msg)
	b, _ := json.Marshal(render(ids))
	if string(a) != string(b) {
		t.Fatalf("split lost bytes: %s %s", a, b)
	}
}

func TestReplacementExpansionIsBounded(t *testing.T) {
	doc := map[string]any{"messages": []any{map[string]any{"role": "system", "content": strings.Repeat("a", 1024)}}}
	if err := ApplyToDocument(doc, []Rule{{Match: "a", Replacement: strings.Repeat("b", 1<<20), Enabled: true}}); err == nil {
		t.Fatal("unbounded replacement accepted")
	}
}

func TestVerificationStaleRevisionAndNewFailure(t *testing.T) {
	for _, change := range []string{"rules", "failure"} {
		t.Run(change, func(t *testing.T) {
			m := memory(t)
			entered := make(chan struct{})
			release := make(chan struct{})
			m.SetProbeFactory(func(Case) Probe {
				return func(_ context.Context, _ []Message, rewrite bool) ProbeResult {
					if rewrite {
						close(entered)
						<-release
					}
					return ProbeResult{Outcome: "pass", HTTP: 200}
				}
			})
			id, _ := m.Capture("m", "u", messages("full"), messages("full"))
			_, _ = m.Start(id, "verify", false)
			<-entered
			if change == "rules" {
				m.Invalidate()
			} else {
				_, _ = m.Capture("m", "u", messages("full"), messages("full"))
			}
			close(release)
			item := waitJob(t, m, id)
			if item.Resolved {
				t.Fatal("stale verification resolved record")
			}
		})
	}
}

func TestRestartDoesNotReplayAndRetentionPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guard.enc")
	m, e := Open(path, "fixture-key")
	if e != nil {
		t.Fatal(e)
	}
	id, _ := m.Capture("m", "u", messages("active"), messages("active"))
	expired, _ := m.Capture("m", "u", messages("expired"), messages("expired"))
	m.mu.Lock()
	m.state.Cases[id].Job = &Job{Status: "running", Kind: "locate"}
	m.state.Cases[expired].LastSeen = time.Now().Add(-31 * 24 * time.Hour)
	e = m.saveLocked()
	m.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	m.Close()
	reopened, e := Open(path, "fixture-key")
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	c, ok := reopened.Get(id)
	if !ok || c.Job.Status != "interrupted" || reopened.Summary().Active != 0 {
		t.Fatal("restart replayed job")
	}
	if _, ok = reopened.Get(expired); ok {
		t.Fatal("expired full prompt retained")
	}
	again, e := Open(path, "fixture-key")
	if e != nil {
		t.Fatal(e)
	}
	defer again.Close()
	if _, ok = again.Get(expired); ok {
		t.Fatal("retention did not persist")
	}
}

func TestLocateUsesCurrentRulesAfterManualRetry(t *testing.T) {
	m := memory(t)
	m.SetPreparer(func(c Case) ([]Message, error) { return m.Preview(c.Original), nil })
	m.SetProbeFactory(func(Case) Probe {
		return func(_ context.Context, msg []Message, _ bool) ProbeResult {
			s := Text(msg)
			if strings.Contains(s, "Old.") || strings.Contains(s, "Another.") {
				return ProbeResult{Outcome: "blocked"}
			}
			return ProbeResult{Outcome: "pass"}
		}
	})
	id, _ := m.Capture("m", "u", messages("Old. Another."), messages("Old. Another."))
	c := m.Config()
	c.Rules = []Rule{{Match: "Old.", Replacement: "Fixed.", Enabled: true}}
	_ = m.Update(c)
	_, _ = m.Start(id, "locate", false)
	item := waitJob(t, m, id)
	if item.Job.Status != "located" || strings.Contains(item.Candidate, "Old.") || !strings.Contains(item.Candidate, "Another.") || Text(item.Diagnostic) != "Fixed. Another." {
		t.Fatalf("manual retry used historical rules: %+v", item)
	}
}
