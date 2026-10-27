package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func TestAdminDSHCompatTogglePersistenceAndOutbound(t *testing.T) {
	upstream.SetDSHCompat(false)
	t.Cleanup(func() { upstream.SetDSHCompat(false) })
	h, path := adminConfigTestEnv(t)
	if got := doAdminConfig(t, h, http.MethodGet, nil)["features"].(map[string]any)["dsh_compat"]; got != false {
		t.Fatalf("old config default: %v", got)
	}
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	base := up.HTTP.Transport
	var sent string
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var doc map[string]any
		if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
			t.Fatal(err)
		}
		sent = doc["messages"].([]any)[0].(map[string]any)["content"].(string)
		return base.RoundTrip(r)
	})
	h.cfg.Upstream = up
	h.cfg.Pool = testPoolWith(&auth.Auth{UID: "u1", AccessToken: "fake", ExpiresAt: 9999999999})
	h.sanitizeHooks.setDSH = upstream.SetDSHCompat
	const original = "You are an AI agent powered by DeepSeek Harness.\nYou are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names. When the user refers to \"this page\", \"this GUI\", or \"this app\" without naming another target, they mean this GUI.\nTokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root; absolute paths identify files or directories on the host."
	const rewritten = "You are an AI agent running on DeepSeek Harness.\nYou are communicating with the user through the DeepSeek Harness Web GUI whose URL the runtime context names. When the user refers to \"this page\", \"this GUI\", or \"this app\" without naming another target, they mean this GUI.\nPaths explicitly referenced by the user are prefixed with @. Resolve relative paths from the workspace root; absolute paths refer to files or directories on the host."
	for _, enabled := range []bool{true, false} {
		out := doAdminConfig(t, h, http.MethodPost, map[string]any{"checkin_hours": []int{9}, "keepalive_hours": []int{22}, "features": map[string]any{"dsh_compat": enabled}})
		if out["updated"].(map[string]any)["dsh_compat"] != enabled {
			t.Fatal("runtime acknowledgement missing")
		}
		if got := doAdminConfig(t, h, http.MethodGet, nil)["features"].(map[string]any)["dsh_compat"]; got != enabled {
			t.Fatal("config echo disagrees with saved toggle")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var saved struct {
			Features map[string]bool `json:"features"`
		}
		if err = json.Unmarshal(raw, &saved); err != nil {
			t.Fatal(err)
		}
		if saved.Features["dsh_compat"] != enabled || !saved.Features["codex_compat"] || !saved.Features["sanitize_blacklist_fingerprints"] {
			t.Fatal("independent feature values were not preserved")
		}
		for _, route := range []string{"/v1/chat/completions", "/v1/responses"} {
			var body []byte
			if route == "/v1/responses" {
				body, _ = json.Marshal(map[string]any{"model": "glm-5.3-flash", "instructions": original, "input": "hello"})
			} else {
				body, _ = json.Marshal(map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "developer", "content": original}, map[string]any{"role": "user", "content": "hello"}}})
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, route, strings.NewReader(string(body))))
			if rec.Code != 200 {
				t.Fatalf("%s: %d %s", route, rec.Code, rec.Body)
			}
			want := original
			if enabled {
				want = rewritten
			}
			if sent != want {
				t.Fatalf("%s enabled=%v: sent %q", route, enabled, sent)
			}
		}
	}
	// Unrelated patches must leave an existing DSH setting intact.
	doAdminConfig(t, h, http.MethodPost, map[string]any{"checkin_hours": []int{9}, "keepalive_hours": []int{22}, "features": map[string]any{"dsh_compat": true}})
	doAdminConfig(t, h, http.MethodPost, map[string]any{"checkin_hours": []int{9}, "keepalive_hours": []int{22}, "features": map[string]any{"passthrough": true}})
	if got := doAdminConfig(t, h, http.MethodGet, nil)["features"].(map[string]any)["dsh_compat"]; got != true {
		t.Fatal("unrelated patch reset DSH compatibility")
	}
}
