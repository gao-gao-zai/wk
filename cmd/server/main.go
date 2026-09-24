// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"crypto/rand"
	"flag"
	"log"
	"net/http"
	// 注册 pprof handler 到 http.DefaultServeMux（上面 ListenAndServe 用的是
	// nil handler = DefaultServeMux）。主服务 handler 是显式传入的 h，不受影响。
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/groups"
	"workbuddy2api/internal/haozhuma"
	"workbuddy2api/internal/metricsstore"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/reqproxy"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/smslogin"
	"workbuddy2api/internal/statestore"
	"workbuddy2api/internal/upstream"
)

type metricsAdapter struct{ store metricsstore.Backend }

func (m metricsAdapter) AddMetrics(requests, successes, failures, inputTokens, outputTokens, totalTokens, cacheRead, cacheWrite, toolCalls, ttfbMillis, ttfbSamples, latencyMillis, lastRequestUnix int64) error {
	return m.store.Add(metricsstore.Snapshot{Requests: requests, Successes: successes, Failures: failures, InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: totalTokens, CacheRead: cacheRead, CacheWrite: cacheWrite, ToolCalls: toolCalls, TTFBMillis: ttfbMillis, TTFBSamples: ttfbSamples, LatencyMillis: latencyMillis, LastRequestUnix: lastRequestUnix})
}

func (m metricsAdapter) AddCredit(consumed float64, source string) error {
	return m.store.AddCredit(consumed, source)
}

func (m metricsAdapter) RecordRequest(record server.RequestLog) error {
	return m.store.RecordRequest(requestRecord(record))
}

func (m metricsAdapter) RecordCompletion(record server.RequestLog, ttfbObserved bool) error {
	delta := metricsstore.Snapshot{Requests: 1, InputTokens: record.InputTokens, OutputTokens: record.OutputTokens,
		TotalTokens: record.TotalTokens, CacheRead: record.CacheReadTokens, CacheWrite: record.CacheWriteTokens,
		ToolCalls: record.ToolCalls, TTFBMillis: record.TTFBMillis, LatencyMillis: record.LatencyMillis, LastRequestUnix: time.Now().Unix()}
	if record.Status >= 200 && record.Status < 300 {
		delta.Successes = 1
	} else {
		delta.Failures = 1
	}
	if ttfbObserved {
		delta.TTFBSamples = 1
	}
	if record.CreditsConsumed > 0 && record.CreditSource != "unknown" {
		delta.CreditsConsumed = record.CreditsConsumed
		delta.CreditRequests = 1
		if record.CreditSource == "upstream" {
			delta.CreditsUpstream = record.CreditsConsumed
		}
		if record.CreditSource == "estimated" {
			delta.CreditsEstimated = record.CreditsConsumed
		}
	}
	return m.store.RecordCompletion(delta, requestRecord(record))
}

func requestRecord(record server.RequestLog) metricsstore.RequestRecord {
	return metricsstore.RequestRecord{
		ID:                    record.ID,
		CreatedAt:             record.CreatedAt,
		Route:                 record.Route,
		Model:                 record.Model,
		Mode:                  record.Mode,
		Status:                record.Status,
		AccountUID:            record.AccountUID,
		AccountRegion:         record.AccountRegion,
		RequestedOutputTokens: record.RequestedOutputTokens,
		InputTokens:           record.InputTokens,
		OutputTokens:          record.OutputTokens,
		TotalTokens:           record.TotalTokens,
		CacheReadTokens:       record.CacheReadTokens,
		CacheWriteTokens:      record.CacheWriteTokens,
		ToolCalls:             record.ToolCalls,
		TTFBMillis:            record.TTFBMillis,
		LatencyMillis:         record.LatencyMillis,
		CreditsConsumed:       record.CreditsConsumed,
		CreditSource:          record.CreditSource,
		Passthrough:           record.Passthrough,
		ErrorCode:             record.ErrorCode,
		ErrorMessage:          record.ErrorMessage,
	}
}

func (m metricsAdapter) RecentRequests(limit int) ([]server.RequestLog, error) {
	records, err := m.store.QueryRequests(metricsstore.RequestFilter{Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]server.RequestLog, 0, len(records))
	for _, record := range records {
		out = append(out, serverRequestLog(record))
	}
	return out, nil
}

// QueryRequests / SummarizeRequests 把控制台请求日志页的筛选条件下推到
// 存储层，映射字段与 metricsstore.RequestFilter 一一对应。
func (m metricsAdapter) QueryRequests(filter server.RequestLogFilter) ([]server.RequestLog, error) {
	records, err := m.store.QueryRequests(requestLogFilter(filter))
	if err != nil {
		return nil, err
	}
	out := make([]server.RequestLog, 0, len(records))
	for _, record := range records {
		out = append(out, serverRequestLog(record))
	}
	return out, nil
}

