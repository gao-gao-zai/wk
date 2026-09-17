package metricsstore

import (
	"path/filepath"
	"testing"
	"time"
)

// countRows 直接数行数（QueryRequests 的 Limit 会被夹到 500，不能用它
// 验证删除语义）。
func countRows(s *Store) int {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&n); err != nil {
		return -1
	}
	return n
}

// TestRetentionDaysAndRows 双条件修剪：时间与条数同时生效，任一命中即删。
func TestRetentionDaysAndRows(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().Unix()
	// 250 行：50 行老（3 天前）、200 行新（刚刚）。
	for i := 0; i < 250; i++ {
		ts := now
		if i < 50 {
			ts = now - 3*86400
		}
		if err := s.RecordRequest(RequestRecord{ID: "r", CreatedAt: ts, Route: "/", Model: "m", Mode: "sync", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}

	// 策略：保留 2 天 + 最多 100 行。时间条件先删掉 50 行老数据，
	// 条数条件再把 200 行削到最新 100 行 → 总数 100。
	s.SetRetention(Retention{MaxRows: 100, MaxAge: 2 * 24 * time.Hour})

	// 手动触发一次修剪（写路径每 100 条才触发，测试直接调）。
	if err := trimRequestLogs(s.db, s.RetentionPolicy()); err != nil {
		t.Fatal(err)
	}
	if n := countRows(s); n != 100 {
		t.Fatalf("rows=%d, want 100 (50 expired by age, then capped at 100)", n)
	}
	// 剩下的必须都是新数据。
	rows, err := s.QueryRequests(RequestFilter{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.CreatedAt < now-86400 {
			t.Fatalf("stale row survived: created_at=%d", r.CreatedAt)
		}
	}
}

// TestRetentionDaysOnly 只按时间：rows=0 表示不限条数。
func TestRetentionDaysOnly(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().Unix()
	for i := 0; i < 120; i++ {
		ts := now - 10*86400 // 全部 10 天前
		if i%2 == 0 {
			ts = now // 一半新
		}
		if err := s.RecordRequest(RequestRecord{ID: "r", CreatedAt: ts, Route: "/", Model: "m", Mode: "sync", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	s.SetRetention(Retention{MaxAge: 5 * 24 * time.Hour}) // rows=0 不限条数
	if err := trimRequestLogs(s.db, s.RetentionPolicy()); err != nil {
		t.Fatal(err)
	}
	if n := countRows(s); n != 60 {
		t.Fatalf("rows=%d, want 60 (age-only must not cap count)", n)
	}
}

// TestRetentionRowsOnly 只按条数：days=0 表示不限时间。
func TestRetentionRowsOnly(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().Unix()
	for i := 0; i < 150; i++ {
		if err := s.RecordRequest(RequestRecord{ID: "r", CreatedAt: now - int64(i)*3600, Route: "/", Model: "m", Mode: "sync", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	s.SetRetention(Retention{MaxRows: 30}) // days=0 不限时间
	if err := trimRequestLogs(s.db, s.RetentionPolicy()); err != nil {
		t.Fatal(err)
	}
	if n := countRows(s); n != 30 {
		t.Fatalf("rows=%d, want 30 (rows-only must not age-filter)", n)
	}
}

// TestRetentionDefaults 默认（零值）行为与旧版一致：1 万条兜底。
func TestRetentionDefaults(t *testing.T) {
	r := Retention{}.OrDefault()
	if r.MaxRows != maxRequestLogs || r.MaxAge != 0 {
		t.Fatalf("default=%+v, want {MaxRows:%d}", r, maxRequestLogs)
	}
	// 双零（未 OrDefault）时 rowsCap 仍有兜底。
	if got := (Retention{}).rowsCap(); got != maxRequestLogs {
		t.Fatalf("zero rowsCap=%d, want %d fallback", got, maxRequestLogs)
	}
	// 只配时间时条数不限。
	if got := (Retention{MaxAge: time.Hour}).rowsCap(); got != 0 {
		t.Fatalf("age-only rowsCap=%d, want 0 (uncapped)", got)
	}
	// 只配条数时无时间条件。
	if got := (Retention{MaxRows: 500}).cutoffUnix(time.Now()); got != 0 {
		t.Fatalf("rows-only cutoff=%d, want 0", got)
	}
}
