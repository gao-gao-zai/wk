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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/collection"
	"workbuddy2api/internal/lifecycle"
	"workbuddy2api/internal/upstream"
)

// ---------------------------------------------------------------------------
// per-account 互斥
// ---------------------------------------------------------------------------

// taskMu/taskLocks 一键完成任务的 per-account 互斥：同一账号的任务动作
// （单任务 / 全量 / 全部账号批跑）同时只允许一条在跑。重复点击直接返回 409
// "仍在执行"，而不是并发跑两遍浪费上游请求（动作虽幂等，expert 系每遍含
// 多次真实对话）。不同账号的此锁独立；连接 IP 的互斥由统一执行池负责。
// TryLock 语义，锁条目常驻（账号数有界）。
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

// growthActions 仅注册当前仍返回、已有实现的三个任务；旧动作实现暂时留存。
// first_buddy 自带必要的解锁上报，不接受或领取已移除的 chat_5。
var growthActions = []growthAction{
	{TaskCode: "first_buddy", Desc: "上报解锁 → 同意协议 → 领取第一只 Buddy（+300 分）", run: runFirstBuddy},
	{TaskCode: "RichMeow_Chat", Desc: "桌面指纹事件链上报（纯 API 可点亮）", run: runRichMeow},
	{TaskCode: "Library_read", Desc: "上报「读资料库介绍」事件", run: runLibraryRead},
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
		cid := upstream.NewEventID()
		if err := h.cfg.Upstream.ReportChatActivity(a, cid, upstream.NewEventID()); err != nil {
			return "", fmt.Errorf("第 %d 条上报失败: %w", i+1, err)
		}
		time.Sleep(actionStepGap)
	}
	return fmt.Sprintf("已上报 %d 条对话活跃事件", need), nil
}

// runFirstBuddy 完成 first_buddy：report（解锁前置）→ agreement → buddy/first。
func runFirstBuddy(h *Handler, a *auth.Auth) (string, error) {
	if err := h.cfg.Upstream.ReportChatActivity(a, upstream.NewEventID(), ""); err != nil {
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
	conv := upstream.NewEventID()
	req := upstream.NewEventID()
	events := upstream.DesktopChatSequence(conv, req, upstream.NewEventID(), "fast-model", "fast-model")
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
	ev := upstream.DesktopAutomationCreateEvent("定时任务")
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
		conv := upstream.NewEventID()
		req := upstream.NewEventID()
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
	conv := upstream.NewEventID()
	req := upstream.NewEventID()
	events := upstream.DesktopPlaybookPromptSequence(conv, req, "pb-1", "用 AI 提升周报效率")
	if err := h.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", fmt.Errorf("事件链上报: %w", err)
	}
	return "已上报灵感案例做同款事件", nil
}

// runCreateCanvas 完成 create_canvas：设计画布创建事件组（Ardot 遥测）。
func runCreateCanvas(h *Handler, a *auth.Auth) (string, error) {
	conv := upstream.NewEventID()
	req := upstream.NewEventID()
	events := upstream.DesktopDesignCanvasSequence(conv, req)
	if err := h.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", fmt.Errorf("事件链上报: %w", err)
	}
	return "已上报设计画布创建事件", nil
}

