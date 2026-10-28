package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
)

// withAnthropicAuth accepts the SDK's x-api-key as well as the normal Bearer
// credential, and uses exactly the same group scope as the Chat endpoint.
func (h *Handler) withAnthropicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" && r.Header.Get("x-api-key") != "" {
			r = r.Clone(r.Context())
			r.Header = r.Header.Clone()
			r.Header.Set("Authorization", "Bearer "+r.Header.Get("x-api-key"))
		}
		scope, ok := h.resolveScope(r)
		if !ok && h.frontendSession(r) {
			scope, ok = requestScope{kind: "admin"}, true
		}
		if !ok {
			anthropicError(w, 401, "authentication_error", "missing or invalid API key")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), scopeKey{}, scope))
		h.observeClientRequest(next, w, r)
	}
}
func anthropicError(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": message}})
}
func (h *Handler) anthropicEnabled() bool {
	h.featuresMu.RLock()
	defer h.featuresMu.RUnlock()
	return h.anthropicAPI
}

func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	if !h.anthropicEnabled() {
		anthropicError(w, 404, "not_found_error", "Anthropic Messages API is disabled")
		return
	}
	body, large, err := h.readRequestBody(r)
	if large {
		anthropicError(w, 413, "invalid_request_error", "request body exceeds configured limit")
		return
	}
	if err != nil {
		anthropicError(w, 400, "invalid_request_error", err.Error())
		return
	}
	chat, stream, model, err := anthropicToChat(body)
	if err != nil {
		anthropicError(w, 400, "invalid_request_error", err.Error())
		return
	}
	req := r.Clone(r.Context())
	req.Body = io.NopCloser(bytes.NewReader(chat))
	req.ContentLength = int64(len(chat))
	req.Header = r.Header.Clone()
	// Chat passthrough may emit a vendor-specific wire format; always normalize it.
	req.Header.Set("X-WorkBuddy-Passthrough", "false")
	if !stream {
		rec := httptest.NewRecorder()
		h.chatCompletions(rec, req)
		if rec.Code < 200 || rec.Code >= 300 {
			anthropicChatError(w, rec.Code, rec.Body.Bytes())
			return
		}
		var value map[string]any
		if json.Unmarshal(rec.Body.Bytes(), &value) != nil {
			anthropicError(w, 502, "api_error", "invalid upstream response")
			return
		}
		result, err := anthropicResponse(value, model)
		if err != nil {
			anthropicError(w, 502, "api_error", err.Error())
			return
		}
		writeJSON(w, 200, result)
		return
	}
	sw := newAnthropicStream(w, model)
	h.chatCompletions(sw, req)
	sw.finish()
}
func anthropicChatError(w http.ResponseWriter, status int, data []byte) {
	var v struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &v)
	msg := v.Error.Message
	if msg == "" {
		msg = http.StatusText(status)
	}
	kind := "api_error"
	switch status {
	case 400, 413:
		kind = "invalid_request_error"
	case 401:
		kind = "authentication_error"
	case 403:
		kind = "permission_error"
	case 404:
		kind = "not_found_error"
	case 429:
		kind = "rate_limit_error"
	case 503:
		kind = "overloaded_error"
	}
	anthropicError(w, status, kind, msg)
}

