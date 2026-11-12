package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/upstream"
)

type importGrowthResult struct {
	Success int `json:"success"`
	Skipped int `json:"skipped"`
	Results []struct {
		UID          string `json:"uid"`
		OK           bool   `json:"ok"`
		Pending      bool   `json:"pending"`
		GrowthJobID  string `json:"growth_job_id"`
		GrowthStatus string `json:"growth_status"`
	} `json:"results"`
}

func importGrowthApply(t *testing.T, h *Handler, req accountImportRequest) importGrowthResult {
	t.Helper()
	w := transferRequest(t, h, "/admin/accounts/import", req)
	var result importGrowthResult
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatalf("import status=%d; invalid import result", w.Code)
	}
	return result
}

func importGrowthJobCount(h *Handler) int {
	h.growthJobs.mu.Lock()
	defer h.growthJobs.mu.Unlock()
	return len(h.growthJobs.jobs)
}

func importGrowthStub(t *testing.T, h *Handler, handler http.Handler) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	h.cfg.Upstream = &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, ChatBaseGlobal: srv.URL,
		BillingBaseCN: srv.URL, BillingBaseGlob: srv.URL, WebBaseCN: srv.URL,
	}
}

func waitImportGrowth(t *testing.T, h *Handler, id string) map[string]any {
	t.Helper()
	job := h.growthJobs.get(id)
	if job == nil {
		t.Fatal("growth job not registered")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot := job.snapshot()
		phase := snapshot["phase"]
		if phase != "running" && phase != "queued" {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatal("local stub growth job did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestImportGrowthPreviewAndDefaultDoNotEnqueue(t *testing.T) {
	h, _ := testTransferHandler(t)
	var calls atomic.Int64
	importGrowthStub(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"code":0,"data":{"tasks":[]}}`))
	}))
	pre := transferRequest(t, h, "/admin/accounts/import/preview", accountImportRequest{Credential: officialImportCredential(), RunGrowthTasks: true})
	if pre.Code != 200 || importGrowthJobCount(h) != 0 || calls.Load() != 0 || h.cfg.Pool.AuthByUID("official-offline") != nil {
		t.Fatal("preview executed or scheduled growth work")
	}
	result := importGrowthApply(t, h, accountImportRequest{Credential: officialImportCredential(), UIDs: []string{"official-offline"}})
	if result.Success != 1 || result.Results[0].GrowthJobID != "" || importGrowthJobCount(h) != 0 || calls.Load() != 0 {
		t.Fatal("default import scheduled growth work")
	}
}

func TestImportGrowthDurableCNOnlyAndDuplicateSkips(t *testing.T) {
	h, dir := testTransferHandler(t)
	var calls atomic.Int64
	var durable atomic.Bool
	importGrowthStub(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		state, err := os.ReadFile(filepath.Join(dir, "state.json"))
		_, fileErr := os.Stat(filepath.Join(h.cfg.AuthDir, "workbuddy-official-offline.json"))
		if err == nil && fileErr == nil && bytes.Contains(state, []byte("official-offline")) && h.cfg.Pool.IsDisabled("official-offline") {
			durable.Store(true)
		}
		w.Write([]byte(`{"code":0,"data":{"tasks":[]}}`))
	}))
	req := accountImportRequest{Credential: officialImportCredential(), UIDs: []string{"official-offline"}, RunGrowthTasks: true}
	result := importGrowthApply(t, h, req)
	if result.Success != 1 || result.Results[0].GrowthStatus != "queued" || result.Results[0].GrowthJobID == "" {
		t.Fatal("successful CN import did not enqueue growth work")
	}
	waitImportGrowth(t, h, result.Results[0].GrowthJobID)
	if calls.Load() == 0 || !durable.Load() || !h.cfg.Pool.IsDisabled("official-offline") || h.cfg.Pool.Pick() != nil {
		t.Fatal("growth work started before durable quarantine or activated the account")
	}
	before := calls.Load()
	again := importGrowthApply(t, h, req)
	if again.Skipped != 1 || again.Results[0].GrowthJobID != "" || importGrowthJobCount(h) != 1 || calls.Load() != before {
		t.Fatal("duplicate import scheduled growth work")
	}
}

func TestImportGrowthGlobalFailureAndPendingDoNotEnqueue(t *testing.T) {
	for _, scenario := range []string{"global", "group_failure", "persistence_failure"} {
		t.Run(scenario, func(t *testing.T) {
			h, _ := testTransferHandler(t)
			var calls atomic.Int64
			importGrowthStub(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Write([]byte(`{"code":0,"data":{"tasks":[]}}`))
			}))
			req := accountImportRequest{Credential: officialImportCredential(), UIDs: []string{"official-offline"}, RunGrowthTasks: true}
			switch scenario {
			case "global":
				req.Credential = bytes.ReplaceAll(req.Credential, []byte("www.codebuddy.cn"), []byte("www.workbuddy.ai"))
			case "group_failure":
				req.GroupMode, req.Groups = "replace", []string{"missing-group"}
			case "persistence_failure":
				h.cfg.Pool.SetStatePersister(&batchPersister{err: errors.New("offline persistence failure")})
			}
			result := importGrowthApply(t, h, req)
			if importGrowthJobCount(h) != 0 || calls.Load() != 0 {
				t.Fatal("ineligible import scheduled growth work")
			}
			if scenario == "global" && (result.Success != 1 || result.Results[0].GrowthStatus != "unsupported_region") {
				t.Fatal("global import must succeed independently and explain unsupported growth")
			}
			if scenario != "global" && result.Success != 0 {
				t.Fatal("failed import was reported successful")
			}
		})
	}
}

func TestImportGrowthEncryptedSelectionOnlyQueuesSelectedNewAccount(t *testing.T) {
	h, _ := testTransferHandler(t)
	importGrowthStub(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"data":{"tasks":[]}}`))
	}))
	doc := accountTransferDocument{Accounts: []transferredAccount{
		{Credential: officialImportCredential(), Groups: []string{"default"}},
		{Credential: bytes.ReplaceAll(officialImportCredential(), []byte("official-offline"), []byte("not-selected")), Groups: []string{"default"}},
	}}
	bundle, err := sealAccounts(doc, transferTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	result := importGrowthApply(t, h, accountImportRequest{Bundle: bundle, Password: transferTestPassword, UIDs: []string{"official-offline"}, RunGrowthTasks: true})
	if result.Success != 1 || result.Results[0].GrowthStatus != "queued" || h.cfg.Pool.AuthByUID("not-selected") != nil || importGrowthJobCount(h) != 1 {
		t.Fatal("encrypted import scheduled work outside the selected new account")
	}
	waitImportGrowth(t, h, result.Results[0].GrowthJobID)
}

