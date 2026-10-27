package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"workbuddy2api/internal/haozhuma"
	h5 "workbuddy2api/internal/haozhumah5"
)

func TestSMSWorkspaceProfileSaveAndProbeKeepsOtherProvider(t *testing.T) {
	ec := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "ec-fixture" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, "20.00")
	}))
	defer ec.Close()
	h, _, path := newWebuiTestHandler(t, `{"sms":{"haozhuma":{"user":"hao-fixture","pass":"hao-pass-fixture","sid":"52283"}}}`)
	body := fmt.Sprintf(`{"sms":{"haozhuma":{"profile":"ecode","token":"ec-fixture","ecode_base":%q,"ecode_keyword":"腾讯科技"}}}`, ec.URL)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/admin/config", strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	s := h.currentSMSSettings().Normalized()
	if s.Credentials.Haozhuma.User != "hao-fixture" || s.Credentials.Ecode.Token != "ec-fixture" {
		t.Fatal("profile save mixed credentials")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/admin/account/sms/platforms/ecode/verify", strings.NewReader(`{}`)))
	if w.Code != 200 {
		t.Fatalf("saved token verification: %d %s", w.Code, w.Body.String())
	}
	before, _ := os.ReadFile(path)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/admin/account/sms/platforms/ecode/verify", strings.NewReader(`{"token":"bad-fixture"}`)))
	if w.Code != 502 {
		t.Fatal("bad draft verification reused saved token")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("probe persisted draft")
	}
	c, err := h.buildTaskSMS(context.Background(), "ecode")
	if err != nil || c.ValidateMode("haozhuma") == nil {
		t.Fatalf("isolated Ecode task: %v", err)
	}
}

func TestSMSWorkspacePauseStopsRemainingMonitorCandidates(t *testing.T) {
	e := newWatchEnv(t, map[string]string{"52283-A": "0.88|23", "52283-B": "0.90|23"})
	cfg := WatchConfig{Enabled: true, WantPerTrigger: 1, Workers: 1, Projects: []WatchProject{e.proj(1, 1)}}
	e.w.cfg = cfg
	e.en.SetLimits(0, 1)
	if _, err := e.w.AddBudget(10, nil); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	e.en.persist = func(accountCredential, string) (map[string]any, int, error) {
		calls.Add(1)
		paused := cfg
		paused.Enabled = false
		e.w.Reconfigure(paused)
		return nil, 500, errors.New("fixture failure while operator pauses monitor")
	}
	e.w.tick(context.Background(), cfg)
	if calls.Load() != 1 {
		t.Fatalf("paused monitor launched %d candidates", calls.Load())
	}
}

