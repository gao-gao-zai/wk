// school.go 开学季活动（school-season，活动期 2026-09-13 ~ 09-24）纯 API 自动化。
// 移植自 linguo2625469/workbuddy2api-panel（同源 MIT fork），判据为该仓库
// 2026-09-13/14 小程序 MCP 逆向 + 三账号实测结论。
//
// 判据：
//   - share_invite（每日 +100c +1抽奖）：POST /tasks/share-complete {channel:"wechat"}
//     即点亮——纯前端上报，服务端不校验真实分享回执。本模块的主目标。
//   - chat_3_times（每日 +50c +1抽奖）：viewed 激活后 3 条 chat_request_send
//     埋点（conversationId 任意，无需真实会话）。
//   - expert_use（每日 +50c +1抽奖）：viewed 后 mp 事件链（召唤×3 + 对话，
//     开学季分类专家）。
//   - desktop_chat_1_time（单次 +100c +1抽奖）：viewed + 真实 chat + 桌面六事件链。
//   - task_student_verify（+100c）：需微信学生真实认证，不做。
//   - 抽奖：POST /wheel/draw {draw_uuid}（前端生成 uuid，消耗 1 chance）。
//
// 端点基址 billing 域（codebuddy.cn）；信封 {code,msg,data}，code=0 成功。
package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"workbuddy2api/internal/auth"
)

const schoolBase = "/portal/activity/school"

// schoolJSON 学院活动 API 请求（billing 域 + BillingHeaders，剥信封）。
func (c *Client) schoolJSON(a *auth.Auth, method, path string, body map[string]any, out any) error {
	data, err := c.billingJSON(a, method, schoolBase+path, body)
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// SchoolTask 开学季任务条目。
type SchoolTask struct {
	TaskCode    string `json:"task_code"`
	Status      string `json:"status"` // pending | in_progress | completed | claimed
	Progress    int    `json:"progress"`
	TargetCount int    `json:"target_count"`
}

// SchoolTasks 任务列表 + 活动是否在期（不在期自动跳过，无需下线代码）。
func (c *Client) SchoolTasks(a *auth.Auth) ([]SchoolTask, bool, error) {
	var out struct {
		Tasks    []SchoolTask `json:"tasks"`
		InPeriod bool         `json:"in_period"`
	}
	if err := c.schoolJSON(a, http.MethodGet, "/tasks", nil, &out); err != nil {
		return nil, false, err
	}
	return out.Tasks, out.InPeriod, nil
}

// SchoolShareComplete 上报「分享完成」（share_invite 判据，实测即点亮）。
func (c *Client) SchoolShareComplete(a *auth.Auth) error {
	return c.schoolJSON(a, http.MethodPost, "/tasks/share-complete",
		map[string]any{"channel": "wechat"}, nil)
}

// SchoolTaskViewed 标记任务已查看（pending → in_progress）。desktop_chat_1_time
// 等任务的计数前置：必须先激活（in_progress）后的行为才计数（三账号实测）。
func (c *Client) SchoolTaskViewed(a *auth.Auth, taskCode string) error {
	return c.schoolJSON(a, http.MethodPost, "/tasks/"+taskCode+"/viewed", map[string]any{}, nil)
}

// SchoolClaimTask 领取任务奖励（返回获得的抽奖次数）。
func (c *Client) SchoolClaimTask(a *auth.Auth, taskCode string) (chanceGranted int, err error) {
	var out struct {
		ChanceGranted int `json:"chance_granted"`
	}
	if err := c.schoolJSON(a, http.MethodPost, "/tasks/"+taskCode+"/claim", map[string]any{}, &out); err != nil {
		return 0, err
	}
	return out.ChanceGranted, nil
}

// SchoolChances 当前抽奖次数余额。
func (c *Client) SchoolChances(a *auth.Auth) (int, error) {
	var out struct {
		Chance struct {
			Balance int `json:"balance"`
		} `json:"chance"`
	}
	if err := c.schoolJSON(a, http.MethodGet, "/config", nil, &out); err != nil {
		return 0, err
	}
	return out.Chance.Balance, nil
}

// SchoolDraw 抽奖一次，返回奖品描述（prize_code + 积分）。
func (c *Client) SchoolDraw(a *auth.Auth) (string, error) {
	var out struct {
		PrizeCode    string `json:"prize_code"`
		CreditAmount int    `json:"credit_amount"`
	}
	if err := c.schoolJSON(a, http.MethodPost, "/wheel/draw",
		map[string]any{"draw_uuid": clientToken()}, &out); err != nil {
		return "", err
	}
	if out.CreditAmount > 0 {
		return fmt.Sprintf("%s +%dc", out.PrizeCode, out.CreditAmount), nil
	}
	return out.PrizeCode, nil
}

// ---- mp（小程序）指纹上报：chat_3_times / expert_use 判据 ----
// 与成长任务的 CLI/桌面/web 三通道并列的第四通道：www.codebuddy.cn/v2/report
// + 小程序指纹（appservice wQ()+Ao() 对齐，panel 逆向）。

// mpEventBase 小程序埋点公共指纹。
func mpEventBase(a *auth.Auth) map[string]any {
	creds := a.Snapshot()
	return map[string]any{
		"timestamp":    time.Now().UnixMilli(),
		"ideType":      "WorkBuddy_MP",
		"ideVersion":   "2.4.0",
		"extName":      "workbuddy-mp",
		"extVersion":   "2.4.0",
		"product":      "SaaS",
		"ideName":      "wx_app_cloud",
		"platform":     "mini_program",
		"os":           "windows",
		"osVersion":    "11",
		"arch":         "x64",
		"machineId":    deriveID(a, "mpmachine"),
		"timezone":     "Asia/Shanghai",
		"userId":       creds.UID,
		"userNickname": creds.Nickname,
	}
}

// ReportMPEvent 以小程序指纹向 billing 域 /v2/report 批量上报事件。
func (c *Client) ReportMPEvent(a *auth.Auth, events ...map[string]any) error {
	if len(events) == 0 {
		return fmt.Errorf("mp report: no events")
	}
	base := mpEventBase(a)
	arr := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range ev {
			m[k] = v
		}
		arr = append(arr, m)
	}
	// billingJSON 对任意 body 原样序列化（含顶层数组）。
	_, err := c.billingJSON(a, http.MethodPost, "/v2/report", arr)
	return err
}

