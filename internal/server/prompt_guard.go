package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"workbuddy2api/internal/promptguard"
)

func (h *Handler) capturePrompt(model, uid string, original []byte, outbound map[string]any, scope requestScope, allow map[string]bool) {
	if h.cfg.PromptGuard == nil {
		return
	}
	var doc map[string]any
	_ = json.Unmarshal(original, &doc)
	if _, err := h.cfg.PromptGuard.CaptureScoped(model, uid, promptguard.Extract(doc), promptguard.Extract(outbound), capturedPromptScope(scope, allow)); err != nil {
		log.Printf("prompt guard: capture failed (see administrator storage status)")
	}
}

// Observe structured SSE errors without changing the existing stream parser.
type promptErrorReader struct {
	io.ReadCloser
	line     []byte
	oversize bool
	blocked  func()
}

func (r *promptErrorReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if r.blocked == nil {
		return n, err
	}
	for _, b := range p[:n] {
		if b == '\n' {
			if !r.oversize {
				line := bytes.TrimSpace(r.line)
				if bytes.HasPrefix(line, []byte("data:")) && promptguard.IsBlocked(400, bytes.TrimSpace(line[5:])) {
					r.blocked()
					r.blocked = nil
					r.line = nil
					break
				}
			}
			r.line = r.line[:0]
			r.oversize = false
		} else if !r.oversize {
			if len(r.line) >= 1<<20 {
				r.line = nil
				r.oversize = true
			} else {
				r.line = append(r.line, b)
			}
		}
	}
	return n, err
}

func (h *Handler) registerPromptGuard() {
	if m := h.cfg.PromptGuard; m != nil {
		if h.cfg.Upstream != nil {
			h.cfg.Upstream.RewriteSystem = m.Apply
			h.cfg.Upstream.LegacyPromptRulesManaged = m.LegacyMigrated()
		}
		m.SetSessionFactory(h.promptSession)
		m.SetPreparer(func(item promptguard.Case) ([]promptguard.Message, error) {
			if h.cfg.Upstream == nil {
				return item.Outbound, nil
			}
			doc := probeDocument(item.Model, item.Original)
			if _, err := h.cfg.Upstream.PrepareChatBody(doc); err != nil {
				return nil, err
			}
			return promptguard.Extract(doc), nil
		})
	}
	h.mux.HandleFunc("/admin/prompt-guard/", h.withFrontend(h.promptGuardAPI))
}

func (h *Handler) legacyPromptMigrated() bool {
	return h.cfg.PromptGuard != nil && h.cfg.PromptGuard.LegacyMigrated()
}

