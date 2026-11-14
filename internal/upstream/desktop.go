// desktop.go 桌面客户端（WorkBuddy Desktop 5.5.6）行为指纹上报。
//
// 桌面端点亮「需电脑端」类任务的关键不是独立端点，而是同一 POST /v2/report 通道上
// **不同的客户端指纹**：
//
//	POST https://copilot.tencent.com/v2/report        ← chatBase（CLI 上报走 billingBase）
//	User-Agent: WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1
//	X-Domain: copilot.tencent.com, X-Product: SaaS, X-User-Id: <uid>
//	Body: [ {...event...} ]                           ← 数组
//
// 每个事件除业务字段外必带桌面指纹（ideName/ideType=WorkBuddy、
// extName=workbuddy-desktop 等）。
//
// 移植自 linguo2625469/workbuddy2api-panel（同源 MIT fork），事件形状来自
// 其 app.asar 逆向 + 抓包实测（data/desktop-task-protocol.md）。服务端对
// 事件链有一定真实性校验倾向（RichMeow 需要消息成功回执），本模块按实测
// 事件形状发送；任务执行侧按 attempt 语义处理结果。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"workbuddy2api/internal/auth"
)

const (
	desktopReportPath    = "/v2/report"
	desktopAppearanceSet = "/v2/user-asset/appearance/set"
	// marketExpertListPath 专家市场列表（专家类任务需真实专家 id）。
	marketExpertListPath = "/portal/operation-platform/market/expert/list"
)

// 事件公共字段里的构建信息按账号所在域取，来源是官方安装包 product.json 的
// commit / date（macOS 包）：国内版 5.9.3、国际版 5.9.4。这两个值属于产品二进制，
// 不能跨版本或跨域混用；换客户端版本时要一起重新核对。
const (
	desktopCommitCN          = "d46924219e79332f49c37ccebf27292f87001b19"
	desktopReleaseDateCN     = int64(1793093023713) // 2026-10-27T09:23:43.713Z
	desktopCommitGlobal      = "fa0a0901906addaa1a4259f6df347388a5563ded"
	desktopReleaseDateGlobal = int64(1793290653510) // 2026-10-29T16:17:33.510Z
)

// deriveID uses a persisted opaque identity. Legacy/read-only accounts retain
// their previous UID-based identity so persistence failures do not break chat.
func deriveID(a *auth.Auth, salt string) string {
	p := a.Device()
	switch salt {
	case "machine":
		return p.MachineID
	case "session":
		return p.SessionID
	case "webmachine":
		return p.WebMachineID
	case "mpmachine":
		return p.MPMachineID
	default:
		panic("unknown device identity scope")
	}
}

// DesktopEvent 桌面端事件：业务字段任意（map），公共指纹由 ReportDesktopEvent 注入。
type DesktopEvent map[string]any

// desktopFingerprint 公共桌面指纹字段（注入每个事件，覆盖同名业务键）。
func desktopFingerprint(a *auth.Auth) map[string]any {
	creds := a.Snapshot()
	device := a.Device()
	now := time.Now().UnixMilli()
	osName := "win32"
	if device.IsMac() {
		osName = "darwin"
	}
	fp := map[string]any{
		"timezone":     device.Timezone,
		"reportDelay":  2000,
		"userId":       creds.UID,
		"username":     creds.Nickname,
		"userNickname": creds.Nickname,
		"product":      "SaaS",
		"releaseDate":  int64(1789036585355),
		"commit":       "5f9692923c93033111c51ad7b003eb80204a9b75",
		"ideName":      "WorkBuddy",
		"ideType":      "WorkBuddy",
		"ideVersion":   desktopVersion,
		"machineId":    device.MachineID,
		"sessionId":    device.SessionID,
		"extName":      "workbuddy-desktop",
		"extVersion":   desktopVersion,
		"os":           osName,
		"arch":         device.Arch,
		"osVersion":    device.OSVersion,
		"cpuCores":     device.CPUCores,
		"memorySize":   device.MemoryGiB,
		"timestamp":    now,
		"presentAt":    now,
	}
	if device.CPUModel != "" {
		fp["cpuModel"] = device.CPUModel
	}
	return fp
}

