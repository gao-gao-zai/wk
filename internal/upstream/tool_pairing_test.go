package upstream

import (
	"encoding/json"
	"testing"
)

// msg / call / toolResult 是测试侧的构造薄包装，保持用例可读。
func msg(role, content string) map[string]any {
	return map[string]any{"role": role, "content": content}
}

func call(id string) map[string]any {
	return map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "exec", "arguments": "{}"}}
}

func toolResult(id, content string) map[string]any {
	return map[string]any{"role": "tool", "tool_call_id": id, "content": content}
}

func assistantWithCalls(content string, ids ...string) map[string]any {
	calls := make([]any, 0, len(ids))
	for _, id := range ids {
		calls = append(calls, call(id))
	}
	return map[string]any{"role": "assistant", "content": content, "tool_calls": calls}
}

// roleAt 取重排后第 i 条消息的 role（断言用）。
func roleAt(t *testing.T, messages []any, i int) string {
	t.Helper()
	m, ok := messages[i].(map[string]any)
	if !ok {
		t.Fatalf("messages[%d] is not a map: %#v", i, messages[i])
	}
	role, _ := m["role"].(string)
	return role
}

func toolCallIDAt(t *testing.T, messages []any, i int) string {
	t.Helper()
	m, ok := messages[i].(map[string]any)
	if !ok {
		t.Fatalf("messages[%d] is not a map: %#v", i, messages[i])
	}
	id, _ := m["tool_call_id"].(string)
	return id
}

// TestRepackMovesInterveningMessageAfterToolResults：item[8] 场景——纯文本
// assistant 夹在并行 tool_calls 与部分结果中间，repack 必须把它挪到整组之后。
func TestRepackMovesInterveningMessageAfterToolResults(t *testing.T) {
	messages := []any{
		msg("user", "看一下硬件配置"),
		assistantWithCalls("我来查一下", "c00", "c01"),
		toolResult("c00", "r00"),
		msg("assistant", "我来查一下这台机器的硬件信息。"),
		toolResult("c01", "r01"),
	}
	out, changed := repackToolResultBlocks(messages)
	if !changed {
		t.Fatalf("expected reorder, got unchanged: %#v", out)
	}
	if len(out) != 5 {
		t.Fatalf("length=%d want 5", len(out))
	}
	// 期望：assistant(tool_calls) | tool c00 | tool c01 | assistant(text) | (user 在前)
	if roleAt(t, out, 1) != "assistant" {
		t.Fatalf("out[1] role=%s want assistant", roleAt(t, out, 1))
	}
	if toolCallIDAt(t, out, 2) != "c00" || toolCallIDAt(t, out, 3) != "c01" {
		t.Fatalf("tool results not adjacent: ids=%s,%s", toolCallIDAt(t, out, 2), toolCallIDAt(t, out, 3))
	}
	if roleAt(t, out, 4) != "assistant" {
		t.Fatalf("intervening assistant should move to group tail, out[4] role=%s", roleAt(t, out, 4))
	}
}

// TestRepackKeepsContiguousResultsUnchanged：结果本来就连续时零改动、零分配
// （返回原 slice）。
func TestRepackKeepsContiguousResultsUnchanged(t *testing.T) {
	messages := []any{
		assistantWithCalls("", "c00", "c01"),
		toolResult("c00", "r00"),
		toolResult("c01", "r01"),
		msg("user", "next"),
	}
	out, changed := repackToolResultBlocks(messages)
	if changed {
		t.Fatalf("unexpected reorder: %#v", out)
	}
	if len(out) != len(messages) {
		t.Fatalf("length changed: %d", len(out))
	}
}

// TestRepackDoesNotSwallowNextGroupHead：下一个带 tool_calls 的 assistant 是
// 新组头，绝不能被上一组当插入物吞掉——否则它那批结果永远得不到重排。
// 用「第二组结果被文本隔断」的场景同时验证：组头幸存 + 第二组自身完成重排。
func TestRepackDoesNotSwallowNextGroupHead(t *testing.T) {
	messages := []any{
		assistantWithCalls("", "c00"),
		toolResult("c00", "r00"),
		assistantWithCalls("", "c01a", "c01b"),
		toolResult("c01a", "r01a"),
		msg("assistant", "中间文本"),
		toolResult("c01b", "r01b"),
	}
	out, changed := repackToolResultBlocks(messages)
	if !changed {
		t.Fatalf("expected second-group reorder, got unchanged")
	}
	if len(out) != 6 {
		t.Fatalf("length=%d want 6", len(out))
	}
	// 第二组：head(c01a,c01b) | tool c01a | tool c01b | 文本挪尾。
	if toolCallIDAt(t, out, 3) != "c01a" || toolCallIDAt(t, out, 4) != "c01b" {
		t.Fatalf("second group results must be adjacent to its head: %v", out)
	}
	if roleAt(t, out, 5) != "assistant" {
		t.Fatalf("intervening text should be at group tail, out[5]=%s", roleAt(t, out, 5))
	}
	// 组头必须带着自己的 tool_calls 幸存（没被第一组吞成插入物）。
	head := out[2].(map[string]any)
	if tcs, ok := head["tool_calls"].([]any); !ok || len(tcs) != 2 {
		t.Fatalf("second group head lost its tool_calls: %#v", head)
	}
}

