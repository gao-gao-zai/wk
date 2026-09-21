package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestResponsesStreamDSHClientShape 模拟 DSH（pi-ai openai-responses）客户端的
// 事件消费逻辑：output_item.added(item.type=reasoning) 建思考槽，
// response.reasoning_text.delta 累积思考文本，completed 终态含 reasoning item。
// 保证我们发出的事件面能被真实客户端消费。
func TestResponsesStreamDSHClientShape(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: {\"id\":\"chatcmpl-dsh\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"think \"}}]}\n\n" +
			"data: {\"id\":\"chatcmpl-dsh\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"step\"}}]}\n\n" +
			"data: {\"id\":\"chatcmpl-dsh\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"}}]}\n\n" +
			"data: {\"id\":\"chatcmpl-dsh\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: [DONE]\n\n", true
	})
	h := newTestHandler(t, Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := doResponsesRequest(h, `{"model":"glm-5.3","input":"hi","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	// 按 SSE 事件逐条解析（与 pi-ai 的事件循环一致）。
	type event struct {
		name string
		data map[string]any
	}
	var events []event
	for _, frame := range strings.Split(rec.Body.String(), "\n\n") {
		var name string
		var payload string
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "event: ") {
				name = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				payload = strings.TrimPrefix(line, "data: ")
			}
		}
		if name == "" || payload == "" || payload == "[DONE]" {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(payload), &data); err != nil {
			t.Fatalf("bad event payload %q: %v", payload, err)
		}
		events = append(events, event{name, data})
	}
	if len(events) == 0 {
		t.Fatal("no events parsed")
	}

	// 模拟 pi-ai createSlot/reasoning delta 消费。
	thinking := ""
	reasoningSlotCreated := false
	textSlotCreated := false
	completedOutput := []any(nil)
	for _, ev := range events {
		switch ev.name {
		case "response.output_item.added":
			item, _ := ev.data["item"].(map[string]any)
			if item == nil {
				t.Fatalf("output_item.added missing item: %v", ev.data)
			}
			switch item["type"] {
			case "reasoning":
				reasoningSlotCreated = true
				if id, _ := item["id"].(string); id == "" {
					t.Fatal("reasoning item missing id")
				}
			case "message":
				textSlotCreated = true
			}
		case "response.reasoning_text.delta":
			if !reasoningSlotCreated {
				t.Fatal("reasoning_text.delta before reasoning slot created")
			}
			delta, _ := ev.data["delta"].(string)
			thinking += delta
		case "response.reasoning_text.done":
			if text, _ := ev.data["text"].(string); text != thinking {
				t.Fatalf("reasoning done text=%q want accumulated %q", text, thinking)
			}
		case "response.completed":
			resp, _ := ev.data["response"].(map[string]any)
			completedOutput, _ = resp["output"].([]any)
		}
	}
	if !reasoningSlotCreated {
		t.Fatal("no reasoning item was added")
	}
	if !textSlotCreated {
		t.Fatal("no message item was added")
	}
	if thinking != "think step" {
		t.Fatalf("accumulated thinking=%q want %q", thinking, "think step")
	}
	if completedOutput == nil {
		t.Fatal("no completed event")
	}
	sawReasoning := false
	for _, raw := range completedOutput {
		if item, ok := raw.(map[string]any); ok && item["type"] == "reasoning" {
			sawReasoning = true
			content, _ := item["content"].([]any)
			if len(content) != 1 {
				t.Fatalf("completed reasoning content parts=%d", len(content))
			}
			if text, _ := content[0].(map[string]any)["text"].(string); text != "think step" {
				t.Fatalf("completed reasoning text=%q", text)
			}
		}
	}
	if !sawReasoning {
		t.Fatal("completed output missing reasoning item")
	}
}