func (m metricsAdapter) SummarizeRequests(filter server.RequestLogFilter) (server.RequestSummary, error) {
	summary, err := m.store.SummarizeRequests(requestLogFilter(filter))
	if err != nil {
		return server.RequestSummary{}, err
	}
	return server.RequestSummary{
		Requests: summary.Requests, Successes: summary.Successes, Failures: summary.Failures,
		InputTokens: summary.InputTokens, OutputTokens: summary.OutputTokens, TotalTokens: summary.TotalTokens,
		CacheReadTokens: summary.CacheReadTokens, CacheWriteTokens: summary.CacheWriteTokens,
		ToolCalls: summary.ToolCalls, TTFBMillisSum: summary.TTFBMillisSum, TTFBSamples: summary.TTFBSamples,
		LatencyMillisSum: summary.LatencyMillisSum, CreditsConsumed: summary.CreditsConsumed,
	}, nil
}

func requestLogFilter(filter server.RequestLogFilter) metricsstore.RequestFilter {
	return metricsstore.RequestFilter{
		Limit: filter.Limit, SinceUnix: filter.SinceUnix, UntilUnix: filter.UntilUnix,
		Model: filter.Model, Route: filter.Route, AccountUID: filter.AccountUID, Region: filter.Region,
		Status: filter.Status, Success: filter.Success, ErrorCode: filter.ErrorCode, ID: filter.ID,
		TTFBMinMillis: filter.TTFBMinMillis, TTFBMaxMillis: filter.TTFBMaxMillis,
	}
}

func serverRequestLog(record metricsstore.RequestRecord) server.RequestLog {
	return server.RequestLog{
		ID:                    record.ID,
		CreatedAt:             record.CreatedAt,
		Route:                 record.Route,
		Model:                 record.Model,
		Mode:                  record.Mode,
		Status:                record.Status,
		AccountUID:            record.AccountUID,
		AccountRegion:         record.AccountRegion,
		RequestedOutputTokens: record.RequestedOutputTokens,
		InputTokens:           record.InputTokens,
		OutputTokens:          record.OutputTokens,
		TotalTokens:           record.TotalTokens,
		CacheReadTokens:       record.CacheReadTokens,
		CacheWriteTokens:      record.CacheWriteTokens,
		ToolCalls:             record.ToolCalls,
		TTFBMillis:            record.TTFBMillis,
		LatencyMillis:         record.LatencyMillis,
		CreditsConsumed:       record.CreditsConsumed,
		CreditSource:          record.CreditSource,
		Passthrough:           record.Passthrough,
		ErrorCode:             record.ErrorCode,
		ErrorMessage:          record.ErrorMessage,
	}
}