func anthropicToChat(data []byte) ([]byte, bool, string, error) {
	var in map[string]any
	if json.Unmarshal(data, &in) != nil || in == nil {
		return nil, false, "", fmt.Errorf("request body must be a JSON object")
	}
	model, ok := in["model"].(string)
	if !ok || model == "" {
		return nil, false, "", fmt.Errorf("model is required")
	}
	n, ok := numberInt(in["max_tokens"])
	if value, valid := in["max_tokens"].(float64); !valid || value != float64(n) {
		ok = false
	}
	if !ok || n <= 0 {
		return nil, false, "", fmt.Errorf("max_tokens must be a positive integer")
	}
	rawMessages, ok := in["messages"].([]any)
	if !ok || len(rawMessages) == 0 {
		return nil, false, "", fmt.Errorf("messages must be a nonempty array")
	}
	out := map[string]any{"model": model, "max_tokens": n}
	stream, streamOK := in["stream"].(bool)
	if _, present := in["stream"]; present && !streamOK {
		return nil, false, "", fmt.Errorf("stream must be a boolean")
	}
	out["stream"] = stream
	if stream {
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	for _, k := range []string{"temperature", "top_p"} {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}
	if v, ok := in["stop_sequences"]; ok {
		out["stop"] = v
	}
	if meta, ok := in["metadata"].(map[string]any); ok {
		out["metadata"] = meta
	}
	if think, ok := in["thinking"].(map[string]any); ok {
		if typ, _ := think["type"].(string); typ == "enabled" {
			if budget, ok := numberInt(think["budget_tokens"]); ok {
				effort := "high"
				if budget <= 1024 {
					effort = "low"
				} else if budget <= 4096 {
					effort = "medium"
				}
				out["reasoning_effort"] = effort
			}
		}
	}
	if config, ok := in["output_config"].(map[string]any); ok {
		if config["format"] != nil {
			return nil, false, "", fmt.Errorf("output_config.format is not supported")
		}
		if e, ok := config["effort"].(string); ok {
			out["reasoning_effort"] = e
		}
	}
	messages := []any{}
	if system, ok := in["system"]; ok {
		parts, err := anthropicParts(system)
		if err != nil {
			return nil, false, "", err
		}
		messages = append(messages, map[string]any{"role": "system", "content": parts})
	}
	for _, raw := range rawMessages {
		msg, ok := raw.(map[string]any)
		if !ok {
			return nil, false, "", fmt.Errorf("invalid message")
		}
		role, _ := msg["role"].(string)
		if role != "user" && role != "assistant" {
			return nil, false, "", fmt.Errorf("unsupported message role %q", role)
		}
		converted, err := anthropicMessage(role, msg["content"])
		if err != nil {
			return nil, false, "", err
		}
		messages = append(messages, converted...)
	}
	out["messages"] = messages
	if tools, ok := in["tools"].([]any); ok {
		converted := []any{}
		for _, raw := range tools {
			t, ok := raw.(map[string]any)
			if !ok {
				return nil, false, "", fmt.Errorf("invalid tool")
			}
			typ, _ := t["type"].(string)
			if typ != "" && typ != "custom" {
				return nil, false, "", fmt.Errorf("server tool %q is not supported", typ)
			}
			name, _ := t["name"].(string)
			schema, ok := t["input_schema"].(map[string]any)
			if name == "" || !ok {
				return nil, false, "", fmt.Errorf("tool requires name and input_schema")
			}
			fn := map[string]any{"name": name, "parameters": schema}
			if d, ok := t["description"]; ok {
				fn["description"] = d
			}
			converted = append(converted, map[string]any{"type": "function", "function": fn})
		}
		out["tools"] = converted
	}
	if choice, ok := in["tool_choice"].(map[string]any); ok {
		switch choice["type"] {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "none":
			out["tool_choice"] = "none"
		case "tool":
			name, ok := choice["name"].(string)
			if !ok || name == "" {
				return nil, false, "", fmt.Errorf("tool_choice.name is required")
			}
			out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": name}}
		default:
			return nil, false, "", fmt.Errorf("unsupported tool_choice")
		}
		if disable, ok := choice["disable_parallel_tool_use"].(bool); ok && disable {
			out["parallel_tool_calls"] = false
		}
	}
	b, err := json.Marshal(out)
	return b, stream, model, err
}

// anthropicParts preserves the order of text and image blocks.
func anthropicParts(content any) (any, error) {
	if s, ok := content.(string); ok {
		return s, nil
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil, fmt.Errorf("content must be text or an array")
	}
	parts := []any{}
	for _, raw := range blocks {
		b, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid content block")
		}
		switch b["type"] {
		case "text":
			text, ok := b["text"].(string)
			if !ok {
				return nil, fmt.Errorf("invalid text block")
			}
			parts = append(parts, map[string]any{"type": "text", "text": text})
		case "image":
			src, ok := b["source"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid image source")
			}
			url, _ := src["url"].(string)
			if src["type"] == "base64" {
				media, _ := src["media_type"].(string)
				data, _ := src["data"].(string)
				if media == "" || data == "" {
					return nil, fmt.Errorf("invalid base64 image")
				}
				url = "data:" + media + ";base64," + data
			} else if src["type"] != "url" {
				return nil, fmt.Errorf("unsupported image source")
			}
			if url == "" {
				return nil, fmt.Errorf("missing image URL")
			}
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
		default:
			return nil, fmt.Errorf("unsupported content block %v", b["type"])
		}
	}
	return parts, nil
}
func anthropicMessage(role string, content any) ([]any, error) {
	if s, ok := content.(string); ok {
		return []any{map[string]any{"role": role, "content": s}}, nil
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil, fmt.Errorf("message content must be text or an array")
	}
	parts := []any{}
	calls := []any{}
	results := []any{}
	for _, raw := range blocks {
		b, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid content block")
		}
		switch b["type"] {
		case "text", "image":
			p, err := anthropicParts([]any{b})
			if err != nil {
				return nil, err
			}
			parts = append(parts, p.([]any)...)
		case "tool_use":
			if role != "assistant" {
				return nil, fmt.Errorf("tool_use requires assistant role")
			}
			id, _ := b["id"].(string)
			name, _ := b["name"].(string)
			input, ok := b["input"].(map[string]any)
			if id == "" || name == "" || !ok {
				return nil, fmt.Errorf("invalid tool_use")
			}
			args, _ := json.Marshal(input)
			calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(args)}})
		case "tool_result":
			if role != "user" {
				return nil, fmt.Errorf("tool_result requires user role")
			}
			id, _ := b["tool_use_id"].(string)
			if id == "" {
				return nil, fmt.Errorf("tool_result requires tool_use_id")
			}
			result := b["content"]
			if result == nil {
				result = ""
			}
			if _, ok := result.(string); !ok {
				p, err := anthropicParts(result)
				if err != nil {
					return nil, err
				}
				result = p
			}
			// Chat has no is_error field. Preserve the distinction in the result
			// text rather than silently presenting an error as a success.
			if failed, _ := b["is_error"].(bool); failed {
				switch v := result.(type) {
				case string:
					result = "Tool error: " + v
				case []any:
					result = append([]any{map[string]any{"type": "text", "text": "Tool error:"}}, v...)
				}
			}
			results = append(results, map[string]any{"role": "tool", "tool_call_id": id, "content": result})
		case "thinking", "redacted_thinking": // Provider-specific signed reasoning cannot be replayed as OpenAI reasoning.
		default:
			return nil, fmt.Errorf("unsupported content block %v", b["type"])
		}
	}
	out := results
	if len(parts) > 0 || len(calls) > 0 {
		m := map[string]any{"role": role}
		if len(parts) > 0 {
			m["content"] = parts
		} else {
			m["content"] = nil
		}
		if len(calls) > 0 {
			m["tool_calls"] = calls
		}
		out = append(out, m)
	}
	return out, nil
}
func anthropicStop(s string) string {
	switch s {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}
func anthropicUsage(u map[string]any) map[string]any {
	input, _ := numberInt(u["prompt_tokens"])
	output, _ := numberInt(u["completion_tokens"])
	result := map[string]any{"input_tokens": input, "output_tokens": output}
	// OpenAI cached tokens are a subset of prompt_tokens; do not double count.
	if details, ok := u["prompt_tokens_details"].(map[string]any); ok {
		if cached, ok := numberInt(details["cached_tokens"]); ok && cached >= 0 && cached <= input {
			result["input_tokens"] = input - cached
			result["cache_read_input_tokens"] = cached
		}
	}
	return result
}
func anthropicResponse(chat map[string]any, model string) (map[string]any, error) {
	choices, ok := chat["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil, fmt.Errorf("upstream response has no choice")
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid upstream choice")
	}
	msg, _ := choice["message"].(map[string]any)
	blocks := []any{}
	if text, ok := msg["content"].(string); ok && text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	if calls, ok := msg["tool_calls"].([]any); ok {
		for _, raw := range calls {
			c, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid tool call")
			}
			fn, _ := c["function"].(map[string]any)
			args, _ := fn["arguments"].(string)
			var input map[string]any
			if json.Unmarshal([]byte(args), &input) != nil || input == nil {
				return nil, fmt.Errorf("invalid tool call arguments")
			}
			blocks = append(blocks, map[string]any{"type": "tool_use", "id": c["id"], "name": fn["name"], "input": input})
		}
	}
	id, _ := chat["id"].(string)
	if id == "" {
		id = newResponseID()
	}
	finish, _ := choice["finish_reason"].(string)
	usage, _ := chat["usage"].(map[string]any)
	return map[string]any{"id": id, "type": "message", "role": "assistant", "model": model, "content": blocks, "stop_reason": anthropicStop(finish), "stop_sequence": nil, "usage": anthropicUsage(usage)}, nil
}

