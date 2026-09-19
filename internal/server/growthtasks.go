// growthtasks.go 成长任务中心：任务列表/接受/领奖的 HTTP 端点 + 「一键完成」动作表。
//
// 设计依据（逆向实测结论，移植自 linguo2625469/workbuddy2api-panel）：
//   - first_buddy（+300 分）：report（前置解锁）→ agreement → buddy/first
//   - chat_5（+100 分）：累计 5 条 chat_request_send 上报
//   - Model_chat_GLM5.2（+100 分）：accept → glm-5.2 真实对话一次 → 上报（模型字段对齐）
//   - RichMeow_Chat：桌面指纹（workbuddy-desktop）完整对话事件链，纯 API 可点亮
//   - Buddy_App / Buddy_App_QQ：buddyapp 五连事件
//   - automation_1：automated_task_create_suc 事件
//   - Library_read：web 域 web_element_click(library_doc_intro_click)
//   - template_5 / playbook_prompt / create_canvas：asar 逆向出的判据事件
//   - expert_5 / Expert_team_use_3：真实专家列表 + 召唤链 + 真实 chat（服务端 requestId）
//   - Hp_Appearance：appearance/set + appearance_skin_apply 事件
//   - skill_1：真实对话 + skill_info 技能加载事件
//   - Expert_lighthouse：轻量云专家召唤+使用链（chat 链带 has_expert）
//   - black_cat：夜猫子窗口内 glm-5.2 对话补足（窗口外提示稍后再试）
//
// 不可自动的 1 项：Expert_Philanthropy（需真实捐款，服务端领奖校验捐赠回执）。
//
// 所有动作幂等：已 claimed/已达标的任务直接跳过，不重复消耗上游配额。
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// ---------------------------------------------------------------------------
// per-account 互斥
// ---------------------------------------------------------------------------

// taskMu/taskLocks 一键完成任务的 per-account 互斥：同一账号的任务动作
// （单任务 / 全量 / 全部账号批跑）同时只允许一条在跑。重复点击直接返回 409
// "仍在执行"，而不是并发跑两遍浪费上游请求（动作虽幂等，expert 系每遍含
// 多次真实对话）。不同账号之间不互斥。TryLock 语义，锁条目常驻（账号数有界）。
type growthTaskLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (g *growthTaskLocks) tryLock(uid string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.locks == nil {
		g.locks = make(map[string]*sync.Mutex)
	}
	m := g.locks[uid]
	if m == nil {
		m = &sync.Mutex{}
		g.locks[uid] = m
	}
	return m.TryLock()
}

func (g *growthTaskLocks) unlock(uid string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if m := g.locks[uid]; m != nil {
		m.Unlock()
	}
}

// ---------------------------------------------------------------------------
// 动作表
// ---------------------------------------------------------------------------

// growthAction 一个可自动化的任务动作。
type growthAction struct {
	TaskCode string // 目标任务 code
	Desc     string // 展示用说明
	Attempt  bool   // true = 尝试型（上游未证实可脚本化，跑了可能不点亮）
	run      func(h *Handler, a *auth.Auth) (string, error)
}

// growthActions 已实现的任务动作表（顺序即执行顺序：先解锁依赖项）。
// first_buddy 依赖活跃上报解锁，故 chat_5/first_buddy 的执行都自带 report 步骤。
var growthActions = []growthAction{
	{TaskCode: "chat_5", Desc: "上报 5 条对话活跃事件（自动补足差额）", run: runChat5},
	{TaskCode: "first_buddy", Desc: "上报解锁 → 同意协议 → 领取第一只 Buddy（+300 分）", run: runFirstBuddy},
	{TaskCode: "Model_chat_GLM5.2", Desc: "接受任务 → glm-5.2 真实对话一次 → 对齐模型上报", run: runModelChat},
	{TaskCode: "RichMeow_Chat", Desc: "桌面指纹事件链上报（纯 API 可点亮）", run: runRichMeow},
	{TaskCode: "Buddy_App", Desc: "上报「进入 Buddy 应用」事件链", run: runBuddyApp},
	// Buddy_App_QQ 与 Buddy_App 共用事件链（判据应用即 QQ 助手）。
	{TaskCode: "Buddy_App_QQ", Desc: "上报「进入企鹅教师助手」事件链", run: runBuddyApp},
	{TaskCode: "automation_1", Desc: "上报「定时任务创建」事件", run: runAutomationCreate},
	{TaskCode: "Library_read", Desc: "上报「读资料库介绍」事件", run: runLibraryRead},
	{TaskCode: "template_5", Desc: "上报「使用模板创建任务」事件组 ×5", run: runTemplateUse},
	{TaskCode: "playbook_prompt", Desc: "上报「灵感案例做同款发送 Prompt」事件组", run: runPlaybookPrompt},
	{TaskCode: "create_canvas", Desc: "上报「设计创意画布创建」事件组（+300 分）", run: runCreateCanvas},
	{TaskCode: "expert_5", Desc: "真实专家召唤+使用链 ×5（含真实短对话）", run: runExpertUse},
	{TaskCode: "Expert_team_use_3", Desc: "真实专家团召唤+使用链 ×3（含真实短对话）", run: runExpertTeamUse},
	{TaskCode: "Hp_Appearance", Desc: "设置主题 API + 皮肤生效事件", run: runAppearance},
	{TaskCode: "skill_1", Desc: "真实对话 + skill_info 技能加载事件", run: runSkillFresh},
	{TaskCode: "Expert_lighthouse", Desc: "真实轻量云专家召唤+使用链（chat 链带 has_expert）", run: runExpertLighthouse},
	{TaskCode: "black_cat", Desc: "夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足", Attempt: true, run: runBlackCat},
}