// runExpertUse 完成 expert_5：真实专家召唤+使用链 ×5。
func runExpertUse(h *Handler, a *auth.Auth) (string, error) {
	experts, err := taskMarketExperts(h.cfg.Upstream, a, "agent", 5, nil)
	if err != nil {
		return "", fmt.Errorf("专家市场列表: %w", err)
	}
	n := len(experts)
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
	experts, err := taskMarketExperts(h.cfg.Upstream, a, "team", 3, nil)
	if err != nil {
		return "", fmt.Errorf("专家团列表: %w", err)
	}
	n := len(experts)
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
	observed, err := h.cfg.Upstream.DesktopChatWithExpertInfoObserved(a, e)
	if err != nil {
		return fmt.Errorf("真实对话: %w", err)
	}
	events := observed.Events()
	events = append(events, observed.ObserveEvents([]upstream.DesktopEvent{
		upstream.DesktopExpertActualUseEvent(e, observed.ConversationID, observed.RequestID),
	})...)
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
	observed, err := h.cfg.Upstream.DesktopChatWithExpertObserved(a, "")
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}
	conv, req := observed.ConversationID, observed.RequestID
	msgID := "msg-" + req[len(req)-8:]
	events := observed.Events()
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
	lh, err := taskLighthouseExpert(h.cfg.Upstream, a)
	if err != nil {
		return "", fmt.Errorf("查找轻量云专家: %w", err)
	}
	if err := h.cfg.Upstream.ReportDesktopEvent(a, upstream.DesktopExpertSummonSequence(lh)...); err != nil {
		return "", fmt.Errorf("召唤链: %w", err)
	}
	observed, err := h.cfg.Upstream.DesktopChatWithExpertInfoObserved(a, lh)
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}
	conv, req := observed.ConversationID, observed.RequestID
	events := observed.Events()
	for _, ev := range events {
		if ev["eventCode"] == "agent_task_created" {
			ev["has_expert"] = true
			ev["expert_id"] = lh.ExpertID
			ev["expert_name"] = lh.DisplayNameZH
			ev["expert_industry_id"] = ""
		}
	}
	events = append(events, observed.ObserveEvents([]upstream.DesktopEvent{
		upstream.DesktopExpertActualUseLocal(lh, conv, req),
	})...)
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
	var tasks []upstream.Task
	var err error
	var snapshot *collection.Snapshot
	if h.cfg.Collection != nil {
		snap := h.cfg.Collection.Snapshot(uid, "tasks")
		_ = json.Unmarshal(snap.Data, &tasks)
		snap.Data = nil
		snapshot = &snap
	} else if !h.cfg.CollectionRequired {
		tasks, err = h.cfg.Upstream.ListTasks(a)
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "list tasks: " + err.Error()})
		return
	}
	// 附注可自动化性（前端展示「一键完成」按钮 or 操作指引）。
	autoable := make(map[string]bool, len(growthActions))
	for _, act := range growthActions {
		autoable[act.TaskCode] = true
	}
	// Collection may still hold snapshots from before tasks were retired.
	current := make([]upstream.Task, 0, len(tasks))
	for _, task := range tasks {
		if upstream.IsCurrentGrowthTask(task.TaskCode) {
			current = append(current, task)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tasks": current, "auto_actions": autoable, "snapshot": snapshot})
}

// growthTaskHTTP preserves synchronous management responses while all mutations
// share the runner's UID/connection-IP reservations. Callbacks never retain w.
func (h *Handler) growthTaskHTTP(w http.ResponseWriter, r *http.Request, a *auth.Auth, desc string, run func(*auth.Auth) (int, map[string]any)) {
	uid := a.Snapshot().UID
	guard := h.growthAuthGuard(a)
	// This is only a compatibility probe; the runner owns the actual lock.
	if !h.growthLocks.tryLock(uid) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "该账号有任务动作正在执行中，请等本轮结束后再试"})
		return
	}
	h.growthLocks.unlock(uid)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	job := h.growthJobs.newJob(uid, "task")
	type response struct {
		status int
		body   map[string]any
	}
	result := make(chan response, 1)
	if !h.growthSubmitTask(ctx, job, a, func(scoped *auth.Auth) {
		status, body := run(scoped)
		if ctx.Err() == nil && guard() == nil {
			h.collectAfterAction(uid)
		}
		job.setTotal(1)
		job.finishItem(growthJobItem{UID: uid, Desc: desc, OK: status < 400 && body["ok"] != false, Skipped: body["skipped"] == true})
		result <- response{status: status, body: body}
	}) {
		job.finish("failed", "该账号已在执行队列中")
		writeJSON(w, http.StatusConflict, map[string]string{"error": "该账号有任务动作正在排队或执行中"})
		return
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case resp := <-result:
			writeJSON(w, resp.status, resp.body)
			return
		case <-ctx.Done():
			writeJSON(w, http.StatusRequestTimeout, map[string]string{"error": "请求已取消，尚未开始的任务不会执行"})
			return
		case <-ticker.C:
			phase, _ := job.snapshot()["phase"].(string)
			if phase == "failed" || phase == "cancelled" || phase == "done" {
				select {
				case resp := <-result:
					writeJSON(w, resp.status, resp.body)
				default:
					writeJSON(w, http.StatusBadGateway, map[string]string{"error": "任务未执行，请查看任务详情", "job_id": job.ID})
				}
				return
			}
		}
	}
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
	for _, code := range body.TaskCodes {
		if !upstream.IsCurrentGrowthTask(code) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "任务已暂停或不在当前任务列表"})
			return
		}
	}
	h.growthTaskHTTP(w, r, a, "接受任务", func(scoped *auth.Auth) (int, map[string]any) {
		if err := h.cfg.Upstream.AcceptTasks(scoped, body.TaskCodes); err != nil {
			return http.StatusBadGateway, map[string]any{"error": "accept: " + err.Error()}
		}
		log.Printf("growth: 接受任务 uid=%s codes=%v", uid, body.TaskCodes)
		return http.StatusOK, map[string]any{"ok": true}
	})
}

