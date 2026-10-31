package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestAnthropicExactToolIntegers(t *testing.T) {
	const exact = "9007199254740993"
	body := `{"model":"m","max_tokens":100,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"lookup","input":{"id":` + exact + `}}]}]}`
	out, _, _, err := anthropicToChat([]byte(body))
	if err != nil || !strings.Contains(string(out), exact) {
		t.Fatalf("input lost precision: %s %v", out, err)
	}
	frames := "data: " + `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"lookup","arguments":"{\"id\":` + exact + `}"}}]},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n"
	for _, stream := range []bool{false, true} {
		rec := anthropicRegressionRequest(t, frames, stream)
		if !strings.Contains(rec.Body.String(), exact) {
			t.Fatalf("stream=%v output lost precision: %s", stream, rec.Body.String())
		}
	}
	if _, _, _, err := anthropicToChat([]byte(body + ` {}`)); err == nil {
		t.Fatal("accepted trailing JSON")
	}
}

func TestAnthropicUsageAliases(t *testing.T) {
	for _, usage := range []string{
		`{"prompt_tokens":100,"completion_tokens":4,"prompt_cache_hit_tokens":60}`,
		`{"input_tokens":100,"output_tokens":4,"input_tokens_details":{"cached_tokens":60}}`,
		`{"input_tokens":100,"output_tokens":4,"cache_read_input_tokens":60}`,
	} {
		for _, stream := range []bool{false, true} {
			frames := "data: " + `{"choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":"stop"}],"usage":` + usage + "}\n\ndata: [DONE]\n\n"
			rec := anthropicRegressionRequest(t, frames, stream)
			_, got := anthropicRegressionResult(t, rec, stream)
			if got["input_tokens"] != float64(40) || got["output_tokens"] != float64(4) || got["cache_read_input_tokens"] != float64(60) {
				t.Fatalf("stream=%v: %v", stream, got)
			}
		}
	}
}

func TestAnthropicResponseHeaders(t *testing.T) {
	frames := "data: " + `{"id":"trace-test","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
	for _, stream := range []bool{false, true} {
		rec := anthropicRegressionRequest(t, frames, stream)
		if rec.Result().Header.Get("X-Request-Id") != "trace-test" {
			t.Fatalf("stream=%v lost trace header: %v", stream, rec.Result().Header)
		}
		if stream && rec.Result().Header.Get("X-Accel-Buffering") != "no" {
			t.Fatal("lost proxy buffering header")
		}
	}
}

type anthropicPingRecorder struct {
	*httptest.ResponseRecorder
	ping chan struct{}
}

func (w *anthropicPingRecorder) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "event: ping") {
		select {
		case w.ping <- struct{}{}:
		default:
		}
	}
	return w.ResponseRecorder.Write(p)
}
func TestAnthropicHeartbeat(t *testing.T) {
	for _, delta := range []string{`{"reasoning_content":"thinking"}`, `{"tool_calls":[{"index":0,"id":"c","function":{"name":"f","arguments":"{"}}]}`} {
		rec := &anthropicPingRecorder{httptest.NewRecorder(), make(chan struct{}, 1)}
		s := newAnthropicStream(rec, "m")
		stop := s.heartbeat(context.Background(), time.Millisecond)
		_, err := s.Write([]byte("data: {\"choices\":[{\"delta\":" + delta + "}]}\n\n"))
		if err != nil {
			stop()
			t.Fatal(err)
		}
		select {
		case <-rec.ping:
		case <-time.After(5 * time.Second):
			stop()
			t.Fatal("missing heartbeat")
		}
		stop()
		stop() // stopping is idempotent and joins the writer.
		if !strings.Contains(rec.Body.String(), "event: ping") {
			t.Fatal("missing ping")
		}
	}
}

func TestAnthropicStrictPolicyAndRuntimeEcho(t *testing.T) {
	body := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"f","strict":true,"input_schema":{"type":"object"}}]}`
	if _, _, _, err := anthropicToChat([]byte(body)); err != nil {
		t.Fatalf("default downgrade rejected: %v", err)
	}
	if _, _, _, err := anthropicToChatWithPolicy([]byte(body), false); err == nil {
		t.Fatal("strict accepted with downgrade disabled")
	}
	h, path := adminConfigTestEnv(t)
	h.featuresMu.Lock()
	h.anthropicAPI = true
	h.featuresMu.Unlock()
	for _, allow := range []bool{false, true} {
		doAdminConfig(t, h, http.MethodPost, map[string]any{"checkin_hours": []int{9}, "keepalive_hours": []int{22}, "features": map[string]any{"anthropic_strict_downgrade": allow}})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
		if !allow && (rec.Code != 400 || !strings.Contains(rec.Body.String(), "anthropic_strict_downgrade")) {
			t.Fatalf("strict policy not applied: %d %s", rec.Code, rec.Body.String())
		}
		if allow && strings.Contains(rec.Body.String(), "anthropic_strict_downgrade is disabled") {
			t.Fatal("policy stayed disabled")
		}
		echo := httptest.NewRecorder()
		h.adminConfig(echo, httptest.NewRequest(http.MethodGet, "/admin/config", nil))
		var config struct {
			Features map[string]bool `json:"features"`
		}
		if err := json.Unmarshal(echo.Body.Bytes(), &config); err != nil {
			t.Fatal(err)
		}
		if !config.Features["anthropic_api"] || config.Features["anthropic_strict_downgrade"] != allow {
			t.Fatalf("wrong runtime echo: %s", echo.Body.String())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &config); err != nil {
			t.Fatal(err)
		}
		if config.Features["anthropic_strict_downgrade"] != allow {
			t.Fatal("policy not persisted")
		}
	}
}
