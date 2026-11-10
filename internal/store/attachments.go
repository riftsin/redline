package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Attachment is an uploaded image's metadata. The bytes live in a file
// named after ID.
type Attachment struct {
	ID            string
	Mime          string
	Bytes         int64
	Width, Height int
	CreatedAt     time.Time
	UsedAt        time.Time
}

// SaveAttachment records an image, or refreshes its last use when the same
// bytes were uploaded before.
func (s *Store) SaveAttachment(ctx context.Context, a Attachment) error {
	now := s.millis()
	_, err := s.db.ExecContext(ctx, `INSERT INTO attachments (id, mime, bytes, width, height, created_at, used_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET used_at = excluded.used_at`,
		a.ID, a.Mime, a.Bytes, a.Width, a.Height, now, now)
	return err
}

// AttachmentByID returns an image's metadata.
func (s *Store) AttachmentByID(ctx context.Context, id string) (Attachment, error) {
	var a Attachment
	var created, used int64
	err := s.db.QueryRowContext(ctx, `SELECT id, mime, bytes, width, height, created_at, used_at FROM attachments WHERE id = ?`, id).
		Scan(&a.ID, &a.Mime, &a.Bytes, &a.Width, &a.Height, &created, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	a.CreatedAt, a.UsedAt = time.UnixMilli(created), time.UnixMilli(used)
	return a, err
}

// Attachments lists every stored image, least recently used first.
func (s *Store) Attachments(ctx context.Context) ([]Attachment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, mime, bytes, width, height, created_at, used_at FROM attachments ORDER BY used_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attachment
	for rows.Next() {
		var a Attachment
		var created, used int64
		if err := rows.Scan(&a.ID, &a.Mime, &a.Bytes, &a.Width, &a.Height, &created, &used); err != nil {
			return nil, err
		}
		a.CreatedAt, a.UsedAt = time.UnixMilli(created), time.UnixMilli(used)
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAttachment forgets an image's metadata.
func (s *Store) DeleteAttachment(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM attachments WHERE id = ?`, id)
	return err
}

// ReferencedAttachments returns the images still in use: on queued notes, on
// prompts the agent has not taken, and on prompts it took within grace (it
// may still be opening the file).
func (s *Store) ReferencedAttachments(ctx context.Context, grace time.Duration) (map[string]bool, error) {
	refs := map[string]bool{}
	add := func(query string, args ...any) error {
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			var list []struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(raw), &list); err != nil {
				continue
			}
			for _, r := range list {
				refs[r.ID] = true
			}
		}
		return rows.Err()
	}
	if err := add(`SELECT attachments FROM queue_items`); err != nil {
		return nil, err
	}
	cutoff := s.now().Add(-grace).UnixMilli()
	if err := add(`SELECT json_extract(payload, '$.attachments') FROM prompts
		WHERE json_extract(payload, '$.attachments') IS NOT NULL AND (delivered_at IS NULL OR delivered_at > ?)`, cutoff); err != nil {
		return nil, err
	}
	return refs, nil
}