func TestImportGrowthEnqueueConflictFinishesNewJobAndPreservesImport(t *testing.T) {
	h, _ := testTransferHandler(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	importGrowthStub(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"data":{"tasks":[]}}`))
	}))
	defer unblock()
	// Occupy only the admission UID with a local callback. An unmanaged Auth
	// cannot be used now that execution validates live pool identity.
	h.initGrowthRunner()
	if !h.growthRunner.enqueue(accountJobType{uid: "official-offline", run: func() { once.Do(func() { close(entered) }); <-release }}) {
		t.Fatal("could not enqueue isolated blocking fixture")
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("local growth stub not reached")
	}
	result := importGrowthApply(t, h, accountImportRequest{Credential: officialImportCredential(), UIDs: []string{"official-offline"}, RunGrowthTasks: true})
	if result.Success != 1 || result.Results[0].GrowthStatus != "enqueue_failed" || result.Results[0].GrowthJobID == "" || !h.cfg.Pool.IsDisabled("official-offline") {
		t.Fatal("enqueue failure altered successful quarantined import")
	}
	job := h.growthJobs.get(result.Results[0].GrowthJobID)
	if job == nil || job.snapshot()["phase"] == "running" {
		t.Fatal("enqueue rejection left an active empty job")
	}
	unblock()
	deadline := time.Now().Add(3 * time.Second)
	for len(h.growthRunner.pending()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("blocking admission fixture did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestImportGrowthTaskFailureNeverRollsBackSuccessfulImport(t *testing.T) {
	h, _ := testTransferHandler(t)
	importGrowthStub(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"offline stub failure"}`))
	}))
	result := importGrowthApply(t, h, accountImportRequest{Credential: officialImportCredential(), UIDs: []string{"official-offline"}, RunGrowthTasks: true})
	if result.Success != 1 || !result.Results[0].OK {
		t.Fatal("growth execution changed import result")
	}
	snapshot := waitImportGrowth(t, h, result.Results[0].GrowthJobID)
	if snapshot["phase"] != "failed" || h.cfg.Pool.AuthByUID("official-offline") == nil || !h.cfg.Pool.IsDisabled("official-offline") {
		t.Fatal("failed growth work rolled back or enabled imported account")
	}
}

func TestImportGrowthRequiresBooleanSwitch(t *testing.T) {
	h, _ := testTransferHandler(t)
	w := transferRequest(t, h, "/admin/accounts/import", map[string]any{"credential": officialImportCredential(), "uids": []string{"official-offline"}, "run_growth_tasks": "true"})
	if w.Code != http.StatusBadRequest || importGrowthJobCount(h) != 0 || strings.Contains(w.Body.String(), "offline-at") {
		t.Fatal("malformed growth option accepted or credential leaked")
	}
}
