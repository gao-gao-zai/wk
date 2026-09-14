// payload.go 改写发往上游的 chat 请求体：
//  1. 强制 stream:true（上游拒绝非流式）
//  2. tool_choice 归一化（上游该字段是 string，对象形式会 400 code=11101）
//  3. developer 消息角色映射为 system（上游不接受 developer）
//  4. 确保首条消息为 system（上游会拒绝 user-first 请求，code=11128）
package upstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
)

// ErrUnprocessableBody 表示请求体无法解码，因而无法做协议改写与内容脱敏。
//
// 之所以要有这个错误而不是"原样透传"：脱敏开关是内容过滤控制，遇到解不开的
// 输入就放行等于 fail-open —— 恰恰是攻击者想要的那一侧。返回错误让调用方
// 以 400 拒绝，控制才真正闭合。
var ErrUnprocessableBody = errors.New("upstream: request body is not decodable JSON")

// PrepareBodyOpt 单 pass 改写；sanitize=false 时跳过内容指纹脱敏，但仍执行上游协议兼容转换。
func PrepareBodyOpt(src []byte, sanitize bool) ([]byte, error) {
	return PrepareBodyOptWithEfforts(src, sanitize, nil)
}

// codexCompatEnabled 进程级开关：true 时改写 Codex CLI 的系统提示词身份句
// （见 codex.go）。由 main 按 features.codex_compat 注入；测试里也直接设置。
var codexCompatEnabled bool

// SetCodexCompat 设置 Codex 兼容改写开关（进程级，启动时配置一次）。
func SetCodexCompat(enabled bool) { codexCompatEnabled = enabled }

// PrepareBodyOptWithEfforts 在 PrepareBodyOpt 基础上按模型 supportedEfforts 降级 reasoning_effort：
// 仅当请求显式携带且模型不支持该档位时，改为 ≤请求档位的最高支持档；支持档全部高于请求档时取最低档；
// 未知模型/未知档位/未携带该字段一律透传。efforts 为 nil 表示未知（不降级）。
//
// 解码失败或重新编码失败时返回 ErrUnprocessableBody（fail-closed）：
// 旧实现返回原始 src，于是"能被上游解析、但过不了 encoding/json"的请求体
// 可以完整绕过脱敏，而且连 stream 都不会被强制。注意空 body 仍然原样返回 ——
// 空请求体由调用方的参数校验负责，不属于"解码失败"。
func PrepareBodyOptWithEfforts(src []byte, sanitize bool, efforts map[string][]string) ([]byte, error) {
	if len(src) == 0 {
		return src, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnprocessableBody, err)
	}
	// json.Unmarshal 对字面量 null 会**成功**并把 obj 留成 nil map，
	// 随后 obj["stream"] = true 会直接 panic（assignment to entry in nil map）。
	// 一个 `null` 请求体就足以打挂一个请求，所以必须显式挡掉。
	if obj == nil {
		return nil, fmt.Errorf("%w: body is not a JSON object", ErrUnprocessableBody)
	}
	obj["stream"] = true
	if model, ok := obj["model"].(string); ok {
		obj["model"] = NormalizeModelID(model)
	}
	normalizeMessageRoles(obj)
	ensureSystemFirstMessage(obj)
	normalizeToolChoice(obj)
	normalizeReasoningEffort(obj, efforts)
	// Codex 兼容改写放在角色映射之后：developer→system 已完成，system
	// 消息（含由 developer 映射来的）都会被检查；放在脱敏之前，两层
	// 改写互不干扰（脱敏管 Claude 指纹，这里管 Codex 身份句）。
	if codexCompatEnabled {
		if msgs, ok := obj["messages"].([]any); ok {
			rewriteCodexPromptMessages(msgs)
		}
	}
	if sanitize {
		if msgs, ok := obj["messages"].([]any); ok {
			sanitizeMessages(msgs)
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("%w: re-encode failed: %v", ErrUnprocessableBody, err)
	}
	return out, nil
}

