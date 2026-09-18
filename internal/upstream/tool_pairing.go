// tool_pairing.go 出站请求体的 tool 配对安全网：结果块重排 + 孤儿配对裁剪。
//
// 背景：WorkBuddy 上游按 OpenAI wire 协议严格校验 tool 配对——带 tool_calls 的
// assistant 消息，其每一个 tool_call id 都必须有对应的一条 role:tool 结果消息，
// 且结果必须紧跟该 assistant（中间不允许插任何其他消息）；反之 role:tool 也必须
// 有对应的前置 tool_call。任一侧缺失或被隔断，上游都会以 400 code=11148
// （tool_call_sequence_broken）拒绝整个请求，并顶死整条会话。
//
// 两类真实会话形状会踩中这个校验：
//
//  1. 插入物隔断：Codex 的 image_resize_notice 特性会把 <image_resize_notice>
//     作为一条 developer/system 消息插在 tool 结果附近；部分模型（尤其国产模型
//     做 Codex 后端时）也会在发起 tool_call 的同轮输出一条纯文本 assistant 消息，
//     经 Responses 转译后落在 tool_calls 与结果中间：
//
//     assistant tool_calls=[c00 c01]
//     tool c00
//     assistant "我来查一下…"      <- 插在中间，配对被判断裂
//     tool c01
//
//  2. 孤儿配对：工具执行失败（参数非法/超时/工具不存在）时客户端把 tool_calls
//     持久化进历史却写不回结果，或只回了一部分——之后每轮全量重放这条坏历史，
//     上游对每一条用户消息都返回 400，整条会话报废。
//
// 网关是最后一道防线：发出请求前先 repack（只调顺序、不改内容），再 cleanup
// （双侧对称裁剪，宁丢一轮工具上下文也让会话自愈）。语义参考社区分支
// linguo2625469/workbuddy2api-panel 的 tool_pairing.go（其本身吸收自参考仓库
// sse.ts 的 resolveToolPairing），实现为独立重写。所有模型一律执行，独立于
// sanitize 开关——这是「让请求通过」的协议安全网，不是内容过滤。
package upstream

// repackToolResultBlocks 把插在 assistant.tool_calls 与其 tool 结果之间的非 tool
// 消息挪到整组结果之后，保证同一批 tool_call 的结果在 wire 上连续：
//
//	assistant tool_calls=[c00 c01] | tool c00 | X | tool c01
//	→ assistant tool_calls=[c00 c01] | tool c00 | tool c01 | X
//
// 结果顺序保持原相对顺序，不引入新的顺序敏感问题。无插入消息时零改动零分配
// （返回原 slice，changed=false）。
func repackToolResultBlocks(messages []any) ([]any, bool) {
	if len(messages) < 3 {
		return messages, false
	}
	out := make([]any, 0, len(messages))
	changed := false
	i := 0
	for i < len(messages) {
		m, ok := messages[i].(map[string]any)
		if !ok || m["role"] != "assistant" {
			out = append(out, messages[i])
			i++
			continue
		}
		tcs, hasCalls := m["tool_calls"].([]any)
		if !hasCalls || len(tcs) == 0 {
			out = append(out, messages[i])
			i++
			continue
		}
		// 本批 tool_call 期望的结果 id 集合。
		want := map[string]bool{}
		for _, tci := range tcs {
			if tc, ok := tci.(map[string]any); ok {
				if id, _ := tc["id"].(string); id != "" {
					want[id] = true
				}
			}
		}
		out = append(out, messages[i])
		i++
		// 收集紧随其后（允许被其他消息打断）的同批 tool 结果，按原相对顺序；
		// 中间的非 tool 消息暂存 between，待整组结果收齐后回填到组尾。
		var results []any
		var between []any
		sawNonTool := false
		for i < len(messages) {
			mm, ok := messages[i].(map[string]any)
			if !ok {
				break
			}
			role, _ := mm["role"].(string)
			if role == "tool" {
				id, _ := mm["tool_call_id"].(string)
				if !want[id] {
					break // 不属于本批的结果：外层继续处理
				}
				results = append(results, messages[i])
				if sawNonTool {
					changed = true
				}
				i++
				continue
			}
			// assistant 后一条结果都没有：谈不上「隔断」，交给
			// cleanupOrphanToolCalls 处理孤儿。
			if len(results) == 0 {
				break
			}
			// 下一个带 tool_calls 的 assistant 是新组头，绝不能当插入物吞掉——
			// 一旦收进 between，它永远不再被外层循环当组头处理，它自己那批结果
			// 就永远得不到重排。必须 break 交还外层循环。
			if role == "assistant" {
				if next, _ := mm["tool_calls"].([]any); len(next) > 0 {
					break
				}
			}
			between = append(between, messages[i])
			sawNonTool = true
			i++
		}
		out = append(out, results...)
		out = append(out, between...)
	}
	if !changed {
		return messages, false
	}
	return out, true
}

