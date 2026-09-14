// Package metricsstore persists aggregate request metrics in SQLite.
package metricsstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Snapshot is the persisted aggregate used by the HTTP status endpoints.
type Snapshot struct {
	Requests         int64   `json:"requests"`
	Successes        int64   `json:"successes"`
	Failures         int64   `json:"failures"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CacheRead        int64   `json:"cache_read_tokens"`
	CacheWrite       int64   `json:"cache_write_tokens"`
	ToolCalls        int64   `json:"tool_calls"`
	TTFBMillis       int64   `json:"ttfb_millis"`
	TTFBSamples      int64   `json:"ttfb_samples"`
	LatencyMillis    int64   `json:"latency_millis"`
	CreditsConsumed  float64 `json:"credits_consumed"`
	CreditsUpstream  float64 `json:"credits_upstream"`
	CreditsEstimated float64 `json:"credits_estimated"`
	CreditRequests   int64   `json:"credit_requests"`
	LastRequestUnix  int64   `json:"last_request_at"`
}

// RequestRecord stores request-level metadata without prompts or response text.
type RequestRecord struct {
	ID                    string  `json:"id"`
	CreatedAt             int64   `json:"created_at"`
	Route                 string  `json:"route"`
	Model                 string  `json:"model"`
	Mode                  string  `json:"mode"`
	Status                int     `json:"status"`
	AccountUID            string  `json:"account_uid,omitempty"`
	AccountRegion         string  `json:"account_region,omitempty"`
	RequestedOutputTokens int64   `json:"requested_output_tokens"`
	InputTokens           int64   `json:"input_tokens"`
	OutputTokens          int64   `json:"output_tokens"`
	TotalTokens           int64   `json:"total_tokens"`
	CacheReadTokens       int64   `json:"cache_read_tokens"`
	CacheWriteTokens      int64   `json:"cache_write_tokens"`
	ToolCalls             int64   `json:"tool_calls"`
	TTFBMillis            int64   `json:"ttfb_millis"`
	LatencyMillis         int64   `json:"latency_millis"`
	CreditsConsumed       float64 `json:"credits_consumed"`
	CreditSource          string  `json:"credit_source"`
	Passthrough           bool    `json:"passthrough"`
	ErrorCode             string  `json:"error_code,omitempty"`
	ErrorMessage          string  `json:"error_message,omitempty"`
}

// Store is safe for concurrent request completion writes.
type Store struct {
	db            *sql.DB
	mu            sync.Mutex
	requestWrites int
}

// Backend is the common persistence contract used by the server. Store and
// PostgresStore both implement it, allowing deployments to switch databases
// without changing request handling code.
type Backend interface {
	Add(Snapshot) error
	AddCredit(float64, string) error
	RecordRequest(RequestRecord) error
	RecordCompletion(Snapshot, RequestRecord) error
	ReconcileRequestCredit(string, float64) error
	QueryRequests(RequestFilter) ([]RequestRecord, error)
	SummarizeRequests(RequestFilter) (RequestSummary, error)
	Snapshot() (Snapshot, error)
	Close() error
}

// RequestFilter describes searchable conditions for request log queries.
// Zero-value fields are ignored, so an empty filter matches everything.
// Limit caps the returned rows only; SummarizeRequests ignores it.
type RequestFilter struct {
	Limit         int
	SinceUnix     int64 // 只保留 created_at >= SinceUnix 的行
	UntilUnix     int64 // 只保留 created_at <= UntilUnix 的行
	Model         string
	Route         string
	AccountUID    string
	Region        string
	Status        int    // 精确状态码；0 = 不过滤
	Success       *bool  // nil = 不过滤；true = 2xx；false = 非 2xx
	ErrorCode     string
	ID            string
	TTFBMinMillis int64 // 首字时间下限（含）；ttfb_millis=0 的行会被 min>0 排除
	TTFBMaxMillis int64 // 首字时间上限（含）
}

// RequestSummary aggregates all request logs matching a filter. Unlike the
// row query it is not capped by Limit, so a windowed dashboard can show exact
// totals even when the matching rows far exceed the page size.
type RequestSummary struct {
	Requests         int64   `json:"requests"`
	Successes        int64   `json:"successes"`
	Failures         int64   `json:"failures"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	ToolCalls        int64   `json:"tool_calls"`
	TTFBMillisSum    int64   `json:"ttfb_millis_sum"`
	TTFBSamples      int64   `json:"ttfb_samples"`
	LatencyMillisSum int64   `json:"latency_millis_sum"`
	CreditsConsumed  float64 `json:"credits_consumed"`
}