// ReportDesktopEvent 以桌面客户端指纹向 copilot.tencent.com/v2/report 批量上报事件。
// events 为业务载荷（eventCode 等字段由调用方给出）；公共指纹自动注入，
// 业务字段优先，账号设备字段由统一设备档案固定。
func (c *Client) ReportDesktopEvent(a *auth.Auth, events ...DesktopEvent) error {
	if len(events) == 0 {
		return fmt.Errorf("desktop report: no events")
	}
	creds := a.Snapshot()
	fp := desktopFingerprint(a)
	if c.desktopHeaders() {
		version := c.clientVersion(a)
		fp["ideVersion"], fp["extVersion"] = version, version
		// 构建信息只适用于核实过的区域和版本。自定义版本不能借用旧构建信息。
		delete(fp, "commit")
		delete(fp, "releaseDate")
		if a.Region() == auth.RegionGlobal && version == "5.9.4" {
			fp["commit"], fp["releaseDate"] = desktopCommitGlobal, desktopReleaseDateGlobal
		} else if a.Region() == auth.RegionCN && version == "5.9.3" {
			fp["commit"], fp["releaseDate"] = desktopCommitCN, desktopReleaseDateCN
		}
	}
	arr := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		arr = append(arr, mergeReportEvent(fp, ev))
	}
	raw, err := json.Marshal(arr)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+desktopReportPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", c.desktopUserAgent(a, false))
	req.Header.Set("X-Domain", c.chatBase(a))
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Request-ID", NewEventID())
	if creds.UID != "" {
		req.Header.Set("X-User-Id", creds.UID)
	}
	if c.desktopHeaders() {
		req.Header.Del("X-Domain")
		c.commonHeaders(req, a)
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	}
	_, err = c.doJSON(req, a)
	return err
}