// growthActionFor 查任务对应的动作；无则返回 nil（不可自动化）。
func growthActionFor(code string) *growthAction {
	for i := range growthActions {
		if growthActions[i].TaskCode == strings.TrimSpace(code) {
			return &growthActions[i]
		}
	}
	return nil
}

// growthActionIndex 任务在 growthActions 中的顺序（全量执行按依赖序排；未知返回大值）。
func growthActionIndex(code string) int {
	for i := range growthActions {
		if growthActions[i].TaskCode == code {
			return i
		}
	}
	return 1 << 20
}

// ---------------------------------------------------------------------------
// 动作实现
// ---------------------------------------------------------------------------

// actionStepGap 行为事件上报后的节流（对齐实测 1.05s 口径，避免上游风控）。测试可置 0。
var actionStepGap = 1050 * time.Millisecond

// expertBatchGap 专家链批次间隔（每条含真实对话，间隔放大防风控）。
var expertBatchGap = 6 * time.Second

// runChat5 完成 chat_5：补足差额条数的活跃上报（5 条上限）。
func runChat5(h *Handler, a *auth.Auth) (string, error) {
	t, err := h.taskByCode(a, "chat_5")
	if err != nil {
		return "", fmt.Errorf("list tasks: %w", err)
	}
	need := int64(5)
	if t != nil {
		if t.Claimed || t.Current >= t.Target {
			return "chat_5 已达标，无需补报", nil
		}
		need = t.Target - t.Current
	}
	for i := int64(0); i < need; i++ {
		cid := fmt.Sprintf("wb2api-chat5-%d-%d", time.Now().UnixMilli(), i)
		if err := h.cfg.Upstream.ReportChatActivity(a, cid, fmt.Sprintf("%s-r%d", cid, i)); err != nil {
			return "", fmt.Errorf("第 %d 条上报失败: %w", i+1, err)
		}
		time.Sleep(actionStepGap)
	}
	return fmt.Sprintf("已上报 %d 条对话活跃事件", need), nil
}

// runFirstBuddy 完成 first_buddy：report（解锁前置）→ agreement → buddy/first。
func runFirstBuddy(h *Handler, a *auth.Auth) (string, error) {
	if err := h.cfg.Upstream.ReportChatActivity(a, fmt.Sprintf("wb2api-adopt-%d", time.Now().UnixMilli()), ""); err != nil {
		return "", fmt.Errorf("解锁上报: %w", err)
	}
	time.Sleep(actionStepGap)
	if err := h.cfg.Upstream.BuddyAgreement(a); err != nil {
		return "", fmt.Errorf("同意协议: %w", err)
	}
	if err := h.cfg.Upstream.BuddyFirst(a); err != nil {
		if upstream.IsBuddyTaskIncomplete(err) {
			return "", fmt.Errorf("领养门槛未达（需先有真实对话记录，明天再试）")
		}
		return "", fmt.Errorf("领养: %w", err)
	}
	return "已领养第一只 Buddy", nil
}

// runModelChat 完成 Model_chat_GLM5.2：accept → glm-5.2 真实对话一次 → 对齐模型上报。
func runModelChat(h *Handler, a *auth.Auth) (string, error) {
	if err := h.cfg.Upstream.AcceptTasks(a, []string{"Model_chat_GLM5.2"}); err != nil {
		log.Printf("growth task Model_chat_GLM5.2 accept: %v", err) // 幂等，失败不阻塞
	}
	ok, err := h.cfg.Upstream.RunNightChats(a, 1)
	if err != nil || ok < 1 {
		return "", fmt.Errorf("glm-5.2 对话失败: %v", err)
	}
	return "已完成 glm-5.2 真实对话并上报", nil
}

// runRichMeow 完成 RichMeow_Chat：桌面指纹完整对话事件链。
func runRichMeow(h *Handler, a *auth.Auth) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-meow-%d", ms)
	req := fmt.Sprintf("wb2api-meow-req-%d", ms)
	events := upstream.DesktopChatSequence(conv, req, "msg-meow", "fast-model", "fast-model")
	if err := h.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", fmt.Errorf("事件链上报: %w", err)
	}
	return "已上报桌面端对话事件链", nil
}

// runBuddyApp 完成 Buddy_App / Buddy_App_QQ：buddyapp 五连事件。
// 判据应用固定用企鹅教师助手（同时满足两任务）。
func runBuddyApp(h *Handler, a *auth.Auth) (string, error) {
	events := upstream.DesktopBuddyAppSequence("cb_y5Dy46tPQGGWtueMxXbe", "企鹅教师助手")
	if err := h.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", fmt.Errorf("事件链上报: %w", err)
	}
	return "已上报进入 Buddy 应用事件链", nil
}

// runAutomationCreate 完成 automation_1：定时任务创建成功事件。
func runAutomationCreate(h *Handler, a *auth.Auth) (string, error) {
	ev := upstream.DesktopAutomationCreateEvent("wb2api-automation")
	if err := h.cfg.Upstream.ReportDesktopEvent(a, ev); err != nil {
		return "", fmt.Errorf("事件上报: %w", err)
	}
	return "已上报定时任务创建事件", nil
}

