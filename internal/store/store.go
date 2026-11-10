// Package store persists redline's review state in SQLite: sessions, the
// reviewer's queue, sent feedback and the conversation. Every operation that
// reads and then writes runs in one transaction; the single connection keeps
// SQLite's one writer serialized.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store is the server's handle on the redline database. Only the background
// server process opens it.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens (creating if needed) the database at path and migrates it.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// SQLite allows one writer; a single connection keeps writes serialized
	// without SQLITE_BUSY retries.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// migrate applies every embedded migration newer than the database's
// user_version, each in its own transaction.
func (s *Store) migrate(ctx context.Context) error {
	var current int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return err
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for i, name := range names {
		version := i + 1
		if !strings.HasPrefix(strings.TrimPrefix(name, "migrations/"), fmt.Sprintf("%03d_", version)) {
			return fmt.Errorf("migration %s is out of sequence (want %03d_)", name, version)
		}
		if version <= current {
			continue
		}
		script, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		err = s.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, string(script)); err != nil {
				return fmt.Errorf("migration %s: %w", name, err)
			}
			_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version))
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) millis() int64 {
	return s.now().UnixMilli()
}

// Errors returned by operations on a session.
var (
	ErrNotFound = errors.New("no such session")
	ErrEnded    = errors.New("session has ended")
	ErrEmpty    = errors.New("empty text")

	ErrNoQueueItem        = errors.New("no such queued note")
	ErrTooManyAttachments = errors.New("too many images on one note")
	ErrAttachmentGone     = errors.New("an attached image no longer exists")
)
