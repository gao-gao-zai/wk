package metricsstore

import (
	"testing"
)

func TestRecordRequestKeepsDuplicateExternalIDs(t *testing.T) {
	store, err := Open(t.TempDir() + "/metrics.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	base := RequestRecord{ID: "upstream-id", CreatedAt: 1, Route: "/v1/chat/completions", Model: "glm-5v-turbo", Mode: "stream", Status: 200}
	if err := store.RecordRequest(base); err != nil {
		t.Fatal(err)
	}
	base.CreatedAt = 2
	if err := store.RecordRequest(base); err != nil {
		t.Fatal(err)
	}
	base.ID = ""
	base.CreatedAt = 3
	if err := store.RecordRequest(base); err != nil {
		t.Fatal(err)
	}

	rows, err := store.RecentRequests(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d request rows, want 3: %#v", len(rows), rows)
	}
	if rows[0].ID != "" || rows[1].ID != "upstream-id" || rows[2].ID != "upstream-id" {
		t.Fatalf("unexpected order or IDs: %#v", rows)
	}
}

// TestQueryRequestsFiltersAndSummarizes covers the console request-log page:
// every filter dimension the UI exposes must survive the SQL round trip, and
// the summary must aggregate the full match set (not the LIMIT-capped page).
func TestQueryRequestsFiltersAndSummarizes(t *testing.T) {
	store, err := Open(t.TempDir() + "/metrics.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	records := []RequestRecord{
		// 最新：成功的流式请求，首字 800ms
		{ID: "req_1", CreatedAt: 1000, Route: "/v1/chat/completions", Model: "glm-5v-turbo", Mode: "stream", Status: 200,
			AccountUID: "uid-a", AccountRegion: "cn", InputTokens: 100, OutputTokens: 50, TotalTokens: 150,
			CacheReadTokens: 20, CacheWriteTokens: 5, ToolCalls: 1, TTFBMillis: 800, LatencyMillis: 5000, CreditsConsumed: 0.5, CreditSource: "upstream"},
		// 失败请求，带错误码
		{ID: "req_2", CreatedAt: 900, Route: "/v1/responses", Model: "kimi-k3", Mode: "sync", Status: 429,
			AccountUID: "uid-b", AccountRegion: "global", ErrorCode: "rate_limited", ErrorMessage: "upstream throttled",
			TTFBMillis: 1200, LatencyMillis: 1500},
		// 老请求：在时间窗之外
		{ID: "req_3", CreatedAt: 100, Route: "/v1/chat/completions", Model: "glm-5v-turbo", Mode: "sync", Status: 200,
			AccountUID: "uid-a", AccountRegion: "cn", InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
		// 无 TTFB 的请求（非流式 / 失败前置）
		{ID: "req_4", CreatedAt: 950, Route: "/v1/chat/completions", Model: "hy3", Mode: "sync", Status: 200,
			AccountUID: "uid-a", AccountRegion: "cn", TTFBMillis: 0, LatencyMillis: 300},
	}
	for _, record := range records {
		if err := store.RecordRequest(record); err != nil {
			t.Fatal(err)
		}
	}

	// 时间窗：只要 created_at >= 500 的三条。
	rows, err := store.QueryRequests(RequestFilter{Limit: 100, SinceUnix: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].ID != "req_1" {
		t.Fatalf("since=500 got %d rows, want 3 newest-first: %+v", len(rows), rows)
	}

	// 多条件组合：时间窗 + 模型 + 成功。
	rows, err = store.QueryRequests(RequestFilter{Limit: 100, SinceUnix: 500, Model: "glm-5v-turbo", Success: boolPtr(true)})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "req_1" {
		t.Fatalf("model+success got %+v", rows)
	}

	// 失败 + 错误码。
	rows, err = store.QueryRequests(RequestFilter{Limit: 100, ErrorCode: "rate_limited"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "req_2" {
		t.Fatalf("error_code got %+v", rows)
	}

	// 状态码精确过滤。
	rows, err = store.QueryRequests(RequestFilter{Limit: 100, Status: 429})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "req_2" {
		t.Fatalf("status=429 got %+v", rows)
	}

	// 账号 + 区域。
	rows, err = store.QueryRequests(RequestFilter{Limit: 100, AccountUID: "uid-b", Region: "global"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "req_2" {
		t.Fatalf("account+region got %+v", rows)
	}

	// 请求 ID。
	rows, err = store.QueryRequests(RequestFilter{Limit: 100, ID: "req_3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "req_3" {
		t.Fatalf("id got %+v", rows)
	}

	// TTFB 区间：[700, 900] 只匹配 req_1（req_4 的 ttfb=0）。
	rows, err = store.QueryRequests(RequestFilter{Limit: 100, TTFBMinMillis: 700, TTFBMaxMillis: 900})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "req_1" {
		t.Fatalf("ttfb range got %+v", rows)
	}

	// 汇总（时间窗内三条：req_1、req_2、req_4）。
	summary, err := store.SummarizeRequests(RequestFilter{SinceUnix: 500})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Requests != 3 || summary.Successes != 2 || summary.Failures != 1 {
		t.Fatalf("summary counts: %+v", summary)
	}
	if summary.InputTokens != 100 || summary.OutputTokens != 50 || summary.TotalTokens != 150 {
		t.Fatalf("summary tokens: %+v", summary)
	}
	if summary.CacheReadTokens != 20 || summary.CacheWriteTokens != 5 || summary.ToolCalls != 1 {
		t.Fatalf("summary cache/tools: %+v", summary)
	}
	// TTFB 只统计 >0 的样本：req_1(800) + req_2(1200) = 2000。
	if summary.TTFBMillisSum != 2000 || summary.TTFBSamples != 2 {
		t.Fatalf("summary ttfb: %+v", summary)
	}
	if summary.LatencyMillisSum != 6800 {
		t.Fatalf("summary latency: %+v", summary)
	}
	if summary.CreditsConsumed != 0.5 {
		t.Fatalf("summary credits: %+v", summary)
	}

	// 空匹配：SUM 为 NULL 时汇总必须归零而不是报错。
	empty, err := store.SummarizeRequests(RequestFilter{SinceUnix: 99999})
	if err != nil {
		t.Fatal(err)
	}
	if empty.Requests != 0 || empty.CreditsConsumed != 0 {
		t.Fatalf("empty summary: %+v", empty)
	}
}

func boolPtr(value bool) *bool { return &value }