// TestCleanupSymmetricTrimOnPartialResults：批 [c1,c2] 只回了 c1 时，裁掉 c2
// 而不是删整批 tool_calls——c1 两侧都保留，不产生半截配对。
func TestCleanupSymmetricTrimOnPartialResults(t *testing.T) {
	messages := []any{
		msg("user", "hi"),
		assistantWithCalls("", "c1", "c2"),
		toolResult("c1", "r1"),
		msg("user", "next"),
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if !changed {
		t.Fatalf("expected cleanup, got unchanged")
	}
	if len(out) != 4 {
		t.Fatalf("length=%d want 4 (user, assistant, tool, user)", len(out))
	}
	assistant := out[1].(map[string]any)
	tcs := assistant["tool_calls"].([]any)
	if len(tcs) != 1 || tcs[0].(map[string]any)["id"] != "c1" {
		t.Fatalf("c2 should be trimmed, kept=%v", tcs)
	}
}

// TestCleanupDropsOrphanToolResult：无对应 tool_call 的孤儿结果整条删除。
func TestCleanupDropsOrphanToolResult(t *testing.T) {
	messages := []any{
		msg("user", "hi"),
		toolResult("ghost", "orphan"),
		msg("user", "next"),
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if !changed {
		t.Fatalf("expected orphan removal")
	}
	if len(out) != 2 {
		t.Fatalf("length=%d want 2", len(out))
	}
}

// TestCleanupKeepsHealthyTrafficUnchanged：全配对健康历史零改动零分配。
func TestCleanupKeepsHealthyTrafficUnchanged(t *testing.T) {
	messages := []any{
		msg("user", "hi"),
		assistantWithCalls("", "c1", "c2"),
		toolResult("c1", "r1"),
		toolResult("c2", "r2"),
		msg("assistant", "done"),
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if changed {
		t.Fatalf("healthy traffic must be untouched: %#v", out)
	}
}

// TestPrepareBodyFixesBrokenToolSequence 端到端：完整 payload 管线对
// 「孤儿调用 + 隔断结果」的复合坏历史完成 repack + cleanup，出站 body 配对完整。
func TestPrepareBodyFixesBrokenToolSequence(t *testing.T) {
	src := []byte(`{
		"model":"glm-5.2",
		"messages":[
			{"role":"user","content":"看一下硬件配置"},
			{"role":"assistant","content":"","tool_calls":[
				{"id":"c00","type":"function","function":{"name":"exec","arguments":"{}"}},
				{"id":"c01","type":"function","function":{"name":"exec","arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"c00","content":"r00"},
			{"role":"assistant","content":"我来查一下这台机器的硬件信息。"},
			{"role":"tool","tool_call_id":"c01","content":"r01"}
		]
	}`)
	out := mustPrepareBody(t, src, false)
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, out)
	}
	messages := body["messages"].([]any)
	// repack：c00/c01 结果连续；cleanup：双侧齐全全保留；插入文本挪到组尾。
	if len(messages) != 6 {
		t.Fatalf("messages=%d want 6 (system+5): %s", len(messages), out)
	}
	// ensureSystemFirstMessage 前置了一条 system，原序列整体后移一位。
	if toolCallIDAt(t, messages, 3) != "c00" || toolCallIDAt(t, messages, 4) != "c01" {
		t.Fatalf("tool results must be adjacent after repack: %s", out)
	}
	if roleAt(t, messages, 5) != "assistant" {
		t.Fatalf("intervening assistant should follow the results: %s", out)
	}
}

// TestPrepareBodyEndToEndOrphanCallTrimmed 端到端：孤儿 tool_call（无结果）
// 被裁掉后，assistant 变纯文本消息，孤儿结果同样被删。
func TestPrepareBodyEndToEndOrphanCallTrimmed(t *testing.T) {
	src := []byte(`{
		"model":"glm-5.2",
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"","tool_calls":[
				{"id":"dead","type":"function","function":{"name":"exec","arguments":"{}"}}
			]},
			{"role":"user","content":"next"}
		]
	}`)
	out := mustPrepareBody(t, src, false)
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	messages := body["messages"].([]any)
	for i, raw := range messages {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, has := m["tool_calls"]; has {
			t.Fatalf("orphan tool_calls must be removed, messages[%d]=%v", i, m)
		}
		if m["role"] == "tool" {
			t.Fatalf("orphan tool result must be removed, messages[%d]=%v", i, m)
		}
	}
}