// DesktopChatSequence 构造一次「桌面端成功对话」的完整事件链
// （agent_task_created → chat_message_send → chat_request_send →
// chat_message_response(isSuccessful) → chat_message_status → chat_request_response）。
// 实测该链点亮 RichMeow_Chat。conversationID/requestID/messageID 由调用方生成。
func DesktopChatSequence(conversationID, requestID, messageID, modelID, modelName string) []DesktopEvent {
	uuid := func() string { return requestID }
	mk := func(code string, extra map[string]any) DesktopEvent {
		ev := DesktopEvent{"eventCode": code}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	return []DesktopEvent{
		mk("agent_task_created", map[string]any{
			"source": "LOCAL", "name": "working", "task_target": "local", "mode": "craft",
			"requestModelId": modelID, "requestModelName": modelName,
			"has_repo": false, "repo_type": "none", "workspace_type": "empty",
			"has_connector": false, "connector_types": []any{},
			"has_mention": false, "mention_types": []any{},
			"has_template": false, "action": "", "template_name": "",
			"has_expert": false, "expert_id": "", "expert_name": "", "expert_industry_id": "",
			"has_skill": false, "skill_names": []any{},
			"conversationId": conversationID, "messageId": messageID,
			"buddyId": "", "buddyName": "",
		}),
		mk("chat_message_send", map[string]any{
			"messageId": messageID + "-assistant", "historyCount": 0,
			"isContextTruncated": false, "currentStepCount": 1,
			"traceId": uuid(), "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
		}),
		mk("chat_request_send", map[string]any{
			"inputLength": 24, "isPlan": false, "isAutoExecuteTerminal": false,
			"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
			"maxSteps": 500, "temperature": 0, "maxRetries": 0,
			"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
			"codebaseId": "", "mentionContextCount": 0, "command": "",
			"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
			"traceId": uuid(), "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
			"codebuddy.session_id":              conversationID,
			"codebuddy.conversation_request_id": requestID,
		}),
		mk("chat_message_response", map[string]any{
			"messageId": messageID + "-assistant", "responseModelId": modelID,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"firstTokenAt": time.Now().UnixMilli(), "traceId": uuid(),
			"conversationId": conversationID,
			"rootRequestId":  requestID, "parentConversationId": conversationID,
			"agentName": "cli", "agentType": "main",
			"codebuddy.session_id":              conversationID,
			"codebuddy.conversation_request_id": requestID,
		}),
		mk("chat_message_status", map[string]any{
			"messageId": messageID + "-assistant", "messageErrorCode": "0",
			"traceId": uuid(), "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
		}),
		mk("chat_request_response", map[string]any{
			"mode": "craft", "toolCallCount": 0,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"rootRequestId": requestID, "parentConversationId": conversationID,
		}),
	}
}

// SetAppearanceTheme 应用外观主题（实测：POST copilot.tencent.com/v2/user-asset/appearance/set，
// 主题 resource_key 例 "theme-tkmw7j"，浅色 "light"、深色 "dark"）。
// 纯 API set 不计 Hp_Appearance 分——需配合 appearance_skin_apply 事件（见任务动作）。
func (c *Client) SetAppearanceTheme(a *auth.Auth, resourceKey string) error {
	body := map[string]string{"kind": "theme", "resource_key": resourceKey}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	creds := a.Snapshot()
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+desktopAppearanceSet, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", c.desktopUserAgent(a, false))
	req.Header.Set("X-Product", "SaaS")
	if creds.UID != "" {
		req.Header.Set("X-User-Id", creds.UID)
	}
	if c.desktopHeaders() {
		c.commonHeaders(req, a)
	}
	_, err = c.doJSON(req, a)
	return err
}

// DesktopBuddyAppSequence 构造「进入 Buddy 应用」五连事件（实测两账号纯 API 点亮
// Buddy_App 与 Buddy_App_QQ）：discover → show → enter_click → auth_confirm →
// bindaccount_skip。buddyID 固定用企鹅教师助手 cb_y5Dy46tPQGGWtueMxXbe
// （Buddy_App_QQ 的判据应用），同时满足 Buddy_App「进入任一应用」。
func DesktopBuddyAppSequence(buddyID, buddyName string) []DesktopEvent {
	mk := func(code string, extra map[string]any) DesktopEvent {
		ev := DesktopEvent{
			"eventCode": code, "mode": "LOCAL",
			"buddyId": buddyID, "buddyName": buddyName,
		}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	return []DesktopEvent{
		mk("buddyapp_discover_click", nil),
		mk("buddyapp_show", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2}),
		mk("buddyapp_enter_click", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2, "isFirstPage": true}),
		mk("buddyapp_auth_confirm_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
		mk("buddyapp_bindaccount_skip_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
	}
}

// DesktopAutomationCreateEvent 构造「定时任务创建成功」事件（实测两账号纯 API
// 点亮 automation_1）。name 为任务名，可与真实创建语义对齐。
func DesktopAutomationCreateEvent(name string) DesktopEvent {
	return DesktopEvent{
		"eventCode": "automated_task_create_suc", "name": name,
		"source": "manually", "modelId": "fast-model", "modelIsThinking": true,
		"connectorCount": 0, "skills": "", "skillCount": 0,
		"scheduleType": "once", "mode": "LOCAL",
	}
}

// ReportWebEvent 以 Web 端指纹向 www.workbuddy.cn/v2/report 上报单事件。
// 与桌面指纹（copilot 域）不同：web 域事件是浏览器形状（os/machineId/userAgent），
// 用于 Library_read 等页面行为类任务（实测 library_doc_intro_click 4 秒点亮）。
func (c *Client) ReportWebEvent(a *auth.Auth, eventCode, pageURL, elementID, elementName string) error {
	creds := a.Snapshot()
	ua := browserUserAgent(a)
	ev := map[string]any{
		"eventCode": eventCode, "timestamp": time.Now().UnixMilli(), "reportDelay": 0,
		"pageURL": pageURL, "elementId": elementID, "elementName": elementName,
		"os": "Win32", "arch": "", "osVersion": "10.0", "userAgent": ua,
		"machineId": deriveID(a, "webmachine"), "userId": creds.UID,
		"userNickname": creds.Nickname, "enterpriseId": creds.EnterpriseID,
	}
	if a.Device().IsMac() {
		ev["os"], ev["osVersion"] = "macOS", "10.15.7"
	}
	raw, err := json.Marshal([]map[string]any{ev})
	if err != nil {
		return err
	}
	// Cookie 路径也要带真实来源页：资料库等事件的 pageURL 不是成长中心，
	// 之前这一路被 websiteJSON 统一按成长中心生成 Referer。
	if _, handled, err := c.websiteJSON(context.Background(), a, http.MethodPost, "/v2/report", raw, true, pageURL); handled {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.webBase()+"/v2/report", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	c.applyWebHeaders(req, a, "/activity/growth", pageURL)
	req.Header.Set("Content-Type", "application/json")
	_, err = c.doJSON(req, a)
	return err
}

// ---------------------------------------------------------------------------
// 模板 / 灵感案例 / 画布（asar 逆向判据事件，三账号实测点亮）
// ---------------------------------------------------------------------------

// DesktopTemplateUseSequence 构造「使用模板创建任务」事件组（实测 template_5 计数）：
// agent_task_created_with_template {mode,isCustomModel,id,name,requestId} +
// template_used {template_id,task_mode}，JOIN 一条完整 chat 链。
// 三账号实测：5 组（不同模板）一次上报 → 5/5 点亮。
func DesktopTemplateUseSequence(conversationID, requestID, templateID, templateName string) []DesktopEvent {
	events := DesktopChatSequence(conversationID, requestID, NewEventID(), "fast-model", "fast-model")
	events = append(events,
		DesktopEvent{
			"eventCode": "agent_task_created_with_template", "mode": "working",
			"isCustomModel": false, "id": templateID, "name": templateName, "requestId": requestID,
		},
		DesktopEvent{"eventCode": "template_used", "template_id": templateID, "task_mode": "working"},
	)
	return events
}

// DesktopPlaybookPromptSequence 构造「灵感案例做同款」事件组（实测 playbook_prompt 计数）：
// web_element_click(playbook_ctaClick) + playbook_cta_click + playbook_prompt_send
// （Dialog 发送 Prompt，带 conversationId/requestId JOIN chat 链）。三账号实测 1/1 点亮。
func DesktopPlaybookPromptSequence(conversationID, requestID, caseID, caseName string) []DesktopEvent {
	events := DesktopChatSequence(conversationID, requestID, NewEventID(), "fast-model", "fast-model")
	payload := map[string]any{
		"id": caseID, "name": caseName, "type": "document",
		"categoryId": "", "categoryName": "",
	}
	events = append(events,
		DesktopEvent{
			"eventCode": "web_element_click", "pageName": "playbook_detail",
			"elementId": "playbook_ctaClick", "elementName": caseName, "source": "discover",
		},
		DesktopEvent(func() map[string]any {
			m := map[string]any{"eventCode": "playbook_cta_click", "source": "discover", "position": 0}
			for k, v := range payload {
				m[k] = v
			}
			return m
		}()),
		DesktopEvent(func() map[string]any {
			m := map[string]any{"eventCode": "playbook_prompt_send", "conversationId": conversationID, "requestId": requestID}
			for k, v := range payload {
				m[k] = v
			}
			return m
		}()),
	)
	return events
}

// DesktopDesignCanvasSequence 构造「设计创意画布」事件组（实测 create_canvas 计数）：
// wbx_design_canvas_task_create + wbx_design_canvas_open（Ardot create_design 工具
// 完成时客户端经 metrics 通道上报，同一 /v2/report 端点）。三账号实测 1/1 点亮。
func DesktopDesignCanvasSequence(conversationID, requestID string) []DesktopEvent {
	events := DesktopChatSequence(conversationID, requestID, NewEventID(), "fast-model", "fast-model")
	return append(events,
		DesktopEvent{
			"eventCode": "wbx_design_canvas_task_create", "conversationId": conversationID,
			"requestId": requestID, "source": "summon_keyword", "cost": 12000, "isSuccessful": true,
		},
		DesktopEvent{
			"eventCode": "wbx_design_canvas_open", "conversationId": conversationID,
			"requestId": requestID, "id": "ardot-file-" + requestID[len(requestID)-8:],
			"source": "summon_keyword", "type": "page", "cost": 13000, "isSuccessful": true,
		},
	)
}

// ---------------------------------------------------------------------------
// 专家市场 + 真实对话（expert_5 / Expert_team_use_3 / Expert_lighthouse / skill_1）
// ---------------------------------------------------------------------------

// MarketExpert 专家市场的单个专家（/portal/operation-platform/market/expert/list 响应子集）。
type MarketExpert struct {
	IsCloud       int             `json:"is_cloud"`
	NeedPay       int             `json:"need_pay"`
	PayDetail     json.RawMessage `json:"pay_detail"`
	ExpertID      string          `json:"expert_id"`
	ExpertType    string          `json:"expert_type"`
	DisplayNameZH string          `json:"display_name_zh"`
	ProfessionZH  string          `json:"profession_zh"`
	Version       string          `json:"version"`
	Categories    []any           `json:"categories"`
}

// MarketExpertPageSize 专家市场列表每页条数（官方专家页一页 20 条）。
const MarketExpertPageSize = 20

// MarketExpertList 拉取专家市场真实专家列表（expertType: "agent" 单专家 / "team" 专家团）。
// expert_actual_use 的判据校验要求 id 是平台上真实存在的专家（编造 id 不计数）。
func (c *Client) MarketExpertList(a *auth.Auth, expertType string) ([]MarketExpert, error) {
	return c.marketExpertList(a, expertType, "", 1)
}

// MarketExpertListPage 拉取第 page 页；往下翻页只改 page（官方翻页请求未抓包，未确认）。
func (c *Client) MarketExpertListPage(a *auth.Auth, expertType string, page int) ([]MarketExpert, error) {
	if page < 1 {
		page = 1
	}
	return c.marketExpertList(a, expertType, "", page)
}

// MarketExpertSearch 按关键词搜第一页。官方专家页的搜索还是同一个列表请求，只是请求体多
// keyword、请求头多 X-Language（界面语言，默认 zh-CN）；5.8.3 是否也这样没核对。
func (c *Client) MarketExpertSearch(a *auth.Auth, expertType, keyword string) ([]MarketExpert, error) {
	return c.marketExpertList(a, expertType, strings.TrimSpace(keyword), 1)
}

func (c *Client) marketExpertList(a *auth.Auth, expertType, keyword string, page int) ([]MarketExpert, error) {
	creds := a.Snapshot()
	body := map[string]any{"page": page, "page_size": MarketExpertPageSize, "sort_by": "reco_rank", "sort_order": "desc"}
	if expertType != "" {
		body["expert_type"] = expertType
	}
	if keyword != "" {
		body["keyword"] = keyword
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+marketExpertListPath, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.desktopUserAgent(a, false))
	req.Header.Set("X-Domain", c.chatBase(a))
	req.Header.Set("X-Product", "SaaS")
	if creds.UID != "" {
		req.Header.Set("X-User-Id", creds.UID)
	}
	var out struct {
		Items   []MarketExpert `json:"items"`
		Experts []MarketExpert `json:"experts"`
	}
	if c.desktopHeaders() {
		req.Header.Del("X-Domain")
		c.commonHeaders(req, a)
	}
	if keyword != "" {
		req.Header.Set("X-Language", "zh-CN")
	}
	data, err := c.doJSON(req, a)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("expert list parse: %w", err)
	}
	if out.Items != nil {
		return out.Items, nil
	}
	return out.Experts, nil
}

// DesktopChatWithExpert 发一条真实桌面指纹 chat 请求（可带 X-Expert-Id），从 SSE 流
// 解析**服务端返回的 requestId**（data.id，如 cmb-xxxx / 32hex）并返回。
// expert_actual_use 等 JOIN 事件的 requestId 必须是该服务端 id——自造 UUID 不计数
// （客户端 resolveRealRequestId 同款语义）。
//
// 注意：这条对话**不走 prepareBody 改写管线**（无需脱敏——载荷是固定的极短
// 中性内容），也不入账号池租约；与 panel 原实现一致，直接裸发 chatBase。
func (c *Client) DesktopChatWithExpert(a *auth.Auth, expertID string) (conversationID, requestID string, err error) {
	result, err := c.DesktopChatWithExpertObserved(a, expertID)
	return result.ConversationID, result.RequestID, err
}

func (c *Client) DesktopChatWithExpertObserved(a *auth.Auth, expertID string) (result DesktopChatResult, err error) {
	return c.DesktopChatWithExpertInfoObserved(a, MarketExpert{ExpertID: expertID})
}

// The caller must pass the selected market entry. An ID alone does not prove
// team membership, payment or cloud execution, so those flags stay absent.
func (c *Client) DesktopChatWithExpertInfoObserved(a *auth.Auth, expert MarketExpert) (result DesktopChatResult, err error) {
	expertID := expert.ExpertID
	creds := a.Snapshot()
	const prompt = "1+1等于几？直接回答。"
	result = DesktopChatResult{ConversationID: NewEventID(), ModelID: "fast-model", InputLength: utf8.RuneCountInString(prompt)}
	body := map[string]any{
		"model": "fast-model",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant. 当前处于中文环境，使用简体中文回答。"},
			map[string]any{"role": "user", "content": prompt},
		},
		"agent":          "cli",
		"temperature":    1,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return result, err
	}
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+"/v2/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return result, err
	}
	h := req.Header
	h.Set("Authorization", "Bearer "+creds.AccessToken)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	h.Set("User-Agent", c.desktopUserAgent(a, true))
	h.Set("X-Domain", c.chatBase(a))
	h.Set("X-Product", "SaaS")
	h.Set("X-User-Id", creds.UID)
	h.Set("X-Conversation-ID", result.ConversationID)
	h.Set("X-Request-ID", NewEventID())
	h.Set("X-Agent-Intent", "craft")
	h.Set("X-Agent-Type", "main")
	h.Set("X-IDE-Name", "WorkBuddy")
	h.Set("X-IDE-Type", "WorkBuddy")
	h.Set("X-IDE-Version", desktopVersion)
	h.Set("x-codebuddy-request", "1")
	if c.desktopHeaders() {
		c.chatHeaders(req, a)
		h.Del("x-codebuddy-request")
	}
	c.privacyHeaders(req, a)
	if expertID != "" {
		h.Set("X-Expert-Id", expertID)
		if expert.ExpertType == "team" {
			h.Set("X-Expert-Team-Task", "true")
		}
		if expert.requiresProtection() {
			h.Set("X-Moderation-Type", `["protected"]`)
		}
	}
	// 账号级统一路由（reqproxy）：经钩子取该账号 client。
	cli, err := c.proxyClientFor(a)
	if err != nil {
		return result, err // D4：无可用节点，不回退直连
	}
	result.StartedAt = time.Now().UnixMilli()
	resp, err := doUpstream(cli, req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return result, fmt.Errorf("chat http %d: %s", resp.StatusCode, b)
	}
	if id := ResponseIDFromHeader(resp.Header); desktopIDRegex.MatchString(id) {
		result.RequestID = id
	}
	err = readDesktopObservation(resp.Body, &result)
	return result, err
}