// accountTaskAcceptAll 接受该账号全部尚未接受的任务（跳过已 accepted/claimed/locked）。
// 批量提交分片（保守每批 20 个），批间节流防风控。
func (h *Handler) accountTaskAcceptAll(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := h.growthAccount(w, uid)
	if a == nil {
		return
	}
	h.growthTaskHTTP(w, r, a, "接受全部可用任务", func(scoped *auth.Auth) (int, map[string]any) {
		tasks, err := h.cfg.Upstream.ListTasks(scoped)
		if err != nil {
			return http.StatusBadGateway, map[string]any{"error": "list tasks: " + err.Error()}
		}
		var codes []string
		for _, t := range tasks {
			if h.growthTaskSkipReason(uid, &t) != "" || t.Claimable || t.AcceptStatus == "accepted" {
				continue
			}
			codes = append(codes, t.TaskCode)
		}
		if len(codes) == 0 {
			return http.StatusOK, map[string]any{"ok": true, "accepted": 0, "message": "所有可用任务均已接受或完成"}
		}
		const batch = 20
		accepted := 0
		var failed []string
		for i := 0; i < len(codes); i += batch {
			end := i + batch
			if end > len(codes) {
				end = len(codes)
			}
			if err := h.cfg.Upstream.AcceptTasks(scoped, codes[i:end]); err != nil {
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
		return http.StatusOK, resp
	})
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
	if !upstream.IsCurrentGrowthTask(body.TaskCode) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "任务已暂停或不在当前任务列表"})
		return
	}
	h.growthTaskHTTP(w, r, a, "领取任务奖励", func(scoped *auth.Auth) (int, map[string]any) {
		credit, energy, err := h.cfg.Upstream.ClaimReward(scoped, body.TaskCode)
		if err != nil {
			return http.StatusBadGateway, map[string]any{"error": "claim: " + err.Error()}
		}
		if !h.cfg.Upstream.GrowthRouteValid(scoped) {
			return http.StatusConflict, map[string]any{"error": "账号或凭据已变化，领奖结果需重新核对"}
		}
		creds := scoped.Snapshot()
		h.growthLedger.record(creds.UID, creds.Nickname, body.TaskCode, credit, energy)
		h.ledgerReconCache.markClaimed(creds.UID, body.TaskCode)
		if credit == 0 && energy == 0 {
			log.Printf("growth: 领取任务奖励 uid=%s code=%s（已领取过，无新增）", uid, body.TaskCode)
			return http.StatusOK, map[string]any{"ok": true, "already_claimed": true, "message": "该奖励此前已领取"}
		}
		log.Printf("growth: 领取任务奖励 uid=%s code=%s +%d分 +%d能", uid, body.TaskCode, credit, energy)
		return http.StatusOK, map[string]any{"ok": true, "credit": credit, "energy": energy}
	})
}