// runLibraryRead 完成 Library_read：web 域 library_doc_intro_click。
// 判据（panel 三账号实测）：pageURL 必须是真实资料库文档地址（/space/d/...），
// 服务端校验页面路径；用其它 URL 上报 200 但不计分。
func runLibraryRead(h *Handler, a *auth.Auth) (string, error) {
	const pageURL = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"
	if err := h.cfg.Upstream.ReportWebEvent(a, "web_element_click", pageURL,
		"library_doc_intro_click", "WorkBuddy资料库介绍"); err != nil {
		return "", fmt.Errorf("web 事件上报: %w", err)
	}
	return "已上报资料库阅读点击事件", nil
}

// runTemplateUse 完成 template_5：5 组（不同模板）事件链。
func runTemplateUse(h *Handler, a *auth.Auth) (string, error) {
	templates := [][2]string{{"1", "深度研究"}, {"2", "周报生成"}, {"3", "竞品分析"}, {"4", "活动策划"}, {"5", "代码评审"}}
	for i, tp := range templates {
		ms := time.Now().UnixMilli()
		conv := fmt.Sprintf("wb2api-tpl-%d-%d", ms, i)
		req := fmt.Sprintf("wb2api-tpl-req-%d-%d", ms, i)
		events := upstream.DesktopTemplateUseSequence(conv, req, tp[0], tp[1])
		if err := h.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
			return "", fmt.Errorf("第 %d 组上报失败: %w", i+1, err)
		}
		time.Sleep(actionStepGap)
	}
	return "已上报 5 组模板使用事件", nil
}

// runPlaybookPrompt 完成 playbook_prompt：灵感案例「做同款」事件组。
func runPlaybookPrompt(h *Handler, a *auth.Auth) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-pb-%d", ms)
	req := fmt.Sprintf("wb2api-pb-req-%d", ms)
	events := upstream.DesktopPlaybookPromptSequence(conv, req, "pb-1", "用 AI 提升周报效率")
	if err := h.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", fmt.Errorf("事件链上报: %w", err)
	}
	return "已上报灵感案例做同款事件", nil
}

// runCreateCanvas 完成 create_canvas：设计画布创建事件组（Ardot 遥测）。
func runCreateCanvas(h *Handler, a *auth.Auth) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-canvas-%d", ms)
	req := fmt.Sprintf("wb2api-canvas-req-%d", ms)
	events := upstream.DesktopDesignCanvasSequence(conv, req)
	if err := h.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", fmt.Errorf("事件链上报: %w", err)
	}
	return "已上报设计画布创建事件", nil
}

// runExpertUse 完成 expert_5：真实专家召唤+使用链 ×5。
func runExpertUse(h *Handler, a *auth.Auth) (string, error) {
	experts, err := h.cfg.Upstream.MarketExpertList(a, "agent")
	if err != nil {
		return "", fmt.Errorf("专家市场列表: %w", err)
	}
	if len(experts) == 0 {
		return "", fmt.Errorf("专家市场为空")
	}
	n := 5
	if len(experts) < n {
		n = len(experts)
	}
	for i := 0; i < n; i++ {
		if err := h.expertChain(a, experts[i]); err != nil {
			return "", fmt.Errorf("第 %d 位专家: %w", i+1, err)
		}
		time.Sleep(expertBatchGap)
	}
	return fmt.Sprintf("已完成 %d 位专家召唤+使用链", n), nil
}

// runExpertTeamUse 完成 Expert_team_use_3：专家团召唤+使用链 ×3。
func runExpertTeamUse(h *Handler, a *auth.Auth) (string, error) {
	experts, err := h.cfg.Upstream.MarketExpertList(a, "team")
	if err != nil {
		return "", fmt.Errorf("专家团列表: %w", err)
	}
	if len(experts) == 0 {
		return "", fmt.Errorf("专家团列表为空")
	}
	n := 3
	if len(experts) < n {
		n = len(experts)
	}
	for i := 0; i < n; i++ {
		if err := h.expertChain(a, experts[i]); err != nil {
			return "", fmt.Errorf("第 %d 个专家团: %w", i+1, err)
		}
		time.Sleep(expertBatchGap)
	}
	return fmt.Sprintf("已完成 %d 个专家团召唤+使用链", n), nil
}

// expertChain 单专家完整链：召唤事件 → 真实对话（服务端 requestId）→ 使用事件。
func (h *Handler) expertChain(a *auth.Auth, e upstream.MarketExpert) error {
	if err := h.cfg.Upstream.ReportDesktopEvent(a, upstream.DesktopExpertSummonSequence(e)...); err != nil {
		return fmt.Errorf("召唤链: %w", err)
	}
	conv, req, err := h.cfg.Upstream.DesktopChatWithExpert(a, e.ExpertID)
	if err != nil {
		return fmt.Errorf("真实对话: %w", err)
	}
	events := upstream.DesktopChatSequence(conv, req, "msg-"+req[len(req)-8:], "fast-model", "fast-model")
	events = append(events, upstream.DesktopExpertActualUseEvent(e, conv, req))
	return h.cfg.Upstream.ReportDesktopEvent(a, events...)
}

