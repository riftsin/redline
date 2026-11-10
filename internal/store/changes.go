package store

import (
	"context"
	"encoding/json"
	"time"
)

// MarkWorking stamps Working on everything delivered to the agent that it
// has not answered yet: the artifact changed after delivery. It reports
// whether any entry changed.
func (s *Store) MarkWorking(ctx context.Context, key string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE chat_entries SET working_at = ?
		WHERE session_key = ? AND role = 'user' AND receipt = '' AND delivered_at IS NOT NULL AND working_at IS NULL AND done_at IS NULL`,
		s.millis(), key)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Change is what one artifact revision changed.
type Change struct {
	Revision  int
	Selectors []string
	Rewrite   bool
	CreatedAt time.Time
}

const maxChangeSelectors = 200

// RecordChange stores what a revision changed. The first report wins: every
// open tab measures the same revision.
func (s *Store) RecordChange(ctx context.Context, key string, c Change) (bool, error) {
	if len(c.Selectors) > maxChangeSelectors {
		c.Selectors = c.Selectors[:maxChangeSelectors]
	}
	clean := []string{}
	for _, sel := range c.Selectors {
		if sel != "" && len(sel) <= selectorMax {
			clean = append(clean, sel)
		}
	}
	b, _ := json.Marshal(clean)
	rewrite := 0
	if c.Rewrite {
		rewrite = 1
	}
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO changes (session_key, revision, selectors, rewrite, created_at) VALUES (?, ?, ?, ?, ?)`,
		key, c.Revision, string(b), rewrite, s.millis())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Changes lists a session's recorded changes, oldest first.
func (s *Store) Changes(ctx context.Context, key string) ([]Change, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT revision, selectors, rewrite, created_at FROM changes WHERE session_key = ? ORDER BY revision`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		var selectors string
		var rewrite int
		var created int64
		if err := rows.Scan(&c.Revision, &selectors, &rewrite, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(selectors), &c.Selectors); err != nil {
			return nil, err
		}
		c.Rewrite = rewrite == 1
		c.CreatedAt = time.UnixMilli(created)
		out = append(out, c)
	}
	return out, rows.Err()
}