// accountTaskAuto 一键完成单个任务：执行对应动作 → 回读进度 → 达标自动领奖。
func (h *Handler) accountTaskAuto(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := h.growthAccount(w, uid)
	if a == nil {
		return
	}
	guard := h.growthAuthGuard(a)
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
	h.growthLocks.unlock(uid)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	job := h.growthJobs.newJob(uid, "task")
	result := make(chan map[string]any, 1)
	if !h.growthSubmitTask(ctx, job, a, func(scoped *auth.Auth) {
		resp := h.runGrowthAction(scoped, act)
		if ctx.Err() == nil && guard() == nil {
			h.collectAfterAction(uid)
		}
		job.setTotal(1)
		ok, _ := resp["ok"].(bool)
		skipped, _ := resp["skipped"].(bool)
		job.finishItem(growthJobItem{TaskCode: act.TaskCode, UID: uid, OK: ok, Skipped: skipped})
		job.finish("done", "")
		result <- resp
	}) {
		job.finish("failed", "该账号已在执行队列中")
		writeJSON(w, http.StatusConflict, map[string]string{"error": "该账号有任务动作正在排队或执行中"})
		return
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case resp := <-result:
			writeJSON(w, http.StatusOK, resp)
			return
		case <-ctx.Done():
			writeJSON(w, http.StatusRequestTimeout, map[string]string{"error": "请求已取消，尚未开始的任务不会执行"})
			return
		case <-ticker.C:
			// Preparation can fail before the callback runs (for example a
			// deleted account or an unavailable route). Do not wait forever.
			if phase, _ := job.snapshot()["phase"].(string); phase == "failed" || phase == "cancelled" {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": "任务未执行，请查看任务详情", "job_id": job.ID})
				return
			}
		}
	}
}

// runGrowthAction 执行单个动作并组装响应（回读 + 自动领奖）。
// 调用方必须已持有该账号的 growth 锁。
func (h *Handler) runGrowthAction(a *auth.Auth, act *growthAction) map[string]any {
	before, err := h.taskByCode(a, act.TaskCode)
	if err != nil {
		return map[string]any{"ok": false, "error": "list tasks: " + err.Error()}
	}
	return h.runGrowthActionFromTask(a, act, before)
}