// runAppearance 完成 Hp_Appearance：set API + appearance_skin_apply 事件。
// 判据是 appearance_skin_apply 事件（客户端在主题生效状态下离开设置页时上报），
// 纯 set 不计分——组合：set API 留痕 + 事件上报。
func runAppearance(h *Handler, a *auth.Auth) (string, error) {
	const themeKey = "theme-tkmw7j" // 判据主题
	if err := h.cfg.Upstream.SetAppearanceTheme(a, themeKey); err != nil {
		return "", fmt.Errorf("设置主题: %w", err)
	}
	time.Sleep(2 * time.Second)
	if err := h.cfg.Upstream.ReportDesktopEvent(a, upstream.DesktopEvent{
		"eventCode": "appearance_skin_apply", "action": "apply", "source": "settings_close",
		"id": themeKey, "vipLevel": 0, "series": "", "type": "unknown",
	}); err != nil {
		return "", err
	}
	return "已设置主题并上报皮肤生效事件", nil
}

// runSkillFresh 完成 skill_1：真实对话（finishReason=tool_calls 语义）+ skill_info 事件。
func runSkillFresh(h *Handler, a *auth.Auth) (string, error) {
	conv, req, err := h.cfg.Upstream.DesktopChatWithExpert(a, "")
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}
	msgID := "msg-" + req[len(req)-8:]
	events := upstream.DesktopChatSequence(conv, req, msgID, "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "chat_message_response" {
			ev["finishReason"] = "tool_calls" // 模型发起工具调用（技能加载）语义
		}
	}
	events = append(events, upstream.DesktopEvent{
		"eventCode":      "skill_info",
		"id":             "润泽小馆·日报撰写",
		"skillId":        "skill_2097350077599879168",
		"skillVersion":   "1.0.0",
		"toolStatus":     "success",
		"fileCount":      56,
		"source":         "workbuddy-desktop",
		"conversationId": conv, "requestId": req, "messageId": msgID,
		"requestModelId": "fast-model", "requestModelName": "fast-model",
		"traceId": req,
	})
	if err := h.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", fmt.Errorf("skill_info 事件: %w", err)
	}
	return "已上报真实对话 + skill_info 技能加载事件", nil
}

// runExpertLighthouse 完成 Expert_lighthouse（体验「腾讯轻量云」专家）。
// 与 expert_5 同构，但 chat 链的 agent_task_created 需带 has_expert:true +
// expert_id，expert_actual_use 的 mode 为 "LOCAL"（非 craft）。
func runExpertLighthouse(h *Handler, a *auth.Auth) (string, error) {
	const lhID = "ex_2cvvUZQhDyeJ"
	lh := upstream.MarketExpert{
		ExpertID: lhID, ExpertType: "agent",
		DisplayNameZH: "腾讯轻量云专家", ProfessionZH: "腾讯轻量云专家", Version: "1.0.2",
	}
	// 市场列表若命中真实条目则用其信息（version 等以服务端为准）。
	if experts, err := h.cfg.Upstream.MarketExpertList(a, "agent"); err == nil {
		for _, e := range experts {
			if e.ExpertID == lhID {
				lh = e
				break
			}
		}
	}
	if err := h.cfg.Upstream.ReportDesktopEvent(a, upstream.DesktopExpertSummonSequence(lh)...); err != nil {
		return "", fmt.Errorf("召唤链: %w", err)
	}
	conv, req, err := h.cfg.Upstream.DesktopChatWithExpert(a, lhID)
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}
	events := upstream.DesktopChatSequence(conv, req, "msg-"+req[len(req)-8:], "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "agent_task_created" {
			ev["has_expert"] = true
			ev["expert_id"] = lh.ExpertID
			ev["expert_name"] = lh.DisplayNameZH
			ev["expert_industry_id"] = ""
		}
	}
	events = append(events, upstream.DesktopExpertActualUseLocal(lh, conv, req))
	// 对齐真实样本细节：轻量云专家 actual_use 的 type 为空、cost=0。
	events[len(events)-1]["type"] = ""
	events[len(events)-1]["cost"] = 0
	if err := h.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", fmt.Errorf("使用事件: %w", err)
	}
	return "已上报轻量云专家召唤+使用链（真实对话 requestId）", nil
}

// runBlackCat 完成 black_cat：窗口内对话补足。
// 判据（panel 实测口径）：**每夜只计 1 次**，target=3 是 3 个夜晚累计——
// 一晚连发多次只有第一次计分。所以这里只跑 1 次，剩余天数靠每日 23 点的
// blackcat 排程逐夜补足（漏跑次日窗口自动补）。
func runBlackCat(h *Handler, a *auth.Auth) (string, error) {
	if !upstream.InNightWindow(time.Now()) {
		return "", fmt.Errorf("夜猫子任务需在 23:00–08:00 窗口内完成，当前不在窗口")
	}
	need, err := h.cfg.Upstream.BlackcatNeed(a)
	if err != nil {
		return "", fmt.Errorf("查任务差额: %w", err)
	}
	if need <= 0 {
		return "夜猫子任务已达标", nil
	}
	if _, err := h.cfg.Upstream.RunNightChats(a, 1); err != nil {
		return "", fmt.Errorf("夜间对话失败: %w", err)
	}
	if need > 1 {
		return fmt.Sprintf("今夜已计 1 次（剩余 %d 夜由每日 23 点排程自动补足）", need-1), nil
	}
	return "已完成今夜的 1 次夜间对话", nil
}