// SchoolChatTimesEvents 构造一条 chat_request_send 事件（chat_3_times 计数）。
func SchoolChatTimesEvents(conversationID string) map[string]any {
	rid := "wb2api-" + clientToken()
	return map[string]any{
		"eventCode":   "chat_request_send",
		"inputLength": 14, "isPlan": false, "isAutoExecuteTerminal": false,
		"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
		"maxSteps": 500, "temperature": 0, "maxRetries": 0,
		"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
		"codebaseId": "", "mentionContextCount": 0, "command": "",
		"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
		"traceId": rid, "rootRequestId": rid,
		"parentConversationId": conversationID, "conversationId": conversationID,
		"messageId": "msg-" + rid[len(rid)-8:],
		"agentName": "mp", "agentType": "main",
		"codebuddy.session_id":              conversationID,
		"codebuddy.conversation_request_id": rid,
	}
}

// SchoolExpertUseEvents 构造专家召唤+对话事件链（expert_use 判据）。
// expertID/expertName 为开学季分类专家（16-BackToSchool）。
func SchoolExpertUseEvents(expertID, expertName, conversationID string) []map[string]any {
	rid := "wb2api-" + clientToken()
	return []map[string]any{
		{
			"eventCode": "expert_summon_click", "id": expertID, "name": expertID,
			"expertTitle": expertName, "type": "16-BackToSchool", "position": 0,
		},
		{
			"eventCode": "expert_summoned", "id": expertID, "name": expertID,
			"expertTitle": expertName,
		},
		{
			"eventCode": "expert_actual_use", "id": expertID, "name": expertID,
			"expertTitle": expertName, "type": "16-BackToSchool",
			"characterCount": 14, "expertType": "builtin",
		},
		{
			"eventCode":   "chat_request_send",
			"inputLength": 14, "isPlan": false, "isAutoExecuteTerminal": false,
			"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
			"maxSteps": 500, "temperature": 0, "maxRetries": 0,
			"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
			"codebaseId": "", "mentionContextCount": 0, "command": "",
			"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
			"traceId": rid, "rootRequestId": rid,
			"parentConversationId": conversationID, "conversationId": conversationID,
			"messageId": "msg-" + rid[len(rid)-8:],
			"agentName": "mp", "agentType": "main",
			"expertId": expertID, "expertName": expertName,
			"codebuddy.session_id":              conversationID,
			"codebuddy.conversation_request_id": rid,
		},
	}
}

// ---- 我的券码（开学季抽奖抽中的第三方实体券）----

// SchoolVoucher 开学季抽奖抽中的第三方券（KFC/瑞幸/酷狗等）。
type SchoolVoucher struct {
	GrantID   int64  `json:"grant_id"`
	SKUCode   string `json:"sku_code,omitempty"`   // kfc_ice_cream / voucher_luckin / voucher_kugou …
	PrizeName string `json:"prize_name,omitempty"` // 肯德基冰淇淋
	Code      string `json:"code"`                 // 券码本体（复制给店员核销）
	ValidTo   string `json:"valid_to,omitempty"`   // "2026-10-24"
	GrantedAt string `json:"granted_at,omitempty"` // RFC3339
}

// SchoolVouchers 查询账号的开学季券码列表（只读）。
// 抽到积分的记录不在此端点（那是 /rewards 的 type=credit 条目）。
func (c *Client) SchoolVouchers(a *auth.Auth) ([]SchoolVoucher, error) {
	var out struct {
		Items []SchoolVoucher `json:"items"`
	}
	if err := c.schoolJSON(a, http.MethodGet, "/vouchers", nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}