// The supplied preflight state decides whether an action is still needed.
func (h *Handler) runGrowthActionFromTask(a *auth.Auth, act *growthAction, before *upstream.Task) map[string]any {
	uid := a.Snapshot().UID
	if !h.cfg.Upstream.GrowthRouteValid(a) {
		return map[string]any{"ok": false, "error": "账号或凭据已变化，成长任务已停止"}
	}
	if before == nil {
		return map[string]any{"ok": true, "skipped": true, "message": "该账号没有此任务"}
	}
	if reason := h.growthTaskSkipReason(uid, before); reason != "" {
		return map[string]any{"ok": true, "skipped": true, "message": reason}
	}
	if act.TaskCode == "black_cat" && !before.Claimable && !upstream.InNightWindow(time.Now()) {
		return map[string]any{"ok": true, "skipped": true, "message": "当前不在夜猫子任务窗口，留待有效时段执行"}
	}
	msg := "任务已达标，无需重复执行"
	after := before
	var aerr error
	if !before.Claimable {
		var err error
		msg, err = act.run(h, a)
		if err != nil {
			return map[string]any{"ok": false, "error": "执行失败: " + err.Error()}
		}
		// A successful event is not proof of progress. Read back with a bounded
		// polling budget before attempting a reward claim.
		after, aerr = h.taskByCodeWaiting(a, act.TaskCode)
	}
	progressBefore, progressAfter := taskProgressText(before), ""
	claimable := false
	if aerr == nil && after != nil {
		progressAfter = taskProgressText(after)
		claimable = after.Claimable
	}
	if aerr != nil {
		return map[string]any{"ok": false, "error": "任务执行后状态查询失败，结果未确认"}
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
			if !h.cfg.Upstream.GrowthRouteValid(a) {
				return map[string]any{"ok": false, "error": "账号或凭据已变化，领奖结果需重新核对"}
			}
			resp["claimed"] = true
			resp["credit"] = credit
			resp["energy"] = energy
			// 持久台账：所有执行路径（单任务/一键/批跑/新号钩子）的领取
			// 都汇聚到这一处，记一次即可全局覆盖。
			creds := a.Snapshot()
			h.growthLedger.record(creds.UID, creds.Nickname, act.TaskCode, credit, energy)
			// 领奖改变了上游 claimed 状态 → 更新该账号的对账快照（单号
			// 精准更新而非整体失效：批跑中后续账号的预跳过仍可用）。
			h.ledgerReconCache.markClaimed(creds.UID, act.TaskCode)
			if credit > 0 || energy > 0 {
				resp["message"] = msg + fmt.Sprintf("；已自动领奖 +%d 分 +%d 能", credit, energy)
			} else {
				resp["message"] = msg + "；奖励此前已领取"
			}
		} else {
			resp["ok"] = false
			resp["claim_error"] = cerr.Error()
			resp["message"] = msg + "；达标但领奖失败，可在任务列表手动点「领取」重试"
		}
	}
	log.Printf("growth: 任务动作 uid=%s code=%s progress %s -> %s claimable=%v claimed=%v",
		uid, act.TaskCode, progressBefore, progressAfter, claimable, resp["claimed"])
	return resp
}

func growthTaskUnavailable(t *upstream.Task) bool {
	for _, state := range []string{t.Status, t.AcceptStatus} {
		switch strings.ToLower(strings.TrimSpace(state)) {
		case "expired", "disabled", "unavailable", "closed", "ended", "not_eligible", "invalid":
			return true
		}
	}
	return false
}

func (h *Handler) growthTaskSkipReason(uid string, t *upstream.Task) string {
	return h.growthTaskSkipReasonAt(uid, t, time.Now())
}

func (h *Handler) growthTaskSkipReasonAt(uid string, t *upstream.Task, now time.Time) string {
	if !upstream.IsCurrentGrowthTask(t.TaskCode) {
		return "任务已暂停或不在当前任务列表，跳过"
	}
	if t.Claimed {
		return "该任务已领取过奖励"
	}
	// The durable ledger is only authoritative for explicitly one-off tasks.
	// A recurring task must always use its current upstream state.
	if strings.EqualFold(strings.TrimSpace(t.TaskType), "single") && h.growthLedger != nil {
		h.growthLedger.mu.Lock()
		account := h.growthLedger.byUID[uid]
		claimed := false
		if account != nil {
			for _, c := range account.Claims {
				claimed = claimed || c.TaskCode == t.TaskCode
			}
		}
		h.growthLedger.mu.Unlock()
		if claimed {
			return "一次性任务已在持久台账中完成，跳过"
		}
	}
	if growthTaskUnavailable(t) {
		return "任务已结束或当前账号不具备完成条件，跳过"
	}
	if t.Locked {
		return "任务尚未解锁，跳过"
	}
	status, acceptStatus := strings.ToLower(strings.TrimSpace(t.Status)), strings.ToLower(strings.TrimSpace(t.AcceptStatus))
	if !t.Claimable && (status == "complete" || status == "completed" || acceptStatus == "completed") {
		return "任务已完成，无需重复执行"
	}
	if t.TaskCode == "black_cat" && !t.Claimable && !upstream.InNightWindow(now) {
		return "当前不在夜猫子任务窗口，留待有效时段执行"
	}
	return ""
}