// ---------------------------------------------------------------------------
// 任务查询 / 回读 / 自动领奖
// ---------------------------------------------------------------------------

// taskByCode 拉取任务列表并定位单个任务；未找到返回 nil（不视为错误）。
func (h *Handler) taskByCode(a *auth.Auth, code string) (*upstream.Task, error) {
	tasks, err := h.cfg.Upstream.ListTasks(a)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i], nil
		}
	}
	return nil, nil
}

// claimPollAttempts / claimPollGap 达标回读的有界轮询参数。
// 背景：上游计分是**异步**的——行为事件上报后进度要数秒才刷新（实测对话完成
// 后立即回读仍是 0/1，约 5-8 秒后才变 1/1）。一次性回读会误判"未达标"，
// 从而跳过自动领奖。这里最多轮询 N 次、每次间隔 gap，总预算约 12 秒。
var (
	claimPollAttempts = 4
	claimPollGap      = 3 * time.Second
)

// taskByCodeWaiting 回读任务，若未达标则在有界预算内轮询等待（上游异步计分）。
// 已达标（claimable）立即返回；预算耗尽返回最后一次结果（可能仍未达标）。
func (h *Handler) taskByCodeWaiting(a *auth.Auth, code string) (*upstream.Task, error) {
	t, err := h.taskByCode(a, code)
	if err != nil || t == nil {
		return t, err
	}
	if t.Claimable || t.Claimed {
		return t, nil
	}
	for i := 1; i < claimPollAttempts; i++ {
		time.Sleep(claimPollGap)
		t2, err2 := h.taskByCode(a, code)
		if err2 != nil {
			return t, nil // 轮询期间的查询失败不覆盖已拿到的结果
		}
		if t2 != nil {
			t = t2
			if t.Claimable || t.Claimed {
				return t, nil
			}
		}
	}
	return t, nil
}

// taskProgressText 任务进度的可读表示（回读对比用）。
func taskProgressText(t *upstream.Task) string {
	if t == nil {
		return "?"
	}
	if t.Target > 0 {
		return fmt.Sprintf("%d/%d", t.Current, t.Target)
	}
	if t.Claimed {
		return "claimed"
	}
	return t.AcceptStatus
}

// ---------------------------------------------------------------------------
// HTTP 端点
// ---------------------------------------------------------------------------

// growthAccount 取账号凭证并校验区域；不存在/海外版时写错误响应并返回 nil。
func (h *Handler) growthAccount(w http.ResponseWriter, uid string) *auth.Auth {
	a := h.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号不存在"})
		return nil
	}
	if a.Region() == "global" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "海外版账号无成长任务体系"})
		return nil
	}
	return a
}

// accountTasks 查询单账号全量任务（进度/状态/可领取/是否可自动完成）。
func (h *Handler) accountTasks(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := h.growthAccount(w, uid)
	if a == nil {
		return
	}
	tasks, err := h.cfg.Upstream.ListTasks(a)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "list tasks: " + err.Error()})
		return
	}
	// 附注可自动化性（前端展示「一键完成」按钮 or 操作指引）。
	autoable := make(map[string]bool, len(growthActions))
	for _, act := range growthActions {
		autoable[act.TaskCode] = true
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tasks": tasks, "auto_actions": autoable})
}

// accountTaskAccept 接受任务（报名；幂等）。
func (h *Handler) accountTaskAccept(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := h.growthAccount(w, uid)
	if a == nil {
		return
	}
	var body struct {
		TaskCodes []string `json:"task_codes"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || len(body.TaskCodes) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task_codes required"})
		return
	}
	if err := h.cfg.Upstream.AcceptTasks(a, body.TaskCodes); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "accept: " + err.Error()})
		return
	}
	log.Printf("growth: 接受任务 uid=%s codes=%v", uid, body.TaskCodes)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountTaskAcceptAll 接受该账号全部尚未接受的任务（跳过已 accepted/claimed/locked）。
// 批量提交分片（保守每批 20 个），批间节流防风控。
func (h *Handler) accountTaskAcceptAll(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := h.growthAccount(w, uid)
	if a == nil {
		return
	}
	tasks, err := h.cfg.Upstream.ListTasks(a)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "list tasks: " + err.Error()})
		return
	}
	var codes []string
	for _, t := range tasks {
		if t.Claimed || t.Locked || t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
			continue
		}
		codes = append(codes, t.TaskCode)
	}
	if len(codes) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accepted": 0, "message": "所有任务均已接受"})
		return
	}
	const batch = 20
	accepted := 0
	var failed []string
	for i := 0; i < len(codes); i += batch {
		end := i + batch
		if end > len(codes) {
			end = len(codes)
		}
		if err := h.cfg.Upstream.AcceptTasks(a, codes[i:end]); err != nil {
			log.Printf("growth: 批量接受失败 uid=%s codes=%v err=%v", uid, codes[i:end], err)
			failed = append(failed, codes[i:end]...)
			continue
		}
		accepted += end - i
		time.Sleep(actionStepGap)
	}
	log.Printf("growth: 全部接受 uid=%s 接受=%d 失败=%d", uid, accepted, len(failed))
	resp := map[string]any{"ok": true, "accepted": accepted, "failed": failed}
	if len(failed) > 0 {
		resp["message"] = "部分任务接受失败（上游拒绝），可重试"
	}
	writeJSON(w, http.StatusOK, resp)
}

// accountTaskClaim 领取任务奖励（未达标时上游返回业务错误，原样透出给前端提示）。
func (h *Handler) accountTaskClaim(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := h.growthAccount(w, uid)
	if a == nil {
		return
	}
	var body struct {
		TaskCode string `json:"task_code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || body.TaskCode == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task_code required"})
		return
	}
	credit, energy, err := h.cfg.Upstream.ClaimReward(a, body.TaskCode)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "claim: " + err.Error()})
		return
	}
	if credit == 0 && energy == 0 {
		log.Printf("growth: 领取任务奖励 uid=%s code=%s（已领取过，无新增）", uid, body.TaskCode)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "already_claimed": true, "message": "该奖励此前已领取"})
		return
	}
	log.Printf("growth: 领取任务奖励 uid=%s code=%s +%d分 +%d能", uid, body.TaskCode, credit, energy)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credit": credit, "energy": energy})
}

