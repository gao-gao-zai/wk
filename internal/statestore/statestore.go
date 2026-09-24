// Package statestore 持久化账号池状态与增长任务台账（SQLite / PostgreSQL 双方言）。
//
// 设计约束（与 metricsstore 对齐）：
//   - SQLite：WAL + busy_timeout + synchronous=NORMAL——池状态是控制面数据，
//     但每 5s 的增量 flush 允许"掉电丢最后一两个事务"（与旧 JSON 全量重写的
//     5s 窗口等价），换取 flush 不卡在 fsync 上。
//   - PostgreSQL：复用 metricsstore 的 DSN 与连接池参数，不新增配置。
//   - 池状态按账号一行（state 列存 JSON blob）：写入粒度 = 变更过的账号，
//     schema 稳定性沿用 stateAccount 的 JSON tag 兼容机制，本包不理解内容。
//   - 台账为结构化列：append-only + (uid, task_code) 幂等。
package statestore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// Store 池状态 + 增长台账的持久化实现。所有方法可并发调用（sql.DB 自带
// 连接池；SQLite 单连接由 OpenSQLite 设置 MaxOpenConns(1) 保证）。
type Store struct {
	db      *sql.DB
	dialect string // "sqlite" | "postgres"
}

// GrowthClaim 单条任务领取记录（账号 × 任务码）。
type GrowthClaim struct {
	UID           string
	TaskCode      string
	Nickname      string
	Credit        int64
	Energy        int64
	ClaimedAtUnix int64
}

// Dialect 返回当前方言标识（"sqlite" / "postgres"），供启动日志区分模式。
func (s *Store) Dialect() string { return s.dialect }

// OpenSQLite 打开（必要时创建）SQLite 状态库。
func OpenSQLite(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create state directory: %w", err)
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
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA synchronous=NORMAL;`); err != nil {
		db.Close()
		return nil, err
	}
	if err := initSQLiteSchema(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, dialect: "sqlite"}, nil
}

func initSQLiteSchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS pool_accounts (
		uid TEXT PRIMARY KEY,
		state TEXT NOT NULL,
		updated_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create pool_accounts table: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS growth_claims (
		uid TEXT NOT NULL,
		task_code TEXT NOT NULL,
		nickname TEXT NOT NULL DEFAULT '',
		credit INTEGER NOT NULL DEFAULT 0,
		energy INTEGER NOT NULL DEFAULT 0,
		claimed_at INTEGER NOT NULL,
		PRIMARY KEY (uid, task_code)
	)`); err != nil {
		return fmt.Errorf("create growth_claims table: %w", err)
	}
	return nil
}

// OpenPostgres 打开 PostgreSQL 状态库。DSN 格式与 metricsstore.OpenPostgres
// 一致（任意 pgx 支持的 URL）。
func OpenPostgres(dsn string, maxOpenConns, maxIdleConns int, connMaxLifetime, connMaxIdleTime time.Duration) (*Store, error) {
	if dsn == "" {
		return nil, fmt.Errorf("postgres DSN is empty")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)
	db.SetConnMaxLifetime(connMaxLifetime)
	db.SetConnMaxIdleTime(connMaxIdleTime)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := initPostgresSchema(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, dialect: "postgres"}, nil
}

func initPostgresSchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS pool_accounts (
		uid TEXT PRIMARY KEY,
		state JSONB NOT NULL,
		updated_at BIGINT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create postgres pool_accounts table: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS growth_claims (
		uid TEXT NOT NULL,
		task_code TEXT NOT NULL,
		nickname TEXT NOT NULL DEFAULT '',
		credit BIGINT NOT NULL DEFAULT 0,
		energy BIGINT NOT NULL DEFAULT 0,
		claimed_at BIGINT NOT NULL,
		PRIMARY KEY (uid, task_code)
	)`); err != nil {
		return fmt.Errorf("create postgres growth_claims table: %w", err)
	}
	return nil
}

// Close 关闭底层连接池。
func (s *Store) Close() error { return s.db.Close() }

// UpsertPoolAccounts 在单事务内批量写入账号状态行（uid → JSON blob）。
// 空 rows 为无操作。
func (s *Store) UpsertPoolAccounts(rows map[string][]byte) error {
	if len(rows) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, s.upsertPoolSQL())
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now().Unix()
	for uid, state := range rows {
		if _, err := stmt.ExecContext(ctx, uid, string(state), now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) upsertPoolSQL() string {
	if s.dialect == "postgres" {
		return `INSERT INTO pool_accounts(uid, state, updated_at) VALUES($1, $2::jsonb, $3)
			ON CONFLICT(uid) DO UPDATE SET state=EXCLUDED.state, updated_at=EXCLUDED.updated_at`
	}
	return `INSERT INTO pool_accounts(uid, state, updated_at) VALUES(?, ?, ?)
		ON CONFLICT(uid) DO UPDATE SET state=EXCLUDED.state, updated_at=EXCLUDED.updated_at`
}

// DeletePoolAccounts 在单事务内删除给定账号的状态行。空 uids 为无操作。
func (s *Store) DeletePoolAccounts(uids []string) error {
	if len(uids) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `DELETE FROM pool_accounts WHERE uid = `+s.placeholder(1))
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, uid := range uids {
		if _, err := stmt.ExecContext(ctx, uid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadPoolAccounts 全量读取账号状态行（uid → JSON blob）。仅启动时调用。
func (s *Store) LoadPoolAccounts() (map[string][]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT uid, state FROM pool_accounts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var uid string
		var state []byte
		if err := rows.Scan(&uid, &state); err != nil {
			return nil, err
		}
		out[uid] = state
	}
	return out, rows.Err()
}

// MaxPoolUpdatedAt 返回最近一次 upsert 的时间戳（秒）；表空返回 0。
// 供启动时与 Redis 快照的 saved_at 比较（择新恢复）。
func (s *Store) MaxPoolUpdatedAt() (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var ts sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(updated_at) FROM pool_accounts`).Scan(&ts); err != nil {
		return 0, err
	}
	if !ts.Valid {
		return 0, nil
	}
	return ts.Int64, nil
}

// RecordGrowthClaim 幂等写入一条领取记录：同 (uid, task_code) 已存在时
// 保留首条的时间与积分，仅刷新昵称（与旧 JSON 台账"重复领取只保留首条"
// 语义一致；昵称更新沿用 growthLedger.record 的行为）。
func (s *Store) RecordGrowthClaim(c GrowthClaim) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var q string
	if s.dialect == "postgres" {
		q = `INSERT INTO growth_claims(uid, task_code, nickname, credit, energy, claimed_at)
			VALUES($1, $2, $3, $4, $5, $6)
			ON CONFLICT(uid, task_code) DO UPDATE SET nickname=EXCLUDED.nickname`
	} else {
		q = `INSERT INTO growth_claims(uid, task_code, nickname, credit, energy, claimed_at)
			VALUES(?, ?, ?, ?, ?, ?)
			ON CONFLICT(uid, task_code) DO UPDATE SET nickname=EXCLUDED.nickname`
	}
	_, err := s.db.ExecContext(ctx, q, c.UID, c.TaskCode, c.Nickname, c.Credit, c.Energy, c.ClaimedAtUnix)
	return err
}

// LoadGrowthClaims 全量读取领取记录（按 uid, task_code 排序保证确定性）。
// 仅启动时调用。
func (s *Store) LoadGrowthClaims() ([]GrowthClaim, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT uid, task_code, nickname, credit, energy, claimed_at
		FROM growth_claims ORDER BY uid, task_code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GrowthClaim
	for rows.Next() {
		var c GrowthClaim
		if err := rows.Scan(&c.UID, &c.TaskCode, &c.Nickname, &c.Credit, &c.Energy, &c.ClaimedAtUnix); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// placeholder 返回给定序号（1 起）的占位符（SQLite "?" / PG "$n"）。
func (s *Store) placeholder(n int) string {
	if s.dialect == "postgres" {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}
