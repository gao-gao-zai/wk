package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/promptguard"
	"workbuddy2api/internal/upstream"
)

func TestPromptGuardCaptureChatAndResponses(t *testing.T) {
	m, e := promptguard.Open("", "")
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	c := m.Config()
	c.Rules = []promptguard.Rule{{Match: "original", Replacement: "rewritten", Enabled: true}}
	if e = m.Update(c); e != nil {
		t.Fatal(e)
	}
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 400, `{"code":11128,"msg":"rejected"}`, false })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "fake", ExpiresAt: 9999999999}), Upstream: up, PromptGuard: m, APIKey: "admin", MaxRotate: 1})
	for _, route := range []string{"/v1/chat/completions", "/v1/responses"} {
		doc := map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "developer", "content": "original"}, map[string]any{"role": "user", "content": "private-user"}}}
		if strings.HasSuffix(route, "responses") {
			doc = map[string]any{"model": "glm-5.3-flash", "instructions": "original", "input": "private-user"}
		}
		body, _ := json.Marshal(doc)
		r := httptest.NewRequest("POST", route, strings.NewReader(string(body)))
		r.Header.Set("Authorization", "Bearer admin")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("%s %d %s", route, w.Code, w.Body)
		}
	}
	list := m.List()
	if len(list) < 1 {
		t.Fatal("no capture")
	}
	for _, v := range list {
		item, _ := m.Get(v.ID)
		if promptguard.Text(item.Original) != "original" || promptguard.Text(item.Outbound) != "rewritten" {
			t.Fatalf("bad capture %+v", item)
		}
		b, _ := json.Marshal(item)
		if strings.Contains(string(b), "private-user") {
			t.Fatal("user history leaked")
		}
	}
	for _, path := range []string{"summary", "config", "cases", "cases/" + list[0].ID} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/admin/prompt-guard/"+path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s: %d", path, w.Code)
		}
		r := httptest.NewRequest("GET", "/admin/prompt-guard/"+path, nil)
		r.Header.Set("Authorization", "Bearer admin")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("admin %s: %d", path, w.Code)
		}
	}
}
func TestPromptProbeRequiresCompleteSuccess(t *testing.T) {
	for _, test := range []struct{ stream, want string }{
		{"data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", "pass"},
		{"data: {\"choices\":[{\"finish_reason\":null}]}\n\ndata: [DONE]\n\n", "inconclusive"},
		{"data: {\"error\":{\"code\":11128}}\n\n", "blocked"},
		{"data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\ndata: {\"error\":{\"code\":429}}\n\n", "inconclusive"},
	} {
		if got := classifyPromptStream(strings.NewReader(test.stream)); got.Outcome != test.want {
			t.Fatalf("%q: %+v", test.stream, got)
		}
	}
}

func TestPromptGuardCapturesStreamErrorsOnly(t *testing.T) {
	for _, code := range []int{11128, 11140, 11101} {
		for _, stream := range []bool{false, true} {
			m, _ := promptguard.Open("", "")
			payload, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": "fixture"}})
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				return 200, "data: " + string(payload) + "\n\ndata: [DONE]\n\n", true
			})
			h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "fake", ExpiresAt: 9999999999}), Upstream: up, PromptGuard: m, APIKey: "admin", MaxRotate: 1})
			body, _ := json.Marshal(map[string]any{"model": "glm-5.3-flash", "stream": stream, "messages": []any{map[string]any{"role": "system", "content": "system fixture"}, map[string]any{"role": "user", "content": "user fixture"}}})
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body)))
			r.Header.Set("Authorization", "Bearer admin")
			h.ServeHTTP(w, r)
			want := 0
			if code == 11128 {
				want = 1
			}
			if len(m.List()) != want {
				t.Fatalf("code=%d stream=%v got=%d", code, stream, len(m.List()))
			}
			m.Close()
		}
	}
}

func TestPromptGuardGroupKeysCannotReadOrProbe(t *testing.T) {
	h, key, _ := newGroupTestHandler(t)
	for _, tc := range []struct{ method, path string }{{"GET", "summary"}, {"GET", "config"}, {"PUT", "config"}, {"GET", "cases"}, {"GET", "cases/fixture"}, {"POST", "cases/fixture/verify"}, {"POST", "cases/fixture/locate"}, {"DELETE", "cases/fixture"}} {
		r := httptest.NewRequest(tc.method, "/admin/prompt-guard/"+tc.path, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("group key reached %s %s: %d", tc.method, tc.path, w.Code)
		}
	}
}

func TestPromptGuardAutoDiagnosisWaitsForOriginalAccountSlot(t *testing.T) {
	m, _ := promptguard.Open("", "")
	defer m.Close()
	cfg := m.Config()
	cfg.Settings.AutoLocate = true
	_ = m.Update(cfg)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var doc map[string]any
		_ = json.NewDecoder(r.Body).Decode(&doc)
		if strings.Contains(promptguard.Text(promptguard.Extract(doc)), "bad sentence") {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"code":11128}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseOK))
	}))
	defer fake.Close()
	up := upstream.New()
	up.HTTP = fake.Client()
	up.ChatBaseCN = fake.URL
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "fixture", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	h := NewHandler(Config{Pool: p, Upstream: up, PromptGuard: m, APIKey: "admin", MaxRotate: 1})
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.3-flash","messages":[{"role":"system","content":"bad sentence."},{"role":"user","content":"hello"}]}`))
	r.Header.Set("Authorization", "Bearer admin")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("capture request: %d", w.Code)
	}
	list := m.List()
	if len(list) != 1 {
		t.Fatal("missing capture")
	}
	id := list[0].ID
	until := time.Now().Add(10 * time.Second)
	for time.Now().Before(until) {
		c, _ := m.Get(id)
		if c.Job != nil && c.Job.Status == "located" {
			if c.Occurrences != 1 {
				t.Fatal("diagnostic captured itself")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	c, _ := m.Get(id)
	t.Fatalf("automatic single-slot diagnosis failed: %+v", c.Job)
}