// normalizeMessageRoles maps OpenAI's developer role to the system role
// accepted by the WorkBuddy upstream while preserving message content and
// every other role unchanged.
func normalizeMessageRoles(obj map[string]any) {
	messages, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, ok := message["role"].(string)
		if ok && strings.EqualFold(role, "developer") {
			message["role"] = "system"
		}
	}
}

// ensureSystemFirstMessage handles a WorkBuddy upstream requirement that the
// first chat message must be a system prompt. OpenAI-compatible clients are
// allowed to start with a user message, so add a neutral prompt only when the
// request does not already begin with system. Existing system/developer
// messages are left untouched and retain their original order.
func ensureSystemFirstMessage(obj map[string]any) {
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) == 0 {
		return
	}
	first, ok := messages[0].(map[string]any)
	if ok {
		if role, _ := first["role"].(string); strings.EqualFold(role, "system") {
			return
		}
	}
	obj["messages"] = append([]any{
		map[string]any{
			"role":    "system",
			"content": "You are a helpful assistant.",
		},
	}, messages...)
}

// NormalizeModelID maps provider aliases to the stable public model IDs used
// by the OpenAI-compatible API. It is intentionally exact so unrelated model
// names are passed through unchanged.
func NormalizeModelID(id string) string {
	switch strings.TrimSpace(id) {
	case "kimi-k3-1":
		return "kimi-k3"
	case "kimi-k2.7":
		return "kimi-k2.7-code"
	default:
		return id
	}
}

// effortRank 档位从低到高。
var effortRank = map[string]int{"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}

// normalizeReasoningEffort 按模型 supportedEfforts 降级 reasoning_effort（snake/camel 双字段兼容）。
//   - 请求档位模型支持 → 原样透传
//   - 请求档位不支持 → 改为 ≤请求档位的最高支持档（降级）
//   - 支持档全部高于请求档 → 取最低支持档（偏离最小）
//   - 未知模型/未知档位/未携带字段/模型未缓存 → 一律透传
func normalizeReasoningEffort(obj map[string]any, efforts map[string][]string) {
	if len(efforts) == 0 {
		return
	}
	model, _ := obj["model"].(string)
	if model == "" {
		return
	}
	supported, ok := efforts[model]
	if !ok || len(supported) == 0 {
		return
	}
	key := ""
	if _, present := obj["reasoning_effort"]; present {
		key = "reasoning_effort"
	} else if _, present := obj["reasoningEffort"]; present {
		key = "reasoningEffort"
	} else {
		return
	}
	reqStr, ok := obj[key].(string)
	if !ok {
		return
	}
	reqStr = strings.TrimSpace(strings.ToLower(reqStr))
	reqIdx, known := effortRank[reqStr]
	if !known {
		return
	}
	// 在 ≤请求档位的支持档里选最高档；命中且与请求不同才改写。
	best, bestIdx := "", -1
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx <= reqIdx && idx > bestIdx {
			best, bestIdx = s, idx
		}
	}
	if best != "" {
		if !strings.EqualFold(best, reqStr) {
			obj[key] = best
			log.Printf("reasoning_effort downgraded model=%s %s -> %s", model, reqStr, best)
		}
		return
	}
	// 支持档全部高于请求档：取最低支持档。
	lowest, lowestIdx := "", 1<<30
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx < lowestIdx {
			lowest, lowestIdx = s, idx
		}
	}
	if lowest != "" {
		obj[key] = lowest
		log.Printf("reasoning_effort floored model=%s %s -> %s", model, reqStr, lowest)
	}
}

// normalizeToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice。
//   - "none"            → 删 tool_choice + 删 tools/functions
//   - {"type":"none"}   → 同上
//   - {"type":"auto"/"required"} → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象/非标量 → 删 tool_choice
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		// Some compatible upstreams otherwise treat a request that contains
		// tools as plain chat and never emit tool_calls. Explicitly selecting
		// auto keeps normal text responses possible while enabling tool use.
		if tools, ok := obj["tools"].([]any); ok && len(tools) > 0 {
			obj["tool_choice"] = "auto"
		}
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}