// desktopIDRegex 服务端 requestId 形状（cmb- 前缀 32hex 或裸 32hex）。
var desktopIDRegex = regexp.MustCompile(`^(cmb-)?[0-9a-f]{32}$`)

// DesktopExpertSummonSequence 构造「召唤平台专家」事件组（expert_summon_click 等），
// 载荷对齐真实抓包样本。需配合 DesktopChatWithExpert +
// DesktopExpertActualUseEvent 完成一次完整「召唤+使用」。
func DesktopExpertSummonSequence(e MarketExpert) []DesktopEvent {
	cat := "expert-all"
	if len(e.Categories) > 0 {
		if s, ok := e.Categories[0].(string); ok {
			cat = s
		}
	}
	ver := e.Version
	if ver == "" {
		ver = "1.0.0"
	}
	return []DesktopEvent{
		{
			"eventCode": "web_element_click", "source": e.ExpertID, "type": cat, "version": ver,
			"elementId": "expert_summon_click", "elementName": "立即召唤",
			"pageURL": "/C:/Program%20Files/WorkBuddy/resources/app.asar/renderer/index.html",
		},
		{
			"eventCode": "expert_summon_click", "id": e.ExpertID, "name": e.DisplayNameZH,
			"expertTitle": e.ProfessionZH, "type": "expert-all", "position": 0,
			"expertType": e.ExpertType, "version": ver, "mode": "LOCAL",
		},
		{
			"eventCode": "expert_summoned", "id": e.ExpertID, "name": e.DisplayNameZH,
			"expertTitle": e.ProfessionZH, "type": "expert-all",
		},
	}
}