func (m metricsAdapter) SnapshotMetrics() map[string]any {
	snapshot, err := m.store.Snapshot()
	if err != nil {
		return map[string]any{"error": "metrics unavailable"}
	}
	requests := snapshot.Requests
	ttfbSamples := snapshot.TTFBSamples
	avgLatency := int64(0)
	if requests > 0 {
		avgLatency = snapshot.LatencyMillis / requests
	}
	avgTTFB := int64(0)
	if ttfbSamples > 0 {
		avgTTFB = snapshot.TTFBMillis / ttfbSamples
	}
	return map[string]any{"requests": requests, "successes": snapshot.Successes, "failures": snapshot.Failures, "input_tokens": snapshot.InputTokens, "output_tokens": snapshot.OutputTokens, "total_tokens": snapshot.TotalTokens, "cache_read_tokens": snapshot.CacheRead, "cache_write_tokens": snapshot.CacheWrite, "tool_calls": snapshot.ToolCalls, "credits_consumed": snapshot.CreditsConsumed, "credits_upstream": snapshot.CreditsUpstream, "credits_estimated": snapshot.CreditsEstimated, "credit_requests": snapshot.CreditRequests, "avg_ttfb_ms": avgTTFB, "avg_latency_ms": avgLatency, "last_request_at": snapshot.LastRequestUnix}
}

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// 配置文件不存在时给一次机会用纯默认 + env
		if !os.IsNotExist(err) {
			log.Fatalf("load config: %v", err)
		}
		log.Printf("config %s not found, using defaults+env", *cfgPath)
		cfg, err = Load("")
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}
	// 两条路径都检查：文件存在但仍是示例占位符，与文件缺失一样危险。
	if err := requireRealCredential(cfg); err != nil {
		log.Fatalf("%v", err)
	}

	// 配置文件可写性探测：控制台保存（管理设置 → 保存）会回写 config.json，
	// 挂载成 :ro 时每次保存都静默失败，直到有人点保存才暴露。启动时探一次，
	// 把问题变成日志里的一行。只告警不阻断——只读部署（纯 env + 外部编排
	// 工具改配置）是合法形态。
	if err := probeConfigWritable(*cfgPath); err != nil {
		log.Printf("[warning] %v", err)
	}

	auths, err := auth.LoadDir(cfg.AuthDir, cfg.Region)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d %s account(s) from %s", len(auths), cfg.Region, cfg.AuthDir)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)
	// 请求日志保留策略：天数与条数双条件（任一命中即删，详见 Retention）。
	retention := metricsstore.Retention{
		MaxRows: cfg.RequestLogRetentionRows,
		MaxAge:  time.Duration(cfg.RequestLogRetentionDays) * 24 * time.Hour,
	}.OrDefault()
	var metricsDB metricsstore.Backend
	if cfg.Postgres.DSN != "" {
		metricsDB, err = metricsstore.OpenPostgres(cfg.Postgres.DSN, cfg.Postgres.MaxOpenConns, cfg.Postgres.MaxIdleConns, cfg.PostgresMaxLifetime, cfg.PostgresMaxIdleTime)
		if err != nil {
			// 失败时返回的是带类型的 nil。留在接口里会被当成非 nil，
			// 紧接着的类型断言和后面的 metricsDB != nil 都会踩空指针。
			metricsDB = nil
		} else if ps, ok := metricsDB.(*metricsstore.PostgresStore); ok {
			ps.SetRetention(retention)
		}
		if err != nil && cfg.Postgres.FallbackToSQLite {
			log.Printf("metrics postgres unavailable: %v; falling back to sqlite", err)
			metricsDB, err = metricsstore.Open(filepath.Join(filepath.Dir(cfg.StateFile), "metrics.db"), retention)
		}
		if err != nil {
			log.Printf("metrics postgres unavailable: %v; using in-memory metrics", err)
		}
	} else {
		metricsDB, err = metricsstore.Open(filepath.Join(filepath.Dir(cfg.StateFile), "metrics.db"), retention)
		if err != nil {
			log.Printf("metrics sqlite unavailable: %v; using in-memory metrics", err)
		}
	}
	if metricsDB != nil {
		defer metricsDB.Close()
	}
	var persistentMetrics server.MetricsStore
	var requestLogs server.RequestLogStore
	var completions server.CompletionStore
	if metricsDB != nil {
		adapter := metricsAdapter{store: metricsDB}
		persistentMetrics = adapter
		requestLogs = adapter
		completions = adapter
	}
	var responseStore server.ResponseStore
	if persistedResponses, ok := store.(server.ResponseStore); ok {
		responseStore = persistedResponses
	}

	p := pool.New(cfg.StateFile)
	defer p.Flush() // 进程退出前强制落盘（后台 flush 每 5s 一次，退出时补一次）

	// statestore：池状态 + 增长台账的数据库后端（PG → SQLite → nil 降级链，
	// 见 openStateStore）。nil 时 pool/台账走 JSON 文件模式（零回归）。
	// 注入时机在 Redis 择新之前：DB 先恢复成基础状态，Redis 快照只在
	// 比 DB 新时才覆盖（见 RestoreFromSnapshot 的择新逻辑），保证
	// "flush 时 DB 写失败但 Redis 镜像成功"的窗口不丢数据。
	stateDB := openStateStore(cfg)
	if stateDB != nil {
		defer stateDB.Close()
		p.SetStatePersister(stateDB)
	}

	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比 DB/本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限 + 三因子加权调优（从 config 注入，非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		salt, err := decodeSessionSalt(cfg.SessionSticky.Salt)
		if err != nil {
			log.Fatalf("%v", err)
		}
		if len(salt) == 0 {
			// 会话粘性没有盐也能工作，但键可被离线预计算（知道账号列表即可反推
			// 任意 key 落到哪个 uid）。这里生成一次性随机盐：会话粘性照常，
			// 重启后全部重新分配。要跨重启保持粘性就把 salt 写进配置。
			buf := make([]byte, 32)
			if _, err := rand.Read(buf); err != nil {
				log.Printf("session sticky: cannot generate salt: %v", err)
			} else {
				salt = buf
				log.Printf("session sticky: session_sticky.salt is unset; generated an ephemeral salt. " +
					"Sticky bindings will NOT survive restart — set session_sticky.salt to a fixed hex value to keep them.")
			}
		}
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			Salt:       salt,
			MaxEntries: cfg.SessionSticky.MaxEntries,
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()
	// 非流式/控制面：整请求（含读完响应体）上限。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 流式：独立策略，不再受上面那个整请求超时约束。默认不限总时长、只守 120s 空闲，
	// 这样长回答能一直流出，而上游卡住时仍会尽快失败。
	up.Stream = upstream.StreamPolicy{
		Total: cfg.StreamTimeoutDur,
		Idle:  cfg.StreamIdleDur,
	}
	up.SanitizeFingerprints = cfg.Features.SanitizeBlacklistFingerprints
	// Codex 兼容：改写 Codex CLI 系统提示词身份句（open source → open-source），
	// 绕开上游逐字指纹拦截。开关由 features.codex_compat / WB2A_CODEX_COMPAT 控制。
	upstream.SetCodexCompat(cfg.Features.CodexCompat)
	log.Printf("upstream timeouts: request=%ds stream_total=%s stream_idle=%s",
		cfg.Upstream.TimeoutSeconds, durLabel(cfg.StreamTimeoutDur), durLabel(cfg.StreamIdleDur))

	// reqproxy：实际请求账号的代理池（区别于 sms 的注册代理）。
	// 始终构造（WebUI 需要管理界面），enabled 状态在 state.json 里控制。
	// 启动失败不致命：降级为直连（零回归），控制台仍可诊断。
	reqProxyMgr, reqProxyKernel, reqProxyErr := newReqProxyManager(cfg)
	if reqProxyErr != nil {
		log.Printf("reqproxy: 启动失败，账号请求直连: %v", reqProxyErr)
	}
	if reqProxyMgr != nil {
		defer reqProxyMgr.Close()
		if reqProxyKernel != nil {
			defer reqProxyKernel.Close()
		}
		// 账号清单注入（预热用）：启动后首轮测速完成即给全量账号建槽，
		// 不等第一个请求才分配。
		reqProxyMgr.SetAccounts(poolAccountSource{p})
		up.DialProxy = reqProxyMgr.DialProxy
	}

	sch := scheduler.New(scheduler.Config{
		Pool:           p,
		Upstream:       up,
		RequestCredits: metricsDB,
		CheckinHours:   cfg.Schedule.CheckinHours,
		TravelHours:    cfg.Schedule.TravelHours,
		ActivityHours:  cfg.Schedule.ActivityHours,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
		BlackcatHours:  cfg.Schedule.BlackcatHours,

		CheckinDisabled:   !cfg.ScheduleEnabled.Checkin,
		TravelDisabled:    !cfg.ScheduleEnabled.Travel,
		ActivityDisabled:  !cfg.ScheduleEnabled.Activity,
		KeepaliveDisabled: !cfg.ScheduleEnabled.Keepalive,
		BlackcatDisabled:  !cfg.ScheduleEnabled.Blackcat,
	})

	// SMSLogin 与 AutoEnroll 必须共享同一个管理器：代理池冷却是全局状态，
	// 各建一份会让手动发码和自动加号在 30 分钟内撞同一个出口 IP。
	smsManager := newSMSLoginManager(cfg)

	// 分组 + 多密钥存储：与 state.json 同目录（部署时已挂载为卷）。
	// 初始化失败是致命的：密钥文件损坏时宁可不起服务，也不要退化成
	// "只有管理员密钥"——那会让所有分组密钥静默失效。
	groupStore, err := groups.New(groupsStoreDir(cfg))
	if err != nil {
		log.Fatalf("groups store: %v", err)
	}
	// 存量账号显式迁移进 default：读取路径有"未登记=default"的兜底，
	// 但选号过滤和分组计数只看归属表，不迁移的话 default 密钥会
	// 选不到任何号。
	if n := groupStore.MigrateUnregistered(p.UIDs()); n > 0 {
		log.Printf("groups: %d 个存量账号已归入 default 分组", n)
	}

	h := server.NewHandler(server.Config{
		Pool:             p,
		Upstream:         up,
		APIKey:           cfg.APIKey,
		FrontendPassword: cfg.FrontendPassword,
		Groups:           groupStore,
		ConfigPath:       *cfgPath,
		AuthDir:          cfg.AuthDir,
		Region:           cfg.Region,
		LoginBin:         "/app/login",
		// 静态控制台资源目录与可信代理：前者只放行 index.html + assets 白名单，
		// 后者决定是否采信 X-Forwarded-For（空 = 谁都不信，用 RemoteAddr）。
		FrontendDir:      cfg.FrontendDir,
		TrustedProxies:   cfg.TrustedProxies,
		CheckinNow:       sch.RunCheckinNow,
		CreditRefreshNow: sch.RunCreditRefreshNow,
		CheckinAccount:   sch.CheckinAccount,
		KeepaliveAccount: sch.KeepaliveAccount,
		// 短信直登只走中国区 codebuddy.cn 的 OneID/Keycloak；海外版继续用 OAuth 链接。
		SMSLogin: smsManager,
		// 请求代理模块（实际请求账号的代理池）：nil = 直连（零回归）。
		ReqProxy: reqProxyMgr,
		// 豪猪自动加号（可选）：取号→直登→落盘全自动。与手动发码共用代理池；
		// AutoEnroll 在 NewHandler 内部组装（persist 回调指向 handler）。
		HaozhumaClient: newHaozhumaClient(cfg),
		HaozhumaSid:    strings.TrimSpace(cfg.SMS.Haozhuma.Sid),
		// 号码账本放在 state.json 同目录（部署时该目录已挂载为卷）。
		AutoEnrollLedger: autoEnrollLedgerPath(cfg),
		// SMS 诊断日志同目录（data/ 卷内）：完整手机号+短信原文，本机排障用。
		SMSDebugPath: smsDebugLogPath(cfg),
		// 批跑恢复标记同目录：重启自动续跑剩余账号（见 ResumeGrowthJobsAfterRestart）。
		GrowthJobMarkPath: filepath.Join(filepath.Dir(cfg.StateFile), "growth-job.json"),
		// 一次性任务台账同目录：领取记录 + 积分收益持久化（前端三态展示）。
		// stateDB 可用时走 DB 模式（此路径退化为旧 JSON 的一次性导入源）。
		GrowthLedgerPath:  filepath.Join(filepath.Dir(cfg.StateFile), "growth-ledger.json"),
		GrowthLedgerStore: ledgerStoreAdapterOrNull(stateDB),
		UpdateSchedule: func(checkinHours, keepaliveHours []int) {
			// 老签名适配：只改签到/保活时点，其余排程参数不动（完整热改走 Reconfigure）。
			sch.Reconfigure(checkinHours, nil, nil, keepaliveHours, nil,
				!cfg.ScheduleEnabled.Checkin, !cfg.ScheduleEnabled.Travel,
				!cfg.ScheduleEnabled.Activity, !cfg.ScheduleEnabled.Keepalive,
				!cfg.ScheduleEnabled.Blackcat)
		},
		ReconfigureSchedule: func(checkinHours, travelHours, activityHours, keepaliveHours, blackcatHours []int,
			checkinDisabled, travelDisabled, activityDisabled, keepaliveDisabled, blackcatDisabled bool) {
			sch.Reconfigure(checkinHours, travelHours, activityHours, keepaliveHours, blackcatHours,
				checkinDisabled, travelDisabled, activityDisabled, keepaliveDisabled, blackcatDisabled)
		},
		TravelNow:   sch.RunTravelNow,
		ActivityNow: func() { go sch.RunActivityNow(context.Background()) },
		SchoolNow:   sch.RunSchoolNow,
		ScheduleEnabled: struct{ AutoenrollGrowthTasks bool }{
			AutoenrollGrowthTasks: cfg.ScheduleEnabled.AutoenrollGrowthTasks,
		},
		Session:         sessRouter,
		StickyCount:     sessCount,
		RedisMode:       redisMode,
		ResponseStore:   responseStore,
		MetricsStore:    persistentMetrics,
		RequestLogStore: requestLogs,
		CompletionStore: completions,
		// WebUI 改请求日志保留策略后即时推给 metricsstore（下一次修剪
		// 每 100 条写入触发——按新值执行）。
		SetRequestLogRetention: func(days, rows int) {
			if metricsDB != nil {
				metricsDB.SetRetention(metricsstore.Retention{
					MaxRows: rows,
					MaxAge:  time.Duration(days) * 24 * time.Hour,
				})
			}
		},
		CreditPolicy: server.CreditPolicy{
			InputPer1K:       cfg.Billing.InputCreditsPer1KTokens,
			OutputPer1K:      cfg.Billing.OutputCreditsPer1KTokens,
			CachedInputPer1K: cfg.Billing.CachedInputCreditsPer1KTokens,
		},
		Passthrough: cfg.Features.Passthrough,
		// Responses 端点开关反转：Config 零值必须表示"开"（单测约定），
		// 配置文件语义是 responses_api=true 开。
		DisableResponses: !cfg.Features.ResponsesAPI,
		// WebUI 特性开关保存后即时推到 upstream Client（脱敏/Codex 改写）。
		SetSanitizeFingerprints: up.SetSanitizeFingerprints,
		SetCodexCompat:          upstream.SetCodexCompat,
		// WebUI 上游超时保存后即时生效（单位秒）。
		SetUpstreamTimeouts: func(reqSecs, streamTotalSecs, streamIdleSecs int) {
			up.SetRequestTimeout(time.Duration(reqSecs) * time.Second)
			up.SetStreamPolicy(upstream.StreamPolicy{
				Total: time.Duration(streamTotalSecs) * time.Second,
				Idle:  time.Duration(streamIdleSecs) * time.Second,
			})
		},
		// 请求体大小上限（字节）：WebUI「请求体大小限制」卡片保存后
		// 即时生效；默认 8 MiB（config normalize 已保证 1-64）。
		MaxRequestBodyBytes: cfg.MaxRequestBodyMiB << 20,
		SoftCooldown:        cfg.SoftRateDur,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 补释放上次进程非正常结束时遗留的号码。放在开始服务之前：容器重启是
	// SIGKILL，收尾代码不会执行，号会一直占着豪猪的取号额度，导致之后每次
	// 取号都返回"余额不足,请释放拉黑后再取号"（看着像没钱，其实是号没还）。
	if h.AutoEnroller() != nil {
		if n := h.AutoEnroller().ReclaimOrphans(); n > 0 {
			log.Printf("auto-enroll: reclaimed %d number(s) left over from a previous run", n)
		}
	}
	go sch.RunCreditRefreshNow()
	go sch.RunRequestCreditRefreshNow()
	go sch.Run(ctx)

	// pprof 性能分析端点（默认关，见 config.Pprof 注释：无鉴权，只允许绑回环）。
	// CPU profile 会带来 ~几% 开销，heap/goroutine 基本零成本——平时不开，
	// 需要抓瓶颈时再 WB2A_PPROF_ENABLED=true。
	if cfg.PprofAddr != "" {
		go func() {
			log.Printf("pprof listening on http://%s/debug/pprof/", cfg.PprofAddr)
			if err := http.ListenAndServe(cfg.PprofAddr, nil); err != nil {
				log.Printf("pprof: %v", err)
			}
		}()
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// Bound header memory usage for internet-facing deployments. Request
		// bodies are limited by the handlers before they reach the upstream.
		MaxHeaderBytes: 32 << 10,
		// Close idle keep-alive connections periodically without affecting
		// active streaming responses.
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("workbuddy2api listening on %s (api_key=%v)", cfg.Listen, cfg.APIKey != "")
	// 重启恢复：上次进程死在批跑半路（部署/崩溃）时自动续跑剩余账号。
	// 动作幂等（已完成的秒级跳过），用户无需手动重新点「一键完成」。
	h.ResumeGrowthJobsAfterRestart()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// poolAccountSource 账号清单适配器（reqproxy 预热用）。
type poolAccountSource struct{ p *pool.Pool }

func (s poolAccountSource) UIDs() []string { return s.p.UIDs() }

func (s poolAccountSource) RegionOf(uid string) string {
	if a := s.p.AuthByUID(uid); a != nil {
		return a.Region()
	}
	return ""
}

// newReqProxyManager 组装请求代理模块。
//
// 返回 (manager, kernel, err)：kernel 单独返回是因为 Manager.Close 会停后台任务，
// 但 kernel 实例的 Close 需要在 manager 停止后仍可控（main defer 里按序关）。
// 启动失败不致命——降级直连（零回归），控制台可见错误。
func newReqProxyManager(cfg *Config) (*reqproxy.Manager, *reqproxy.Kernel, error) {
	rc := reqproxy.DefaultConfig()
	if cfg.ReqProxy.StateFile != "" {
		rc.StateFile = cfg.ReqProxy.StateFile
	} else {
		// 默认与 state.json 同目录（部署挂载卷内）
		rc.StateFile = filepath.Join(filepath.Dir(cfg.StateFile), "reqproxy", "state.json")
	}
	if cfg.ReqProxyHealthDur > 0 {
		rc.HealthInterval = cfg.ReqProxyHealthDur
	}
	if cfg.ReqProxyLatencyDur > 0 {
		rc.LatencyTimeout = cfg.ReqProxyLatencyDur
	}
	if cfg.ReqProxyUnhealthyDur > 0 {
		rc.UnhealthyCooldown = cfg.ReqProxyUnhealthyDur
	}
	if cfg.ReqProxy.PortMin > 0 {
		rc.PortMin = cfg.ReqProxy.PortMin
	}
	if cfg.ReqProxy.PortMax > 0 {
		rc.PortMax = cfg.ReqProxy.PortMax
	}

	kernel, err := reqproxy.NewKernel()
	if err != nil {
		return nil, nil, err
	}
	mgr, err := reqproxy.NewManager(rc, kernel)
	if err != nil {
		kernel.Close()
		return nil, nil, err
	}
	log.Printf("reqproxy: 已启动（端口段 %d-%d，状态文件 %s）", rc.PortMin, rc.PortMax, rc.StateFile)
	return mgr, kernel, nil
}

// newSMSLoginManager 组装短信直登管理器。
//
// 未配置 sms.two_captcha_key 时不装 solver：遇到 need_captcha 会保持原有行为
// （提示改用浏览器授权），而不是因为缺密钥就整体不可用。
func newSMSLoginManager(cfg *Config) *smslogin.Manager {
	m := smslogin.NewManager(smslogin.DefaultEndpoints(), 10*time.Minute)
	if solver := smslogin.NewTwoCaptchaSolver(cfg.SMS.TwoCaptchaKey); solver != nil {
		m.SetSolver(solver)
		log.Printf("sms login: 2captcha enabled for human verification challenges")
	}
	// 登录代理只影响短信直登链路，号池的日常 API 调用不走它。
	// file 优先：Webshare 这类静态名单每次登录换一条；url 是单出口（1024proxy 粘性或一条静态）。
	if file := strings.TrimSpace(cfg.SMS.Proxy.File); file != "" {
		pool, err := smslogin.LoadPoolDialer(file, cfg.SMSProxyCooldownDur)
		if err != nil {
			log.Fatalf("sms login: 加载代理名单 %s: %v", file, err)
		}
		m.SetProxyDialer(pool)
		log.Printf("sms login: proxy pool enabled (%d endpoints, cooldown=%s)", pool.Len(), cfg.SMSProxyCooldownDur)
	} else if url := strings.TrimSpace(cfg.SMS.Proxy.URL); url != "" {
		d := smslogin.NewResolverProxyDialerWithRegion(url, cfg.SMS.Proxy.Region, cfg.SMS.Proxy.StickyMinutes)
		d.InjectSID = cfg.SMS.Proxy.InjectSID
		m.SetProxyDialer(d)
		log.Printf("sms login: outbound proxy enabled (inject_sid=%v region=%s sticky=%dm)",
			d.InjectSID, strings.ToUpper(strings.TrimSpace(cfg.SMS.Proxy.Region)), stickyMinutesOrDefault(cfg.SMS.Proxy.StickyMinutes))
	}
	return m
}

// autoEnrollLedgerPath 号码账本路径：与 state.json 同目录（部署时已挂载为卷，
// 所以容器重建后文件还在）。StateFile 为空时返回空串 = 关闭持久化。
func autoEnrollLedgerPath(cfg *Config) string {
	sf := strings.TrimSpace(cfg.StateFile)
	if sf == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(sf), "autoenroll-held.json")
}

// ledgerStoreAdapter 把 statestore 的台账方法适配成 server.GrowthLedgerStore。
// 与 metricsAdapter 同一模式：main 负责类型翻译，包间不直接依赖。
type ledgerStoreAdapter struct{ store *statestore.Store }

func (a ledgerStoreAdapter) RecordGrowthClaim(c server.GrowthLedgerClaim) error {
	return a.store.RecordGrowthClaim(statestore.GrowthClaim{
		UID: c.UID, TaskCode: c.TaskCode, Nickname: c.Nickname,
		Credit: c.Credit, Energy: c.Energy, ClaimedAtUnix: c.ClaimedAtUnix,
	})
}

func (a ledgerStoreAdapter) LoadGrowthClaims() ([]server.GrowthLedgerClaim, error) {
	claims, err := a.store.LoadGrowthClaims()
	if err != nil {
		return nil, err
	}
	out := make([]server.GrowthLedgerClaim, 0, len(claims))
	for _, c := range claims {
		out = append(out, server.GrowthLedgerClaim{
			UID: c.UID, TaskCode: c.TaskCode, Nickname: c.Nickname,
			Credit: c.Credit, Energy: c.Energy, ClaimedAtUnix: c.ClaimedAtUnix,
		})
	}
	return out, nil
}

// ledgerStoreAdapterOrNull stateDB 为 nil 时返回 nil（台账走文件模式），
// 非空时包一层适配器。单独成函数是为了让 server.Config 字段表达式保持简洁。
func ledgerStoreAdapterOrNull(s *statestore.Store) server.GrowthLedgerStore {
	if s == nil {
		return nil
	}
	return ledgerStoreAdapter{store: s}
}

// openStateStore 打开池状态 + 增长台账的持久化后端（降级链：PG → SQLite → nil）。
//   - 配置了 postgres.dsn：优先 PG（失败不致命，回退 SQLite——与 metricsstore
//     的降级语义不同，池状态不允许"纯内存"静默丢，必须有本地兜底）；
//   - 否则 SQLite（state.json 同目录的 state.db）；
//   - SQLite 也打不开（卷故障/只读）：返回 nil，pool/growthledger 退化为
//     现有 JSON 文件模式（零回归），启动日志明示。
func openStateStore(cfg *Config) *statestore.Store {
	stateDir := groupsStoreDir(cfg)
	if cfg.Postgres.DSN != "" {
		s, err := statestore.OpenPostgres(cfg.Postgres.DSN, cfg.Postgres.MaxOpenConns, cfg.Postgres.MaxIdleConns, cfg.PostgresMaxLifetime, cfg.PostgresMaxIdleTime)
		if err == nil {
			log.Printf("[statestore] 池状态 + 增长台账存储 = postgres")
			return s
		}
		log.Printf("[statestore] postgres 不可用 (%v)，回退 sqlite", err)
	}
	s, err := statestore.OpenSQLite(filepath.Join(stateDir, "state.db"))
	if err != nil {
		log.Printf("[statestore] state.db 不可用 (%v)，池状态/台账退化为 JSON 文件模式", err)
		return nil
	}
	log.Printf("[statestore] 池状态 + 增长台账存储 = sqlite (%s)", filepath.Join(stateDir, "state.db"))
	return s
}

// smsDebugLogPath SMS 诊断日志路径：与号码账本同目录（data/ 卷，容器重建
// 不丢；0600 权限在 smsDebug 的 OpenFile 里设置）。StateFile 为空 = 关闭。
func smsDebugLogPath(cfg *Config) string {
	sf := strings.TrimSpace(cfg.StateFile)
	if sf == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(sf), "sms-debug.log")
}

// groupsStoreDir 分组/密钥存储目录：与 state.json 同目录（同 autoEnrollLedgerPath
// 的理由——挂载卷里，容器重建不丢）。StateFile 为空（池不落盘）时退回当前目录。
func groupsStoreDir(cfg *Config) string {
	sf := strings.TrimSpace(cfg.StateFile)
	if sf == "" {
		return "."
	}
	return filepath.Dir(sf)
}

func stickyMinutesOrDefault(v int) int {
	if v > 0 {
		return v
	}
	return 30
}

// durLabel 渲染超时用于启动日志。0 在这里是"不限/不检查"而非"零时长"，
// 直接打 Duration.String() 会显示 "0s"，容易被误读成一个立即超时的错误配置。
func durLabel(d time.Duration) string {
	if d <= 0 {
		return "unlimited"
	}
	return d.String()
}

// newHaozhumaClient 建豪猪客户端。未配置账号或项目 ID 时返回 nil（端点关闭）。
// persist/find 回调由 server.NewHandler 内部注入（依赖 handler 自身状态）。
//
// 有 user/pass 时优先用它们 login（token 失效能自动重登）；只有 token
// 时用 NewWithCredentials 尽量带上账密，便于运行期重登。
func newHaozhumaClient(cfg *Config) *haozhuma.Client {
	hz := cfg.SMS.Haozhuma
	sid := strings.TrimSpace(hz.Sid)
	if sid == "" {
		return nil
	}
	user := strings.TrimSpace(hz.User)
	pass := hz.Pass
	token := strings.TrimSpace(hz.Token)
	author := strings.TrimSpace(hz.Author)
	uid := strings.TrimSpace(hz.UID)
	isp := strings.TrimSpace(hz.ISP)

	setup := func(c *haozhuma.Client) *haozhuma.Client {
		c.Author, c.ISP = author, isp
		c.SetUID(uid)
		return c
	}

	if user != "" && pass != "" {
		c, err := haozhuma.Login(user, pass)
		if err != nil {
			// login 失败但手里有 token 时仍可先跑（token 可能还有效）。
			if token != "" {
				log.Printf("auto-enroll: 豪猪 login 失败(%v)，改用已配置 token", err)
				return setup(haozhuma.NewWithCredentials(token, user, pass))
			}
			log.Printf("auto-enroll: 豪猪登录失败，自动加号关闭: %v", err)
			return nil
		}
		log.Printf("auto-enroll: haozhuma login ok (sid=%s uid=%q isp=%q author=%q)", sid, uid, isp, author)
		return setup(c)
	}
	if token != "" {
		log.Printf("auto-enroll: haozhuma token configured (sid=%s uid=%q isp=%q author=%q)", sid, uid, isp, author)
		return setup(haozhuma.New(token))
	}
	return nil
}
