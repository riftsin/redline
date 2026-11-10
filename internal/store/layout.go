package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/riftsin/redline/internal/layout"
)

func readLayout(ctx context.Context, q querier, key string) ([]layout.Warning, error) {
	var raw string
	if err := q.QueryRowContext(ctx, `SELECT layout_warnings FROM sessions WHERE key = ?`, key).Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var ws []layout.Warning
	if err := json.Unmarshal([]byte(raw), &ws); err != nil {
		return nil, err
	}
	return ws, nil
}

func writeLayout(ctx context.Context, tx *sql.Tx, key string, ws []layout.Warning) error {
	if ws == nil {
		ws = []layout.Warning{}
	}
	b, err := json.Marshal(ws)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE sessions SET layout_warnings = ? WHERE key = ?`, string(b), key)
	return err
}

// LayoutWarnings returns a session's layout-issue records.
func (s *Store) LayoutWarnings(ctx context.Context, key string) ([]layout.Warning, error) {
	return readLayout(ctx, s.db, key)
}

// ApplyLayoutPass folds one audit pass into the session's records,
// retiring viewport classes no longer audited, and reports whether
// anything changed.
func (s *Store) ApplyLayoutPass(ctx context.Context, key string, p layout.Pass, classes []string) (bool, error) {
	changed := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		ws, err := readLayout(ctx, tx, key)
		if err != nil {
			return err
		}
		ws, obsolete := layout.MarkObsolete(ws, classes, p.Revision, p.At)
		ws, applied := layout.Apply(ws, p)
		if changed = obsolete || applied; !changed {
			return nil
		}
		return writeLayout(ctx, tx, key, ws)
	})
	return changed, err
}

// QueueLayoutFixes marks the selected warnings as queued for repair and
// queues one note asking the agent to fix them, in one transaction. It
// returns how many were queued.
func (s *Store) QueueLayoutFixes(ctx context.Context, key string, ids []string) (int, error) {
	n := 0
	err := s.tx(ctx, func(tx *sql.Tx) error {
		sess, err := openSession(ctx, tx, key)
		if err != nil {
			return err
		}
		ws, err := readLayout(ctx, tx, key)
		if err != nil {
			return err
		}
		ws, queued := layout.Queue(ws, ids, sess.ArtifactRevision, s.now())
		if len(queued) == 0 {
			return nil
		}
		prompt, target := layout.Prompt(queued)
		label := "1 selected"
		if len(queued) != 1 {
			label = fmt.Sprintf("%d selected", len(queued))
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO queue_items (session_key, kind, prompt, tag, excerpt, target, created_at) VALUES (?, 'layout', ?, 'layout-warnings', ?, ?, ?)`,
			key, prompt, label, string(target), s.millis()); err != nil {
			return err
		}
		n = len(queued)
		return writeLayout(ctx, tx, key, ws)
	})
	return n, err
}

// DismissLayoutWarning hides a warning for the current artifact revision.
func (s *Store) DismissLayoutWarning(ctx context.Context, key, id string) (bool, error) {
	changed := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		sess, err := getSession(ctx, tx, key)
		if err != nil {
			return err
		}
		ws, err := readLayout(ctx, tx, key)
		if err != nil {
			return err
		}
		ws, changed = layout.Dismiss(ws, id, sess.ArtifactRevision, time.UnixMilli(s.millis()))
		if !changed {
			return nil
		}
		return writeLayout(ctx, tx, key, ws)
	})
	return changed, err
}