// accountTaskAutoAll 一键完成该账号当前可自动任务（依赖序 + 逐项自动领奖）。
// 异步 job 化 + 入执行池：立即返回 202 + job_id，进度走 GET /admin/growth/jobs/{id} 轮询。
// 与全池批跑共用同一队列（growthRunner）：同账号/连接IP互斥，按实时并发设置不同资源并发；
// 已在队列/执行中返回409（前端提示等待）。
func (h *Handler) accountTaskAutoAll(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := h.growthAccount(w, uid)
	if a == nil {
		return
	}
	job := h.growthJobs.newJob(uid, "account")
	if !h.growthSubmitAccount(job, a) {
		job.finish("failed", "该账号已在执行队列中，请等本轮结束后再试")
		writeJSON(w, http.StatusConflict, map[string]string{"error": "该账号已在执行队列中，请等本轮结束后再试"})
		return
	}
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

// allAccountsTaskAutoAll 对全部非禁用 CN 账号执行一键完成（批跑）。
// 入执行池（growthRunner）：所有触发源（手动全池/手动单账号/自动加号/重启恢复）
// 共用账号/连接IP互斥的有界执行池。job化立即返回202 + job_id；
// 进度 = 账号粒度（当前跑到哪个账号）。
func (h *Handler) allAccountsTaskAutoAll(w http.ResponseWriter, r *http.Request) {
	var accounts []*auth.Auth
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
		accounts = append(accounts, a)
	}
	if len(accounts) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "没有可执行的账号"})
		return
	}
	job := h.growthJobs.newJob("", "all")
	enqueued := h.growthSubmitAll(job, accounts)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "job_id": job.ID, "total_accounts": enqueued,
		"message": "批跑已开始，可关闭页面",
	})
}

// runGrowthAllCollect 带逐项回调的全量流水线（all 模式复用；onStart 在每项
// 动作开始前调用一次）。返回 (明细, 列表拉取是否成功)。
func (h *Handler) runGrowthAllCollect(a *auth.Auth, onStart func(code, desc string)) ([]growthJobItem, bool) {
	uid := a.Snapshot().UID
	defer func() {
		if h.cfg.Upstream.GrowthRouteValid(a) {
			h.collectAfterAction(uid)
		}
	}()
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
		if growthActionFor(t.TaskCode) == nil || h.growthTaskSkipReason(uid, &t) != "" || t.Claimable || t.AcceptStatus == "accepted" {
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
		// Earlier actions can complete or unlock another task (Buddy_App and
		// Buddy_App_QQ share events). Recheck outstanding items before acting.
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
		if item.OK && !item.Skipped && !item.Claimed {
			item.OK = false
			item.Message += fmt.Sprintf("；上游未确认完成（进度 %v → %v），未领奖", res["progress_before"], res["progress_after"])
		}
		items = append(items, item)
		if !item.Skipped {
			time.Sleep(actionStepGap)
		}
	}
	return items, true
}

// runTravel / runActivity 手动触发旅行巡检 / 活跃上报（异步起跑，立即返回）。
func (h *Handler) runTravel(w http.ResponseWriter, r *http.Request) {
	if h.cfg.TravelNow == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "旅行排程未启用"})
		return
	}
	lifecycle.Go(h.cfg.TravelNow)
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
	lifecycle.Go(h.cfg.SchoolNow)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "开学季活动闭环已启动"})
}

// schoolStatus 开学季状态：并发拉各账号任务矩阵 + 剩余抽奖次数 + 券码。
// 供前端状态卡展示（只读；活动期外 in_period=false 全量返回）。
func (h *Handler) schoolStatus(w http.ResponseWriter, r *http.Request) {
	type accountStatus struct {
		UID      string                   `json:"uid"`
		Nickname string                   `json:"nickname,omitempty"`
		InPeriod bool                     `json:"in_period"`
		Tasks    []upstream.SchoolTask    `json:"tasks,omitempty"`
		Chances  int                      `json:"chances"`
		Vouchers []upstream.SchoolVoucher `json:"vouchers,omitempty"`
		Error    string                   `json:"error,omitempty"`
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