func TestSMSWorkspaceUIDSyncPreservesUnmanagedAndOldOnPartialFailure(t *testing.T) {
	for _, failSecond := range []bool{false, true} {
		t.Run(fmt.Sprint(failSecond), func(t *testing.T) {
			removed := make(chan string, 2)

			deleted := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					list := []map[string]any{{"code": "52283-UNMANAGED", "price": "1", "available": 1}}
					if !deleted {
						list = append(list, map[string]any{"code": "52283-OLD", "price": "1", "available": 1})
					}
					json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"list": list, "total": len(list)}})
					return
				}
				var body struct {
					Codes []string `json:"codes"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				if r.URL.Path == "/bindings/batch" {
					deleted = true
					removed <- strings.Join(body.Codes, ",")
					fmt.Fprint(w, `{"code":0,"data":{"affected":1}}`)
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"results": []map[string]any{{"code": body.Codes[0], "ok": !(failSecond && body.Codes[0] == "52283-NEWB")}}}})
			}))
			defer srv.Close()
			old := h5.BaseURL
			h5.BaseURL = srv.URL
			defer func() { h5.BaseURL = old }()
			h := newTestHandler(t, Config{HaozhumaH5: h5.New("fixture")})
			errs := h.syncUIDSelection(context.Background(), []string{"52283-OLD"}, []string{"52283-NEWA", "52283-NEWB"})
			if failSecond {
				if len(errs) != 1 || !strings.Contains(errs[0], "52283-NEWB") {
					t.Fatal("partial failure not reported")
				}
				select {
				case <-removed:
					t.Fatal("old code removed despite partial failure")
				default:
				}
			} else {
				if len(errs) > 0 {
					t.Fatal(errs)
				}
				select {
				case uid := <-removed:
					if uid != "52283-OLD" {
						t.Fatal("unmanaged code was removed")
					}
				default:
					t.Fatal("old managed code not removed")
				}
			}
		})
	}
}

func TestSMSWorkspaceProbeReusesSavedAccountInsteadOfMask(t *testing.T) {
	h, _, _ := newWebuiTestHandler(t, `{"sms":{"haozhuma":{"user":"fixture-account","pass":"fixture-pass","sid":"52283"}}}`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/admin/account/sms/platforms/haozhuma/verify", strings.NewReader(`{}`)))
	if w.Code != 200 {
		t.Fatalf("probe: %d %s", w.Code, w.Body.String())
	}
}

func TestSMSWorkspaceMonitorLeaseGuardsFetchAndSource(t *testing.T) {
	c := haozhuma.New("fixture")
	a := NewAutoEnroller(nil, c, "52283", nil, nil, nil)
	restore, err := a.borrowMonitorFetch("68899", "68899-UID")
	if err != nil {
		t.Fatal(err)
	}
	written := false
	err = a.applyFetchConfig(haozhuma.Settings{Sid: "999"}, func() error { written = true; return nil })
	if !errors.Is(err, ErrBusyAuth) || written {
		t.Fatal("monitor lease allowed config write")
	}
	if err = a.AutoRunWith(AutoRunOptions{Want: 1, Source: "manual"}); err == nil {
		t.Fatal("manual task entered monitor lease")
	}
	restore()
	if a.Sid() != "52283" || a.Status().ConfigBusy {
		t.Fatal("monitor lease was not restored")
	}
}

func TestSMSWorkspaceLocalFailureChargesReceivedCodeWithoutBanningSupplier(t *testing.T) {
	e := newWatchEnv(t, map[string]string{"52283-CHEAP": "0.88|23"})
	e.en.persist = func(accountCredential, string) (map[string]any, int, error) {
		return nil, 500, errors.New("fixture disk error")
	}
	e.en.SetLimits(0, 1)
	if _, err := e.w.AddBudget(10, nil); err != nil {
		t.Fatal(err)
	}
	e.w.executeTrigger(context.Background(), WatchConfig{WantPerTrigger: 1, Workers: 1}, e.proj(1, 1), "52283-CHEAP", 0.88, "fixture")
	st := e.w.stateSnapshot()
	if st.SpentTotal != 0.88 || st.BudgetRemaining != 9.12 || e.fh.removed.Load() != 0 || e.en.Status().Received != 1 {
		t.Fatalf("wrong received-code accounting or supplier punishment: spent=%v balance=%v removed=%v", st.SpentTotal, st.BudgetRemaining, e.fh.removed.Load())
	}
}

func TestSMSWorkspaceStatusReturnsActualTask(t *testing.T) {
	h, _, _ := newWebuiTestHandler(t, `{}`)
	a := h.cfg.AutoEnroll
	a.mu.Lock()
	a.runWant = 20
	a.runProvider = "ecode"
	a.runSource = "monitor"
	a.runGroups = []string{"default"}
	a.mu.Unlock()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/admin/account/sms/auto-enroll", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"want":20`) || !strings.Contains(w.Body.String(), `"source":"monitor"`) {
		t.Fatal(w.Body.String())
	}
}

func TestSMSWorkspaceMonitorDisableUsesSavedProjects(t *testing.T) {
	h, _, path := newWebuiTestHandler(t, `{}`)
	w := NewUIDWatcher(WatchConfig{Enabled: true, Projects: []WatchProject{{Sid: "52283", HexSID: "a1b2c3d4e5f60718", MaxPrice: 1, MinStock: 1, Enabled: true}}}, nil, nil, "")
	h.cfg.UIDWatcher = w
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("PATCH", "/admin/account/sms/haozhuma/watch", strings.NewReader(`{"enabled":false,"projects":[{"max_price":0}]}`)))
	if r.Code != 200 || w.Status().Enabled {
		t.Fatalf("pause: %d %s", r.Code, r.Body.String())
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"max_price": 1`) {
		t.Fatal("unsaved draft replaced stored projects")
	}
}