// Tool arguments are buffered per upstream index until completion. This keeps
// interleaved parallel calls separate and emits valid Anthropic block lifetimes.
type anthropicCall struct {
	id, name string
	args     strings.Builder
}
type anthropicStream struct {
	dst                             http.ResponseWriter
	header                          http.Header
	status                          int
	buf                             bytes.Buffer
	errorBody                       bytes.Buffer
	model, id, reason               string
	usage                           map[string]any
	started, textOpen, done, failed bool
	index                           int
	calls                           map[int]*anthropicCall
}

func newAnthropicStream(w http.ResponseWriter, model string) *anthropicStream {
	return &anthropicStream{dst: w, header: make(http.Header), model: model, calls: map[int]*anthropicCall{}}
}
func (s *anthropicStream) Header() http.Header  { return s.header }
func (s *anthropicStream) WriteHeader(code int) { s.status = code }
func (s *anthropicStream) Flush() {
	if f, ok := s.dst.(http.Flusher); ok && s.started {
		f.Flush()
	}
}
func (s *anthropicStream) emit(typ string, value map[string]any) error {
	value["type"] = typ
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.dst, "event: %s\ndata: %s\n\n", typ, b)
	if err == nil {
		s.Flush()
	}
	return err
}
func (s *anthropicStream) start() error {
	if s.started {
		return nil
	}
	s.started = true
	if s.id == "" {
		s.id = newResponseID()
	}
	s.dst.Header().Set("Content-Type", "text/event-stream")
	s.dst.Header().Set("Cache-Control", "no-cache")
	return s.emit("message_start", map[string]any{"message": map[string]any{"id": s.id, "type": "message", "role": "assistant", "model": s.model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}})
}
func (s *anthropicStream) Write(p []byte) (int, error) {
	if s.status >= 400 {
		return s.errorBody.Write(p)
	}
	s.buf.Write(p)
	for {
		data := s.buf.Bytes()
		i := bytes.Index(data, []byte("\n\n"))
		if i < 0 {
			break
		}
		frame := string(data[:i])
		s.buf.Next(i + 2)
		for _, line := range strings.Split(frame, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				s.done = true
				continue
			}
			var chunk map[string]any
			if json.Unmarshal([]byte(payload), &chunk) != nil {
				continue
			}
			if e, ok := chunk["error"]; ok {
				if err := s.start(); err != nil {
					return 0, err
				}
				s.failed = true
				msg := "upstream stream error"
				if m, ok := e.(map[string]any); ok {
					if v, ok := m["message"].(string); ok {
						msg = v
					}
				}
				if err := s.emit("error", map[string]any{"error": map[string]any{"type": "api_error", "message": msg}}); err != nil {
					return 0, err
				}
				continue
			}
			if s.failed {
				continue
			}
			if id, ok := chunk["id"].(string); ok && s.id == "" {
				s.id = id
			}
			if u, ok := chunk["usage"].(map[string]any); ok {
				s.usage = u
			}
			if err := s.start(); err != nil {
				return 0, err
			}
			choices, _ := chunk["choices"].([]any)
			if len(choices) == 0 {
				continue
			}
			c, _ := choices[0].(map[string]any)
			if f, ok := c["finish_reason"].(string); ok && f != "" {
				s.reason = f
			}
			d, _ := c["delta"].(map[string]any)
			if text, ok := d["content"].(string); ok && text != "" {
				if !s.textOpen {
					s.textOpen = true
					if err := s.emit("content_block_start", map[string]any{"index": s.index, "content_block": map[string]any{"type": "text", "text": ""}}); err != nil {
						return 0, err
					}
				}
				if err := s.emit("content_block_delta", map[string]any{"index": s.index, "delta": map[string]any{"type": "text_delta", "text": text}}); err != nil {
					return 0, err
				}
			}
			if calls, ok := d["tool_calls"].([]any); ok {
				for _, raw := range calls {
					call, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					idx, _ := numberInt(call["index"])
					c := s.calls[idx]
					if c == nil {
						c = &anthropicCall{}
						s.calls[idx] = c
					}
					if id, ok := call["id"].(string); ok && id != "" {
						c.id = id
					}
					fn, _ := call["function"].(map[string]any)
					if name, ok := fn["name"].(string); ok {
						c.name += name
					}
					if args, ok := fn["arguments"].(string); ok {
						c.args.WriteString(args)
					}
				}
			}
		}
	}
	return len(p), nil
}
func (s *anthropicStream) finish() {
	if s.status >= 400 {
		anthropicChatError(s.dst, s.status, s.errorBody.Bytes())
		return
	}
	if s.failed {
		return
	}
	if !s.done || s.reason == "" {
		if !s.started {
			anthropicError(s.dst, 502, "api_error", "upstream stream interrupted")
		} else {
			_ = s.emit("error", map[string]any{"error": map[string]any{"type": "api_error", "message": "upstream stream interrupted"}})
		}
		return
	}
	if err := s.start(); err != nil {
		return
	}
	if s.textOpen {
		_ = s.emit("content_block_stop", map[string]any{"index": s.index})
		s.index++
	}
	keys := make([]int, 0, len(s.calls))
	for k := range s.calls {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	for _, k := range keys {
		c := s.calls[k]
		var input map[string]any
		if c.id == "" || c.name == "" || json.Unmarshal([]byte(c.args.String()), &input) != nil || input == nil {
			_ = s.emit("error", map[string]any{"error": map[string]any{"type": "api_error", "message": "invalid upstream tool call"}})
			return
		}
		_ = s.emit("content_block_start", map[string]any{"index": s.index, "content_block": map[string]any{"type": "tool_use", "id": c.id, "name": c.name, "input": map[string]any{}}})
		_ = s.emit("content_block_delta", map[string]any{"index": s.index, "delta": map[string]any{"type": "input_json_delta", "partial_json": c.args.String()}})
		_ = s.emit("content_block_stop", map[string]any{"index": s.index})
		s.index++
	}
	_ = s.emit("message_delta", map[string]any{"delta": map[string]any{"stop_reason": anthropicStop(s.reason), "stop_sequence": nil}, "usage": anthropicUsage(s.usage)})
	_ = s.emit("message_stop", map[string]any{})
}