// DesktopExpertActualUseEvent 构造「专家真实使用」事件（expert_5/Expert_team_use_3 计数）。
// requestID 必须是 DesktopChatWithExpert 返回的服务端 requestId。
func DesktopExpertActualUseEvent(e MarketExpert, conversationID, requestID string) DesktopEvent {
	ev := desktopExpertActualUse(e, conversationID, requestID)
	ev["mode"] = "craft"
	return ev
}

// DesktopExpertActualUseLocal mode:"LOCAL" 变体（Expert_lighthouse 判据要求 LOCAL：
// 轻量云专家使用时 mode=LOCAL、type 为空、cost=0）。
func DesktopExpertActualUseLocal(e MarketExpert, conversationID, requestID string) DesktopEvent {
	ev := desktopExpertActualUse(e, conversationID, requestID)
	ev["mode"] = "LOCAL"
	return ev
}

// desktopExpertActualUse expert_actual_use 公共载荷。
func desktopExpertActualUse(e MarketExpert, conversationID, requestID string) DesktopEvent {
	cat := "expert-all"
	if len(e.Categories) > 0 {
		if s, ok := e.Categories[0].(string); ok {
			cat = s
		}
	}
	ver := e.Version
	if ver == "" {
		ver = "1.0.0"
	}
	return DesktopEvent{
		"eventCode": "expert_actual_use",
		"id":        e.ExpertID, "name": e.DisplayNameZH, "expertTitle": e.ProfessionZH,
		"type": cat, "expertType": e.ExpertType, "source": "builtin", "version": ver,
		"cost": 9000, "characterCount": 14,
		"conversationId": conversationID, "requestId": requestID, "messageId": "msg-" + requestID[len(requestID)-8:],
		"requestModelId": "fast-model", "requestModelName": "fast-model",
	}
}
