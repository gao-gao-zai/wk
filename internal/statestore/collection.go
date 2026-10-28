package statestore

import (
	"context"
	"time"
)

// CollectionLoad and CollectionSave keep jobs/items/snapshots in the existing
// state database. Per-item records avoid rewriting an entire large batch.
func (s *Store) CollectionLoad() (map[string][]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS collection_records (id TEXT PRIMARY KEY, body TEXT NOT NULL)`); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, body FROM collection_records`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var id string
		var body []byte
		if err := rows.Scan(&id, &body); err != nil {
			return nil, err
		}
		out[id] = body
	}
	return out, rows.Err()
}
func (s *Store) CollectionSave(rows map[string][]byte, deletes []string) error {
	if len(rows) == 0 && len(deletes) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := `INSERT INTO collection_records(id,body) VALUES(` + s.placeholder(1) + `,` + s.placeholder(2) + `) ON CONFLICT(id) DO UPDATE SET body=EXCLUDED.body`
	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for k, v := range rows {
		if _, err := stmt.ExecContext(ctx, k, string(v)); err != nil {
			return err
		}
	}
	for _, k := range deletes {
		if _, err := tx.ExecContext(ctx, `DELETE FROM collection_records WHERE id=`+s.placeholder(1), k); err != nil {
			return err
		}
	}
	return tx.Commit()
}