// cleanupOrphanToolCalls 剔除无法配对的 tool_call 与 tool 结果，双侧按同一份
// keepCalls 对称裁剪：
//
//   - 收集全线 role:tool 的 tool_call_id（结果集）与 assistant.tool_calls[].id（调用集）；
//   - keepCalls = 调用集 ∩ 结果集（双侧齐全才保留）；
//   - assistant.tool_calls 只留 keepCalls 命中的调用；过滤后为空则删除整个
//     tool_calls 键（不带工具调用的纯文本 assistant 是合法消息，保留消息本身）；
//   - role:tool 只在对应 tool_call 被保留时才保留，孤儿结果整条删除。
//
// 对称性是关键：批 [c1,c2] 只回了 c1 时，c2 被裁（而不是删整批 tool_calls 键），
// c1 的调用与结果两侧都保留——任何输入都不会产生半截配对。重复 id 与乱序均按
// 集合处理。无任何工具流量时原 slice 原样返回（零分配零改动）。
func cleanupOrphanToolCalls(messages []any) ([]any, bool) {
	if len(messages) == 0 {
		return messages, false
	}
	callIDs := map[string]bool{}
	resultIDs := map[string]bool{}
	hasTraffic := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch msg["role"] {
		case "tool":
			if id, ok := msg["tool_call_id"].(string); ok && id != "" {
				resultIDs[id] = true
				hasTraffic = true
			}
		case "assistant":
			if tcs, ok := msg["tool_calls"].([]any); ok {
				for _, tci := range tcs {
					tc, ok := tci.(map[string]any)
					if !ok {
						continue
					}
					if id, ok := tc["id"].(string); ok && id != "" {
						callIDs[id] = true
						hasTraffic = true
					}
				}
			}
		}
	}
	if !hasTraffic {
		return messages, false
	}
	keepCalls := map[string]bool{}
	for id := range callIDs {
		if resultIDs[id] {
			keepCalls[id] = true
		}
	}
	changed := false
	// 1) 调用侧：按 keepCalls 对称裁剪，过滤后为空才删键。
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "assistant" {
			continue
		}
		tcs, ok := msg["tool_calls"].([]any)
		if !ok || len(tcs) == 0 {
			continue
		}
		keptCalls := make([]any, 0, len(tcs))
		for _, tci := range tcs {
			tc, ok := tci.(map[string]any)
			if !ok {
				continue
			}
			if id, _ := tc["id"].(string); keepCalls[id] {
				keptCalls = append(keptCalls, tc)
			}
		}
		if len(keptCalls) == len(tcs) {
			continue // 整批齐全：零改动
		}
		changed = true
		if len(keptCalls) == 0 {
			delete(msg, "tool_calls")
			continue
		}
		msg["tool_calls"] = keptCalls
	}
	// 2) 结果侧：只有对应 tool_call 被保留才保留；孤儿结果整条删除。
	kept := make([]any, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			kept = append(kept, m)
			continue
		}
		if role, _ := msg["role"].(string); role == "tool" {
			id, _ := msg["tool_call_id"].(string)
			if !keepCalls[id] {
				changed = true
				continue
			}
		}
		kept = append(kept, m)
	}
	if !changed {
		return messages, false
	}
	return kept, true
}