const maxRequestLogs = 10000

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create metrics directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS metrics (id INTEGER PRIMARY KEY CHECK (id = 1), data TEXT NOT NULL, updated_at INTEGER NOT NULL)`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS request_logs (
		log_id INTEGER PRIMARY KEY AUTOINCREMENT,
		id TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		route TEXT NOT NULL,
		model TEXT NOT NULL,
		mode TEXT NOT NULL,
		status INTEGER NOT NULL,
		account_uid TEXT NOT NULL DEFAULT '',
		account_region TEXT NOT NULL DEFAULT '',
		requested_output_tokens INTEGER NOT NULL DEFAULT 0,
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens INTEGER NOT NULL DEFAULT 0,
		cache_read_tokens INTEGER NOT NULL DEFAULT 0,
		cache_write_tokens INTEGER NOT NULL DEFAULT 0,
		tool_calls INTEGER NOT NULL DEFAULT 0,
		ttfb_millis INTEGER NOT NULL DEFAULT 0,
		latency_millis INTEGER NOT NULL DEFAULT 0,
		credits_consumed REAL NOT NULL DEFAULT 0,
		credit_source TEXT NOT NULL DEFAULT '',
		passthrough INTEGER NOT NULL DEFAULT 0,
		error_code TEXT NOT NULL DEFAULT '',
		error_message TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		db.Close()
		return nil, err
	}
	// Keep existing installations compatible when request_logs was created by
	// an earlier version without the human-readable error detail column.
	if _, err := db.Exec(`ALTER TABLE request_logs ADD COLUMN error_message TEXT NOT NULL DEFAULT ''`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`ALTER TABLE request_logs ADD COLUMN account_region TEXT NOT NULL DEFAULT ''`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		db.Close()
		return nil, err
	}
	// Older installations used the externally supplied id as the primary key.
	// Migrate those tables to an internal row id so repeated/empty external IDs
	// remain distinct while preserving the public id field and all rows.
	if err := migrateRequestLogs(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS request_logs_created_at_idx ON request_logs(created_at DESC)`); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func migrateRequestLogs(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(request_logs)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var hasLogID, idPrimary bool
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == "log_id" {
			hasLogID = true
		}
		if name == "id" && pk != 0 {
			idPrimary = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if hasLogID || !idPrimary {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `ALTER TABLE request_logs RENAME TO request_logs_legacy`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE request_logs (
		log_id INTEGER PRIMARY KEY AUTOINCREMENT,
		id TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		route TEXT NOT NULL,
		model TEXT NOT NULL,
		mode TEXT NOT NULL,
		status INTEGER NOT NULL,
		account_uid TEXT NOT NULL DEFAULT '',
		account_region TEXT NOT NULL DEFAULT '',
		requested_output_tokens INTEGER NOT NULL DEFAULT 0,
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens INTEGER NOT NULL DEFAULT 0,
		cache_read_tokens INTEGER NOT NULL DEFAULT 0,
		cache_write_tokens INTEGER NOT NULL DEFAULT 0,
		tool_calls INTEGER NOT NULL DEFAULT 0,
		ttfb_millis INTEGER NOT NULL DEFAULT 0,
		latency_millis INTEGER NOT NULL DEFAULT 0,
		credits_consumed REAL NOT NULL DEFAULT 0,
		credit_source TEXT NOT NULL DEFAULT '',
		passthrough INTEGER NOT NULL DEFAULT 0,
		error_code TEXT NOT NULL DEFAULT '',
		error_message TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO request_logs(id, created_at, route, model, mode, status, account_uid, account_region, requested_output_tokens, input_tokens, output_tokens, total_tokens, cache_read_tokens, cache_write_tokens, tool_calls, ttfb_millis, latency_millis, credits_consumed, credit_source, passthrough, error_code, error_message)
		SELECT id, created_at, route, model, mode, status, account_uid, '', requested_output_tokens, input_tokens, output_tokens, total_tokens, cache_read_tokens, cache_write_tokens, tool_calls, ttfb_millis, latency_millis, credits_consumed, credit_source, passthrough, error_code, error_message FROM request_logs_legacy`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DROP TABLE request_logs_legacy`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS request_logs_created_at_idx ON request_logs(created_at DESC)`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Add(delta Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return addDelta(s.db, delta)
}

type metricsExecutor interface {
	QueryRow(query string, args ...any) *sql.Row
	Exec(query string, args ...any) (sql.Result, error)
}

func addDelta(db metricsExecutor, delta Snapshot) error {
	current, err := readSnapshot(db)
	if err != nil {
		return err
	}
	current.Requests += delta.Requests
	current.Successes += delta.Successes
	current.Failures += delta.Failures
	current.InputTokens += delta.InputTokens
	current.OutputTokens += delta.OutputTokens
	current.TotalTokens += delta.TotalTokens
	current.CacheRead += delta.CacheRead
	current.CacheWrite += delta.CacheWrite
	current.ToolCalls += delta.ToolCalls
	current.TTFBMillis += delta.TTFBMillis
	current.TTFBSamples += delta.TTFBSamples
	current.LatencyMillis += delta.LatencyMillis
	current.CreditsConsumed += delta.CreditsConsumed
	current.CreditsUpstream += delta.CreditsUpstream
	current.CreditsEstimated += delta.CreditsEstimated
	current.CreditRequests += delta.CreditRequests
	if delta.LastRequestUnix > current.LastRequestUnix {
		current.LastRequestUnix = delta.LastRequestUnix
	}
	raw, err := json.Marshal(current)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO metrics(id, data, updated_at) VALUES(1, ?, ?) ON CONFLICT(id) DO UPDATE SET data=excluded.data, updated_at=excluded.updated_at`, string(raw), time.Now().Unix())
	return err
}

func (s *Store) AddCredit(consumed float64, source string) error {
	if consumed <= 0 {
		return nil
	}
	delta := Snapshot{CreditsConsumed: consumed, CreditRequests: 1}
	switch source {
	case "upstream":
		delta.CreditsUpstream = consumed
	case "estimated":
		delta.CreditsEstimated = consumed
	}
	return s.Add(delta)
}

func (s *Store) RecordRequest(record RequestRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := insertRequest(s.db, record, s.requestWrites+1)
	if err == nil {
		s.requestWrites++
	}
	return err
}

// RecordCompletion commits usage, credits and the request log together. One
// durable transaction replaces up to three separate commits per HTTP request;
// failures roll back both the counters and the log, without an async loss window.
func (s *Store) RecordCompletion(delta Snapshot, record RequestRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = addDelta(tx, delta); err != nil {
		return err
	}
	if err = insertRequest(tx, record, s.requestWrites+1); err != nil {
		return err
	}
	if err = tx.Commit(); err == nil {
		s.requestWrites++
	}
	return err
}

// ReconcileRequestCredit replaces an estimated/unknown charge with the
// authoritative billing-meter value for a request ID.
func (s *Store) ReconcileRequestCredit(id string, credit float64) error {
	if id == "" || credit < 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old float64
	var source string
	err = tx.QueryRow(`SELECT credits_consumed, credit_source FROM request_logs WHERE id=? ORDER BY log_id DESC LIMIT 1`, id).Scan(&old, &source)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if source == "upstream" && old == credit {
		return nil
	}
	delta := Snapshot{CreditsConsumed: credit - old, CreditsUpstream: credit - old}
	switch source {
	case "estimated":
		delta.CreditsEstimated = -old
		delta.CreditsUpstream = credit
	case "unknown":
		delta.CreditRequests = 1
		delta.CreditsUpstream = credit
	}
	if err = addDelta(tx, delta); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE request_logs SET credits_consumed=?, credit_source='upstream' WHERE log_id=(SELECT log_id FROM request_logs WHERE id=? ORDER BY log_id DESC LIMIT 1)`, credit, id); err != nil {
		return err
	}
	return tx.Commit()
}

func insertRequest(db metricsExecutor, record RequestRecord, writes int) error {
	_, err := db.Exec(`INSERT INTO request_logs(
		id, created_at, route, model, mode, status, account_uid, account_region, requested_output_tokens,
		input_tokens, output_tokens, total_tokens, cache_read_tokens, cache_write_tokens,
		tool_calls, ttfb_millis, latency_millis, credits_consumed, credit_source,
		passthrough, error_code, error_message
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.CreatedAt, record.Route, record.Model, record.Mode, record.Status, record.AccountUID, record.AccountRegion, record.RequestedOutputTokens,
		record.InputTokens, record.OutputTokens, record.TotalTokens, record.CacheReadTokens, record.CacheWriteTokens,
		record.ToolCalls, record.TTFBMillis, record.LatencyMillis, record.CreditsConsumed, record.CreditSource,
		boolInt(record.Passthrough), record.ErrorCode, record.ErrorMessage,
	)
	if err == nil {
		if writes%100 == 0 {
			_, err = db.Exec(`DELETE FROM request_logs WHERE rowid IN (
				SELECT rowid FROM request_logs ORDER BY created_at DESC, rowid DESC LIMIT -1 OFFSET ?
			)`, maxRequestLogs)
		}
	}
	return err
}

func (s *Store) RecentRequests(limit int) ([]RequestRecord, error) {
	return s.QueryRequests(RequestFilter{Limit: limit})
}

// QueryRequests returns request log rows matching the filter, newest first.
func (s *Store) QueryRequests(filter RequestFilter) ([]RequestRecord, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	where, args := requestFilterWhere(filter, "?")
	rows, err := s.db.Query(`SELECT id, created_at, route, model, mode, status, account_uid, account_region, requested_output_tokens,
		input_tokens, output_tokens, total_tokens, cache_read_tokens, cache_write_tokens,
		tool_calls, ttfb_millis, latency_millis, credits_consumed, credit_source, passthrough, error_code, error_message
		FROM request_logs`+where+` ORDER BY created_at DESC, rowid DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RequestRecord
	for rows.Next() {
		var record RequestRecord
		var passthrough int
		if err := rows.Scan(&record.ID, &record.CreatedAt, &record.Route, &record.Model, &record.Mode, &record.Status, &record.AccountUID, &record.AccountRegion, &record.RequestedOutputTokens,
			&record.InputTokens, &record.OutputTokens, &record.TotalTokens, &record.CacheReadTokens, &record.CacheWriteTokens,
			&record.ToolCalls, &record.TTFBMillis, &record.LatencyMillis, &record.CreditsConsumed, &record.CreditSource, &passthrough, &record.ErrorCode, &record.ErrorMessage); err != nil {
			return nil, err
		}
		record.Passthrough = passthrough != 0
		out = append(out, record)
	}
	return out, rows.Err()
}

// SummarizeRequests aggregates every matching row without a LIMIT cap.
// COALESCE keeps SUM() NULL on an empty match set from failing the scan.
func (s *Store) SummarizeRequests(filter RequestFilter) (RequestSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	where, args := requestFilterWhere(filter, "?")
	row := s.db.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN status >= 200 AND status < 300 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status >= 200 AND status < 300 THEN 0 ELSE 1 END), 0),
		COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(total_tokens), 0),
		COALESCE(SUM(cache_read_tokens), 0), COALESCE(SUM(cache_write_tokens), 0), COALESCE(SUM(tool_calls), 0),
		COALESCE(SUM(ttfb_millis), 0), COALESCE(SUM(CASE WHEN ttfb_millis > 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(latency_millis), 0), COALESCE(SUM(credits_consumed), 0)
		FROM request_logs`+where, args...)
	var summary RequestSummary
	if err := row.Scan(&summary.Requests, &summary.Successes, &summary.Failures,
		&summary.InputTokens, &summary.OutputTokens, &summary.TotalTokens,
		&summary.CacheReadTokens, &summary.CacheWriteTokens, &summary.ToolCalls,
		&summary.TTFBMillisSum, &summary.TTFBSamples, &summary.LatencyMillisSum,
		&summary.CreditsConsumed); err != nil {
		return RequestSummary{}, err
	}
	return summary, nil
}

// requestFilterWhere builds the shared SQL WHERE clause and bound arguments
// for request log queries. placeholder is the dialect marker ("?" for SQLite,
// "$N" for Postgres). The returned clause is either empty or begins with
// " WHERE ".
func requestFilterWhere(filter RequestFilter, placeholder string) (string, []any) {
	useDollar := placeholder != "?"
	args := []any{}
	next := 1
	// ph 返回下一个占位符并把 arg 追加到参数列表。
	ph := func(arg any) string {
		var p string
		if useDollar {
			p = fmt.Sprintf("$%d", next)
		} else {
			p = "?"
		}
		next++
		args = append(args, arg)
		return p
	}
	var clauses []string
	if filter.SinceUnix > 0 {
		clauses = append(clauses, "created_at >= "+ph(filter.SinceUnix))
	}
	if filter.UntilUnix > 0 {
		clauses = append(clauses, "created_at <= "+ph(filter.UntilUnix))
	}
	if filter.Model != "" {
		clauses = append(clauses, "model = "+ph(filter.Model))
	}
	if filter.Route != "" {
		clauses = append(clauses, "route = "+ph(filter.Route))
	}
	if filter.AccountUID != "" {
		clauses = append(clauses, "account_uid = "+ph(filter.AccountUID))
	}
	if filter.Region != "" {
		clauses = append(clauses, "account_region = "+ph(filter.Region))
	}
	if filter.Status != 0 {
		clauses = append(clauses, "status = "+ph(filter.Status))
	}
	if filter.Success != nil && *filter.Success {
		clauses = append(clauses, "status >= 200 AND status < 300")
	} else if filter.Success != nil {
		clauses = append(clauses, "(status < 200 OR status >= 300)")
	}
	if filter.ErrorCode != "" {
		clauses = append(clauses, "error_code = "+ph(filter.ErrorCode))
	}
	if filter.ID != "" {
		clauses = append(clauses, "id = "+ph(filter.ID))
	}
	if filter.TTFBMinMillis > 0 {
		clauses = append(clauses, "ttfb_millis >= "+ph(filter.TTFBMinMillis))
	}
	if filter.TTFBMaxMillis > 0 {
		clauses = append(clauses, "ttfb_millis <= "+ph(filter.TTFBMaxMillis))
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func (s *Store) Snapshot() (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *Store) snapshotLocked() (Snapshot, error) {
	return readSnapshot(s.db)
}

func readSnapshot(db metricsExecutor) (Snapshot, error) {
	var raw string
	err := db.QueryRow(`SELECT data FROM metrics WHERE id=1`).Scan(&raw)
	if err == sql.ErrNoRows {
		return Snapshot{}, nil
	}
	if err != nil {
		return Snapshot{}, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("decode metrics: %w", err)
	}
	return snapshot, nil
}

func (s *Store) Close() error { return s.db.Close() }

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