// accountTaskAuto 一键完成单个任务：执行对应动作 → 回读进度 → 达标自动领奖。
func (h *Handler) accountTaskAuto(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := h.growthAccount(w, uid)
	if a == nil {
		return
	}
	var body struct {
		TaskCode string `json:"task_code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || body.TaskCode == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task_code required"})
		return
	}
	act := growthActionFor(body.TaskCode)
	if act == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "该任务需要客户端内交互（无对应接口），无法自动完成；请按任务说明在官方客户端操作",
		})
		return
	}
	if !h.growthLocks.tryLock(uid) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "该账号有任务动作正在执行中，请等本轮结束后再试"})
		return
	}
	defer h.growthLocks.unlock(uid)
	resp := h.runGrowthAction(a, act)
	writeJSON(w, http.StatusOK, resp)
}

// runGrowthAction 执行单个动作并组装响应（回读 + 自动领奖）。
// 调用方必须已持有该账号的 growth 锁。
func (h *Handler) runGrowthAction(a *auth.Auth, act *growthAction) map[string]any {
	uid := a.Snapshot().UID
	// 前置读取：已完成的任务直接跳过（幂等，不浪费上游调用）。
	before, err := h.taskByCode(a, act.TaskCode)
	if err != nil {
		return map[string]any{"ok": false, "error": "list tasks: " + err.Error()}
	}
	if before == nil {
		return map[string]any{"ok": false, "error": "该账号没有此任务"}
	}
	if before.Claimed {
		return map[string]any{"ok": true, "skipped": true, "message": "该任务已领取过奖励"}
	}
	msg, err := act.run(h, a)
	if err != nil {
		return map[string]any{"ok": false, "error": "执行失败: " + err.Error()}
	}
	// 回读验证：上报 200 ≠ 计分（上游可能静默丢弃 + 计分异步），
	// 用有界轮询等异步计分落定，再决定是否自动领奖。
	after, aerr := h.taskByCodeWaiting(a, act.TaskCode)
	progressBefore, progressAfter := taskProgressText(before), ""
	claimable := false
	if aerr == nil && after != nil {
		progressAfter = taskProgressText(after)
		claimable = after.Claimable
	}
	resp := map[string]any{
		"ok":              true,
		"message":         msg,
		"progress_before": progressBefore,
		"progress_after":  progressAfter,
		"claimable":       claimable,
		"attempt":         act.Attempt,
	}
	// 达标即自动领奖（Web 端 claim）：把"完成→领奖"收敛成一步。
	if claimable {
		if credit, energy, cerr := h.cfg.Upstream.ClaimReward(a, act.TaskCode); cerr == nil {
			resp["claimed"] = true
			resp["credit"] = credit
			resp["energy"] = energy
			// 持久台账：所有执行路径（单任务/一键/批跑/新号钩子）的领取
			// 都汇聚到这一处，记一次即可全局覆盖。
			creds := a.Snapshot()
			h.growthLedger.record(creds.UID, creds.Nickname, act.TaskCode, credit, energy)
			if credit > 0 || energy > 0 {
				resp["message"] = msg + fmt.Sprintf("；已自动领奖 +%d 分 +%d 能", credit, energy)
			} else {
				resp["message"] = msg + "；奖励此前已领取"
			}
		} else {
			resp["claim_error"] = cerr.Error()
			resp["message"] = msg + "；达标但领奖失败，可在任务列表手动点「领取」重试"
		}
	}
	log.Printf("growth: 任务动作 uid=%s code=%s progress %s -> %s claimable=%v claimed=%v",
		uid, act.TaskCode, progressBefore, progressAfter, claimable, resp["claimed"])
	return resp
}

// accountTaskAutoAll 一键完成该账号全部可自动任务（17 项，依赖序 + 逐项自动领奖）。
// 异步 job 化：立即返回 202 + job_id，进度走 GET /admin/growth/jobs/{id} 轮询。
// 用户可以关页面——任务在后台跑完，结果保留 2 小时可回看。
func (h *Handler) accountTaskAutoAll(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := h.growthAccount(w, uid)
	if a == nil {
		return
	}
	if !h.growthLocks.tryLock(uid) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "该账号有任务动作正在执行中，请等本轮结束后再试"})
		return
	}
	job := h.growthJobs.newJob(uid, "account")
	go func() {
		defer h.growthLocks.unlock(uid) // 流水线真正结束才放锁
		h.runGrowthAllWithJob(a, job)
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "job_id": job.ID, "message": "任务已开始，可关闭页面",
	})
}

// growthJobStatus 查询单个任务进度（幂等，可反复轮询）。
func (h *Handler) growthJobStatus(w http.ResponseWriter, r *http.Request) {
	job := h.growthJobs.get(r.PathValue("id"))
	if job == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "任务不存在（可能已过期清理）"})
		return
	}
	writeJSON(w, http.StatusOK, job.snapshot())
}

// growthJobList 列出进行中的任务（页面加载时恢复进度条）。
func (h *Handler) growthJobList(w http.ResponseWriter, r *http.Request) {
	jobs := h.growthJobs.active()
	out := make([]map[string]any, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.snapshot())
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "jobs": out})
}

// runGrowthAllWithJob 带进度上报的单账号全量流水线（account 模式）。
// 每项开始时 noteCurrent（当前执行到哪儿）、结束时 finishItem（明细实时可见）。
func (h *Handler) runGrowthAllWithJob(a *auth.Auth, job *growthJob) {
	uid := a.Snapshot().UID
	job.noteCurrent(uid + "｜拉取任务列表")
	items, ok := h.runGrowthAllCollect(a, func(code, desc string) {
		job.noteCurrent(uid + "｜" + code)
	})
	if !ok {
		job.finish("failed", "list tasks: 任务列表拉取失败")
		return
	}
	// total = 报名 1 项 + 实际任务项数，让进度条有确定分母。
	job.setTotal(1 + len(items))
	job.finishItem(growthJobItem{TaskCode: "accept", Desc: "任务报名", OK: true})
	for _, item := range items {
		job.finishItem(item)
	}
	job.finish("done", "")
	log.Printf("growth: 一键完成可自动任务 uid=%s 共 %d 项", uid, len(items))
}

// allAccountsTaskAutoAll 对全部非禁用 CN 账号串行执行一键完成（批跑）。
// 同样 job 化：立即返回 202 + job_id；进度 = 账号粒度（当前跑到哪个账号）。
// 单账号失败不影响后续；账号间共享的 expert 链节流照常生效。
func (h *Handler) allAccountsTaskAutoAll(w http.ResponseWriter, r *http.Request) {
	var accounts []accountJobType
	for _, st := range h.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := h.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.Snapshot().AccessToken == "" {
			continue
		}
		if a.Region() == "global" {
			continue
		}
		accounts = append(accounts, accountJobType{st.UID, a})
	}
	if len(accounts) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "没有可执行的账号"})
		return
	}
	pending := make([]string, 0, len(accounts))
	for _, aj := range accounts {
		pending = append(pending, aj.uid)
	}
	job := h.growthJobs.newJob("", "all")
	// 进度分母 = 账号总数（启动即知），分子 = 已完成账号数。任务项明细进
	// results 但不参与 done/total——否则 total 只能逐账号累加，进度条全程
	// 显示 0/? 或追赶态，用户看不出"85 个账号跑到第几个"。
	job.setTotal(len(accounts))
	go h.runAllAccountsPipeline(job, accounts, pending)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "job_id": job.ID, "total_accounts": len(accounts),
		"message": "批跑已开始，可关闭页面",
	})
}

// runAllAccountsPipeline 批跑主管线（端点触发与重启恢复共用）。
// accounts 为本轮要跑的账号；pendingUIDs 为落盘标记的待跑清单（每完成一个
// 划掉一个，全部结束删除标记；nil = 不落盘）。
func (h *Handler) runAllAccountsPipeline(job *growthJob, accounts []accountJobType, pendingUIDs []string) {
	pending := pendingUIDs
	h.writeGrowthJobMark(pending)
	for _, aj := range accounts {
		job.noteCurrent("账号 " + aj.uid)
		if !h.growthLocks.tryLock(aj.uid) {
			job.finishItem(growthJobItem{UID: aj.uid, OK: false, Message: "该账号有任务动作正在执行中，跳过"})
			job.accountDone()
		} else {
			h.runGrowthAllForJobAll(aj.a, job)
			h.growthLocks.unlock(aj.uid)
		}
		// 标记划账：完成的账号从待跑清单移除（重启恢复只重跑剩余的）。
		if len(pending) > 0 {
			pending = pending[1:]
			h.writeGrowthJobMark(pending)
		}
	}
	if len(pending) == 0 {
		h.clearGrowthJobMark()
	}
	job.finish("done", "")
	log.Printf("growth: 全部账号一键完成 共 %d 个账号", len(accounts))
}

// runGrowthAllForJobAll 单账号全量流水线，结果逐项合入 all 模式的 job
// （进度按账号粒度推进：本函数结束 = done+1；任务项明细进 results 但
// 不计入 done/total，明细条目带 uid 区分账号）。
func (h *Handler) runGrowthAllForJobAll(a *auth.Auth, job *growthJob) {
	uid := a.Snapshot().UID
	items, ok := h.runGrowthAllCollect(a, func(code, desc string) {
		job.noteCurrent(uid + "｜" + code)
	})
	if !ok {
		job.finishItem(growthJobItem{UID: uid, OK: false, Message: "任务列表拉取失败"})
	} else {
		for _, item := range items {
			item.UID = uid
			job.appendResult(item)
		}
	}
	job.accountDone()
}

// runGrowthAllCollect 带逐项回调的全量流水线（all 模式复用；onStart 在每项
// 动作开始前调用一次）。返回 (明细, 列表拉取是否成功)。
func (h *Handler) runGrowthAllCollect(a *auth.Auth, onStart func(code, desc string)) ([]growthJobItem, bool) {
	uid := a.Snapshot().UID
	tasks, err := h.cfg.Upstream.ListTasks(a)
	if err != nil {
		return nil, false
	}
	byCode := make(map[string]*upstream.Task, len(tasks))
	for i := range tasks {
		byCode[tasks[i].TaskCode] = &tasks[i]
	}
	var codes []string
	for _, t := range tasks {
		if t.Claimed || t.Locked || t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
			continue
		}
		codes = append(codes, t.TaskCode)
	}
	if len(codes) > 0 {
		if err := h.cfg.Upstream.AcceptTasks(a, codes); err != nil {
			log.Printf("growth: 批量接受失败 uid=%s: %v", uid, err)
		}
		time.Sleep(actionStepGap)
	}
	var items []growthJobItem
	for i := range growthActions {
		act := &growthActions[i]
		t := byCode[act.TaskCode]
		if t == nil {
			continue
		}
		if t.Claimed {
			items = append(items, growthJobItem{
				TaskCode: act.TaskCode, Desc: act.Desc, OK: true, Skipped: true, Message: "已领取过奖励",
			})
			continue
		}
		if onStart != nil {
			onStart(act.TaskCode, act.Desc)
		}
		res := h.runGrowthAction(a, act)
		item := growthJobItem{TaskCode: act.TaskCode, Desc: act.Desc}
		if ok, _ := res["ok"].(bool); ok {
			item.OK = true
			if sk, _ := res["skipped"].(bool); sk {
				item.Skipped = true
			}
			if cl, _ := res["claimed"].(bool); cl {
				item.Claimed = true
			}
		}
		item.Message, _ = res["message"].(string)
		if item.Message == "" {
			if e, _ := res["error"].(string); e != "" {
				item.Message = e
			}
		}
		items = append(items, item)
		time.Sleep(actionStepGap)
	}
	return items, true
}

// runTravel / runActivity 手动触发旅行巡检 / 活跃上报（异步起跑，立即返回）。
func (h *Handler) runTravel(w http.ResponseWriter, r *http.Request) {
	if h.cfg.TravelNow == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "旅行排程未启用"})
		return
	}
	go h.cfg.TravelNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "旅行巡检已启动"})
}

func (h *Handler) runActivity(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ActivityNow == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "活跃上报排程未启用"})
		return
	}
	h.cfg.ActivityNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "活跃上报已启动"})
}

// runSchool 手动触发开学季活动闭环（异步起跑，立即返回）。
func (h *Handler) runSchool(w http.ResponseWriter, r *http.Request) {
	if h.cfg.SchoolNow == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "开学季排程未启用"})
		return
	}
	go h.cfg.SchoolNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "开学季活动闭环已启动"})
}

// schoolStatus 开学季状态：并发拉各账号任务矩阵 + 剩余抽奖次数 + 券码。
// 供前端状态卡展示（只读；活动期外 in_period=false 全量返回）。
func (h *Handler) schoolStatus(w http.ResponseWriter, r *http.Request) {
	type accountStatus struct {
		UID       string                 `json:"uid"`
		Nickname  string                 `json:"nickname,omitempty"`
		InPeriod  bool                   `json:"in_period"`
		Tasks     []upstream.SchoolTask  `json:"tasks,omitempty"`
		Chances   int                    `json:"chances"`
		Vouchers  []upstream.SchoolVoucher `json:"vouchers,omitempty"`
		Error     string                 `json:"error,omitempty"`
	}
	var accounts []*auth.Auth
	for _, st := range h.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := h.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.Snapshot().RefreshToken == "" || a.Region() == "global" {
			continue
		}
		accounts = append(accounts, a)
	}
	out := make([]accountStatus, len(accounts))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, a := range accounts {
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			creds := a.Snapshot()
			st := accountStatus{UID: creds.UID, Nickname: creds.Nickname}
			tasks, inPeriod, err := h.cfg.Upstream.SchoolTasks(a)
			if err != nil {
				st.Error = err.Error()
				out[i] = st
				return
			}
			st.InPeriod = inPeriod
			st.Tasks = tasks
			if chances, err := h.cfg.Upstream.SchoolChances(a); err == nil {
				st.Chances = chances
			}
			if vouchers, err := h.cfg.Upstream.SchoolVouchers(a); err == nil && len(vouchers) > 0 {
				st.Vouchers = vouchers
			}
			out[i] = st
		}(i, a)
	}
	wg.Wait()
	// 汇总：在期账号数 + 全池待办数（供前端一眼看全貌）。
	inPeriod, pending := 0, 0
	for _, st := range out {
		if st.InPeriod {
			inPeriod++
		}
		for _, t := range st.Tasks {
			if t.TaskCode == "task_student_verify" {
				continue // 学生认证不做
			}
			if t.Status != "claimed" && t.Status != "completed" {
				pending++
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "in_period_accounts": inPeriod, "pending_tasks": pending,
		"accounts": out,
	})
}