func (h *Handler) promptGuardAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	m := h.cfg.PromptGuard
	if m == nil {
		writeJSON(w, 503, map[string]any{"error": "提示词管理未启用：需要管理员凭据和可写的数据目录"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/admin/prompt-guard/")
	bad := func(err string) { writeJSON(w, 400, map[string]any{"error": err}) }
	decode := func(out any) bool {
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
		d.DisallowUnknownFields()
		if d.Decode(out) != nil {
			bad("无效或超过 8 MiB 的请求体")
			return false
		}
		var extra any
		if d.Decode(&extra) != io.EOF {
			bad("请求体必须为单个 JSON 对象")
			return false
		}
		return true
	}
	switch {
	case path == "summary" && r.Method == http.MethodGet:
		writeJSON(w, 200, m.Summary())
	case path == "config" && r.Method == http.MethodGet:
		writeJSON(w, 200, m.Config())
	case path == "config" && r.Method == http.MethodPut:
		var c promptguard.Config
		if !decode(&c) {
			return
		}
		if err := m.Update(c); err != nil {
			bad(err.Error())
			return
		}
		writeJSON(w, 200, m.Config())
	case path == "cases" && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]any{"cases": m.List()})
	case strings.HasPrefix(path, "cases/"):
		parts := strings.Split(strings.TrimPrefix(path, "cases/"), "/")
		id := parts[0]
		item, ok := m.Get(id)
		if !ok {
			writeJSON(w, 404, map[string]any{"error": "记录不存在"})
			return
		}
		if len(parts) == 1 && r.Method == http.MethodGet {
			writeJSON(w, 200, item)
			return
		}
		if len(parts) == 1 && r.Method == http.MethodDelete {
			if err := m.Delete(id); err != nil {
				bad(err.Error())
				return
			}
			writeJSON(w, 200, map[string]any{"success": true})
			return
		}
		if len(parts) == 2 && r.Method == http.MethodPost {
			switch parts[1] {
			case "locate", "verify":
				var options struct {
					AccountScope *struct {
						Kind  string `json:"kind"`
						Group string `json:"group,omitempty"`
					} `json:"account_scope,omitempty"`
				}
				if r.Body != nil && r.ContentLength != 0 && !decode(&options) {
					return
				}
				var job promptguard.Job
				var err error
				if options.AccountScope != nil {
					var scope promptguard.AccountScope
					scope, err = h.manualPromptScope(options.AccountScope.Kind, options.AccountScope.Group)
					if err == nil {
						job, err = m.StartWithAccountScope(id, parts[1], scope)
					}
				} else {
					job, err = m.Start(id, parts[1], false)
				}
				if err != nil {
					writeJSON(w, 409, map[string]any{"error": err.Error()})
					return
				}
				writeJSON(w, 202, job)
				return
			case "cancel":
				if err := m.Cancel(id); err != nil {
					bad(err.Error())
					return
				}
				writeJSON(w, 200, map[string]any{"success": true})
				return
			case "preview":
				doc := probeDocument(item.Model, item.Original)
				if h.cfg.Upstream == nil {
					bad("上游未配置")
					return
				}
				if _, err := h.cfg.Upstream.PrepareChatBody(doc); err != nil {
					bad("提示词无法准备")
					return
				}
				writeJSON(w, 200, map[string]any{"messages": promptguard.Extract(doc)})
				return
			}
		}
		writeJSON(w, 405, map[string]any{"error": "不支持的操作"})
	default:
		writeJSON(w, 404, map[string]any{"error": "接口不存在"})
	}
}

func probeDocument(model string, messages []promptguard.Message) map[string]any {
	msgs := make([]any, 0, len(messages)+2)
	for _, m := range promptguard.Clone(messages) {
		msgs = append(msgs, map[string]any{"role": "system", "content": m.Content})
	}
	if len(msgs) == 0 {
		msgs = append(msgs, map[string]any{"role": "system", "content": "You are a helpful assistant."})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "Reply with OK."})
	return map[string]any{"model": model, "stream": true, "max_tokens": 32, "messages": msgs}
}

func classifyPromptStream(rc io.Reader) promptguard.ProbeResult {
	unknown := promptguard.ProbeResult{Outcome: "inconclusive", HTTP: 200, Reason: "incomplete_or_invalid_stream"}
	scanner := bufio.NewScanner(io.LimitReader(rc, (1<<20)+1))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	finished, done := false, false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			done = true
			break
		}
		var event struct {
			Error   json.RawMessage `json:"error"`
			Code    json.RawMessage `json:"code"`
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &event) != nil {
			return unknown
		}
		if promptguard.IsBlocked(400, []byte(data)) {
			return promptguard.ProbeResult{Outcome: "blocked", HTTP: 200, Reason: "11128"}
		}
		if len(event.Error) > 0 && string(event.Error) != "null" || len(event.Code) > 0 && string(event.Code) != "0" && string(event.Code) != `"0"` {
			return unknown
		}
		for _, c := range event.Choices {
			if c.FinishReason != nil && (*c.FinishReason == "stop" || *c.FinishReason == "length" || *c.FinishReason == "tool_calls") {
				finished = true
			}
		}
	}
	if scanner.Err() != nil || !done || !finished {
		return unknown
	}
	return promptguard.ProbeResult{Outcome: "pass", HTTP: 200}
}
