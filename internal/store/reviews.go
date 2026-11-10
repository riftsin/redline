package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// Session statuses.
const (
	StatusOpen     = "open"
	StatusFeedback = "feedback"
	StatusEnded    = "ended"
)

// Who ended a session.
const (
	EndedByUser  = "user"
	EndedByAgent = "agent"
)

// Session is one review: one person and one agent over one HTML file.
type Session struct {
	Key              string
	File             string
	Status           string
	EndedBy          string
	ArtifactRevision int
	DOMSnapshot      string
	ArtifactFailures []Failure
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Failure makes the review itself unusable: the artifact or one of its own
// local assets failed to load.
type Failure struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// AttachmentRef names an uploaded image attached to a note or message.
type AttachmentRef struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// PromptAttachment is an attached image as the agent receives it: a local
// file it can open.
type PromptAttachment struct {
	Path   string `json:"path"`
	Mime   string `json:"mime"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Bytes  int64  `json:"bytes"`
	Name   string `json:"name,omitempty"`
	ID     string `json:"id"`
}

// Describer turns an attachment reference into what the agent receives, or
// reports that the image no longer exists.
type Describer func(AttachmentRef) (PromptAttachment, bool)

// MaxAttachmentsPerPrompt bounds the images on one note or message.
const MaxAttachmentsPerPrompt = 4

// QueueItem is a note the reviewer queued but has not sent.
type QueueItem struct {
	ID         int64
	SessionKey string
	Kind       string // "annotation", "answer" or "message"
	Prompt     string
	Selector   string
	Tag        string
	Excerpt    string
	// Target describes what the note is attached to, as reported by the
	// SDK (for example a text range or a table cell). It may be empty.
	Target      json.RawMessage
	QueueKey    string
	Attachments []AttachmentRef
}

// Prompt is sent feedback in the shape the agent receives it.
type Prompt struct {
	Prompt   string          `json:"prompt"`
	Kind     string          `json:"kind"`
	Tag      string          `json:"tag,omitempty"`
	Selector string          `json:"selector,omitempty"`
	Text     string          `json:"text,omitempty"`
	Target   json.RawMessage `json:"target,omitempty"`

	Attachments []PromptAttachment `json:"attachments,omitempty"`
}

// Anchor names what a sent note was attached to, in the words the reviewer
// saw: "<h2>" for an element, "text" for a selection, "cell" for a table cell.
type Anchor struct {
	Kind     string `json:"kind"`
	Label    string `json:"label"`
	Excerpt  string `json:"excerpt,omitempty"`
	Selector string `json:"selector,omitempty"`
}

// ChatEntry is one bubble of the conversation.
type ChatEntry struct {
	ID          int64
	Role        string // "user" or "agent"
	Kind        string
	Text        string
	Anchor      *Anchor
	Attachments []AttachmentRef
	CreatedAt   time.Time

	// Delivery receipts of a user entry (zero until the step happened).
	DeliveredAt, WorkingAt, DoneAt time.Time
	// Receipt is ReceiptLive, ReceiptClosed or ReceiptNone.
	Receipt string
}

// Receipt kinds of a user entry.
const (
	ReceiptLive   = ""       // stamps still advance
	ReceiptClosed = "closed" // its review round ended; stamps are frozen
	ReceiptNone   = "none"   // sent before receipts existed; show none
)

const (
	maxArtifactFailures = 20
	excerptMax          = 120
	labelMax            = 40
	selectorMax         = 512
	promptMax           = 20000
	queueKeyMax         = 200
	tagMax              = 64
	targetMax           = 16384
	snapshotMax         = 200000
)

const sessionColumns = `key, file, status, ended_by, artifact_revision, dom_snapshot, artifact_failures, created_at, updated_at`

type scanner interface{ Scan(...any) error }

func scanSession(row scanner) (*Session, error) {
	var s Session
	var failures string
	var created, updated int64
	err := row.Scan(&s.Key, &s.File, &s.Status, &s.EndedBy, &s.ArtifactRevision, &s.DOMSnapshot, &failures, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(failures), &s.ArtifactFailures); err != nil {
		return nil, err
	}
	s.CreatedAt, s.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
	return &s, nil
}

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getSession(ctx context.Context, q querier, key string) (*Session, error) {
	return scanSession(q.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE key = ?`, key))
}

// Session returns the session with key.
func (s *Store) Session(ctx context.Context, key string) (*Session, error) {
	return getSession(ctx, s.db, key)
}

// ActiveSessions lists sessions that have not ended, by file path.
func (s *Store) ActiveSessions(ctx context.Context) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE status != ? ORDER BY file`, StatusEnded)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sess)
	}
	return out, rows.Err()
}

// OpenResult tells how OpenSession went.
type OpenResult int

const (
	Opened OpenResult = iota
	// UserEnded means the user ended this session from the browser and
	// reopen was not requested, so it stays ended.
	UserEnded
)

// OpenSession opens or resumes the session for a canonical file path. A
// session the user ended is not revived unless reopen is true.
func (s *Store) OpenSession(ctx context.Context, key, file string, reopen bool) (*Session, OpenResult, error) {
	var sess *Session
	result := Opened
	err := s.tx(ctx, func(tx *sql.Tx) error {
		existing, err := getSession(ctx, tx, key)
		now := s.millis()
		switch {
		case errors.Is(err, ErrNotFound):
			_, err := tx.ExecContext(ctx, `INSERT INTO sessions (key, file, created_at, updated_at) VALUES (?, ?, ?, ?)`, key, file, now, now)
			if err != nil {
				return err
			}
		case err != nil:
			return err
		case existing.Status == StatusEnded && existing.EndedBy == EndedByUser && !reopen:
			sess, result = existing, UserEnded
			return nil
		case existing.Status == StatusEnded:
			status := StatusOpen
			var pending int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM prompts WHERE session_key = ? AND delivered_at IS NULL`, key).Scan(&pending); err != nil {
				return err
			}
			if pending > 0 {
				status = StatusFeedback
			}
			// Working and Done belong to one review round: a later round's
			// edits and replies are about that round's notes.
			if _, err := tx.ExecContext(ctx, `UPDATE chat_entries SET receipt = ? WHERE session_key = ? AND role = 'user' AND receipt = ''`, ReceiptClosed, key); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET status = ?, ended_by = '', updated_at = ? WHERE key = ?`, status, now, key); err != nil {
				return err
			}
		}
		sess, err = getSession(ctx, tx, key)
		return err
	})
	return sess, result, err
}

// EndSession ends a session. A user end wins over an agent one, so a later
// agent end cannot turn a user end into a reopenable one.
func (s *Store) EndSession(ctx context.Context, key, by string) (*Session, error) {
	var sess *Session
	err := s.tx(ctx, func(tx *sql.Tx) error {
		existing, err := getSession(ctx, tx, key)
		if err != nil {
			return err
		}
		endedBy := by
		if existing.EndedBy == EndedByUser {
			endedBy = EndedByUser
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET status = ?, ended_by = ?, updated_at = ? WHERE key = ?`, StatusEnded, endedBy, s.millis(), key); err != nil {
			return err
		}
		sess, err = getSession(ctx, tx, key)
		return err
	})
	return sess, err
}

// BumpRevision records that the artifact file changed on disk.
func (s *Store) BumpRevision(ctx context.Context, key string) (*Session, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET artifact_revision = artifact_revision + 1, updated_at = ? WHERE key = ?`, s.millis(), key)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.Session(ctx, key)
}

func openSession(ctx context.Context, tx *sql.Tx, key string) (*Session, error) {
	sess, err := getSession(ctx, tx, key)
	if err != nil {
		return nil, err
	}
	if sess.Status == StatusEnded {
		return nil, ErrEnded
	}
	return sess, nil
}

type rowQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func queryQueue(ctx context.Context, q rowQuerier, key string) ([]QueueItem, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, session_key, kind, prompt, selector, tag, excerpt, target, queue_key, attachments FROM queue_items WHERE session_key = ? ORDER BY id`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueueItem
	for rows.Next() {
		var it QueueItem
		var target, attachments string
		if err := rows.Scan(&it.ID, &it.SessionKey, &it.Kind, &it.Prompt, &it.Selector, &it.Tag, &it.Excerpt, &target, &it.QueueKey, &attachments); err != nil {
			return nil, err
		}
		if target != "" {
			it.Target = json.RawMessage(target)
		}
		if err := json.Unmarshal([]byte(attachments), &it.Attachments); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Queue lists the session's queued notes, oldest first.
func (s *Store) Queue(ctx context.Context, key string) ([]QueueItem, error) {
	return queryQueue(ctx, s.db, key)
}

func encodeRefs(refs []AttachmentRef) (string, error) {
	if len(refs) > MaxAttachmentsPerPrompt {
		return "", ErrTooManyAttachments
	}
	clean := []AttachmentRef{}
	for _, r := range refs {
		if r.ID == "" {
			continue
		}
		clean = append(clean, AttachmentRef{ID: truncate(r.ID, 128), Name: bound(r.Name, 120)})
	}
	b, err := json.Marshal(clean)
	return string(b), err
}

// Enqueue queues a note. An existing item with the same non-empty QueueKey
// is replaced, so re-answering a question keeps one queued answer.
func (s *Store) Enqueue(ctx context.Context, item QueueItem) (int64, error) {
	item.Prompt = truncate(item.Prompt, promptMax)
	item.Selector = bound(item.Selector, selectorMax)
	item.Tag = bound(strings.ToLower(item.Tag), tagMax)
	item.Excerpt = bound(item.Excerpt, excerptMax*4)
	item.QueueKey = strings.TrimSpace(truncate(item.QueueKey, queueKeyMax))
	target := ""
	if len(item.Target) > 0 && len(item.Target) <= targetMax && json.Valid(item.Target) && string(item.Target) != "null" {
		target = string(item.Target)
	}
	attachments, err := encodeRefs(item.Attachments)
	if err != nil {
		return 0, err
	}

	var id int64
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := openSession(ctx, tx, item.SessionKey); err != nil {
			return err
		}
		if item.QueueKey != "" {
			if _, err := tx.ExecContext(ctx, `DELETE FROM queue_items WHERE session_key = ? AND queue_key = ?`, item.SessionKey, item.QueueKey); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO queue_items (session_key, kind, prompt, selector, tag, excerpt, target, queue_key, attachments, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			item.SessionKey, item.Kind, item.Prompt, item.Selector, item.Tag, item.Excerpt, target, item.QueueKey, attachments, s.millis())
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// RemoveQueued deletes one queued note.
func (s *Store) RemoveQueued(ctx context.Context, key string, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM queue_items WHERE session_key = ? AND id = ?`, key, id)
	return err
}

// EditQueued replaces a queued note's text, keeping what it is attached to.
func (s *Store) EditQueued(ctx context.Context, key string, id int64, prompt string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := openSession(ctx, tx, key); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE queue_items SET prompt = ? WHERE session_key = ? AND id = ?`, truncate(prompt, promptMax), key, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNoQueueItem
		}
		return nil
	})
}

// SendResult describes what SendQueue did.
type SendResult struct {
	Sent    int
	Ended   bool
	Session *Session
}

// SendQueue sends every queued note, plus message when it is not blank, to
// the agent in one transaction: each becomes a pending prompt and a
// conversation entry, and the queue is cleared. With end, the session also
// ends as user-initiated. A session that already ended refuses with
// ErrEnded, because no agent would ever receive the feedback.
func (s *Store) SendQueue(ctx context.Context, req SendRequest) (SendResult, error) {
	var result SendResult
	key, snapshot, end := req.Key, req.Snapshot, req.End
	message := strings.TrimSpace(truncate(req.Message, promptMax))
	if _, err := encodeRefs(req.MessageAttachments); err != nil {
		return result, err
	}
	if len(snapshot) > snapshotMax {
		snapshot = snapshot[:snapshotMax]
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := openSession(ctx, tx, key); err != nil {
			return err
		}
		items, err := queryQueue(ctx, tx, key)
		if err != nil {
			return err
		}
		if message != "" || len(req.MessageAttachments) > 0 {
			items = append(items, QueueItem{SessionKey: key, Kind: "message", Prompt: message, Attachments: req.MessageAttachments})
		}

		now := s.millis()
		for _, it := range items {
			prompt := PromptFor(it)
			for _, ref := range it.Attachments {
				if req.Describe == nil {
					continue
				}
				a, ok := req.Describe(ref)
				if !ok {
					return ErrAttachmentGone
				}
				prompt.Attachments = append(prompt.Attachments, a)
			}
			anchor := ""
			if a := AnchorFor(it); a != nil {
				b, _ := json.Marshal(a)
				anchor = string(b)
			}
			refs, err := encodeRefs(it.Attachments)
			if err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO chat_entries (session_key, role, kind, text, anchor, attachments, created_at) VALUES (?, 'user', ?, ?, ?, ?, ?)`, key, it.Kind, it.Prompt, anchor, refs, now)
			if err != nil {
				return err
			}
			chatID, err := res.LastInsertId()
			if err != nil {
				return err
			}
			payload, err := json.Marshal(prompt)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO prompts (session_key, payload, chat_entry_id, created_at) VALUES (?, ?, ?, ?)`, key, string(payload), chatID, now); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM queue_items WHERE session_key = ?`, key); err != nil {
			return err
		}

		switch {
		case end && len(items) > 0:
			_, err = tx.ExecContext(ctx, `UPDATE sessions SET status = ?, ended_by = ?, dom_snapshot = ?, updated_at = ? WHERE key = ?`, StatusEnded, EndedByUser, snapshot, now, key)
		case end:
			_, err = tx.ExecContext(ctx, `UPDATE sessions SET status = ?, ended_by = ?, updated_at = ? WHERE key = ?`, StatusEnded, EndedByUser, now, key)
		case len(items) > 0:
			_, err = tx.ExecContext(ctx, `UPDATE sessions SET status = ?, dom_snapshot = ?, updated_at = ? WHERE key = ?`, StatusFeedback, snapshot, now, key)
		}
		if err != nil {
			return err
		}
		result.Sent, result.Ended = len(items), end
		result.Session, err = getSession(ctx, tx, key)
		return err
	})
	return result, err
}

// SendRequest is a reviewer's Send (or Send & End).
type SendRequest struct {
	Key                string
	Message            string
	MessageAttachments []AttachmentRef
	Snapshot           string
	End                bool
	// Describe resolves attachments to local files for the agent.
	Describe Describer
}

// PromptFor is the prompt exactly as the agent receives it (attachments
// are resolved separately).
func PromptFor(it QueueItem) Prompt {
	if it.Kind == "message" {
		return Prompt{Prompt: it.Prompt, Kind: "message"}
	}
	return Prompt{Prompt: it.Prompt, Kind: it.Kind, Tag: it.Tag, Selector: it.Selector, Text: it.Excerpt, Target: it.Target}
}

// AnchorFor names what a queued note is attached to, or nil for a message.
func AnchorFor(it QueueItem) *Anchor {
	if it.Kind == "message" {
		return nil
	}
	var target struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		RowLabel    string `json:"rowLabel"`
		ColumnLabel string `json:"columnLabel"`
	}
	if len(it.Target) > 0 {
		_ = json.Unmarshal(it.Target, &target)
	}

	var a *Anchor
	switch target.Type {
	case "text-range":
		text := target.Text
		if text == "" {
			text = it.Excerpt
		}
		a = &Anchor{Kind: "text", Label: "text", Excerpt: bound(text, excerptMax)}
	case "table-cell":
		var parts []string
		for _, p := range []string{target.RowLabel, target.ColumnLabel} {
			if p = strings.TrimSpace(p); p != "" {
				parts = append(parts, p)
			}
		}
		if len(parts) > 0 {
			a = &Anchor{Kind: "cell", Label: "cell", Excerpt: bound(strings.Join(parts, " → "), excerptMax)}
		}
	}
	if a == nil && it.Tag != "" {
		a = &Anchor{Kind: "element", Label: bound("<"+it.Tag+">", labelMax), Excerpt: bound(it.Excerpt, excerptMax)}
	}
	if a != nil {
		a.Selector = bound(it.Selector, selectorMax)
	}
	return a
}

// Feedback is what a poll takes for the agent.
type Feedback struct {
	// Status is "feedback", "waiting", "ended" or "missing".
	Status           string
	Prompts          []Prompt
	DOMSnapshot      string
	ArtifactFailures []Failure
	SessionEnded     bool
	EndedBy          string

	delivery delivery
}

// delivery is what RestoreFeedback needs to undo a take.
type delivery struct {
	promptIDs   []int64
	chatIDs     []int64
	snapshot    string
	failures    []Failure
	deliveredAt int64
}

// TakeFeedback takes everything waiting for the agent: pending prompts, the
// DOM snapshot and fatal artifact failures, marking them delivered. Prompts
// sent with Send & End are still delivered (SessionEnded); the next take
// then reports "ended". If the response carrying the feedback cannot be
// written, pass it to RestoreFeedback.
func (s *Store) TakeFeedback(ctx context.Context, key string) (Feedback, error) {
	var fb Feedback
	err := s.tx(ctx, func(tx *sql.Tx) error {
		sess, err := getSession(ctx, tx, key)
		if errors.Is(err, ErrNotFound) {
			fb.Status = "missing"
			return nil
		}
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id, payload, chat_entry_id FROM prompts WHERE session_key = ? AND delivered_at IS NULL ORDER BY id`, key)
		if err != nil {
			return err
		}
		var ids, chatIDs []int64
		var prompts []Prompt
		for rows.Next() {
			var id int64
			var payload string
			var chatID sql.NullInt64
			if err := rows.Scan(&id, &payload, &chatID); err != nil {
				rows.Close()
				return err
			}
			if chatID.Valid {
				chatIDs = append(chatIDs, chatID.Int64)
			}
			var p Prompt
			if err := json.Unmarshal([]byte(payload), &p); err != nil {
				rows.Close()
				return err
			}
			ids, prompts = append(ids, id), append(prompts, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		ended := sess.Status == StatusEnded
		if len(prompts) == 0 && len(sess.ArtifactFailures) == 0 {
			if ended {
				fb.Status, fb.EndedBy = "ended", sess.EndedBy
			} else {
				fb.Status = "waiting"
			}
			return nil
		}

		now := s.millis()
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE prompts SET delivered_at = ? WHERE id = ?`, now, id); err != nil {
				return err
			}
		}
		for _, id := range chatIDs {
			if _, err := tx.ExecContext(ctx, `UPDATE chat_entries SET delivered_at = ? WHERE id = ? AND receipt = '' AND delivered_at IS NULL`, now, id); err != nil {
				return err
			}
		}
		status := StatusOpen
		if ended {
			status = StatusEnded
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET dom_snapshot = '', artifact_failures = '[]', status = ?, updated_at = ? WHERE key = ?`, status, now, key); err != nil {
			return err
		}
		fb = Feedback{
			Status:           "feedback",
			Prompts:          prompts,
			DOMSnapshot:      sess.DOMSnapshot,
			ArtifactFailures: sess.ArtifactFailures,
			SessionEnded:     ended,
			EndedBy:          sess.EndedBy,
			delivery:         delivery{promptIDs: ids, chatIDs: chatIDs, snapshot: sess.DOMSnapshot, failures: sess.ArtifactFailures, deliveredAt: now},
		}
		return nil
	})
	return fb, err
}

// RestoreFeedback puts back feedback whose delivery failed because the
// poll's client went away before the response was written. A newer snapshot
// or newer failures recorded since the take are kept.
func (s *Store) RestoreFeedback(ctx context.Context, key string, fb Feedback) error {
	d := fb.delivery
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, id := range d.promptIDs {
			if _, err := tx.ExecContext(ctx, `UPDATE prompts SET delivered_at = NULL WHERE id = ? AND delivered_at = ?`, id, d.deliveredAt); err != nil {
				return err
			}
		}
		// The take never reached the agent, so its Seen stamps go too.
		for _, id := range d.chatIDs {
			if _, err := tx.ExecContext(ctx, `UPDATE chat_entries SET delivered_at = NULL WHERE id = ? AND delivered_at = ?`, id, d.deliveredAt); err != nil {
				return err
			}
		}
		sess, err := getSession(ctx, tx, key)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		snapshot := sess.DOMSnapshot
		if snapshot == "" {
			snapshot = d.snapshot
		}
		failures, _ := json.Marshal(mergeFailures(d.failures, sess.ArtifactFailures))
		status := StatusFeedback
		if sess.Status == StatusEnded {
			status = StatusEnded
		}
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET dom_snapshot = ?, artifact_failures = ?, status = ?, updated_at = ? WHERE key = ?`, snapshot, string(failures), status, s.millis(), key)
		return err
	})
}

// RecordArtifactFailures records failures that make the review unusable.
// Unlike anything else the browser detects, these wake the agent's poll
// without a reviewer action. It reports whether the agent should be woken
// (false for an ended session).
func (s *Store) RecordArtifactFailures(ctx context.Context, key string, failures []Failure) (bool, error) {
	wake := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		sess, err := getSession(ctx, tx, key)
		if err != nil {
			return err
		}
		if sess.Status == StatusEnded {
			return nil
		}
		merged, _ := json.Marshal(mergeFailures(sess.ArtifactFailures, failures))
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET artifact_failures = ?, status = ?, updated_at = ? WHERE key = ?`, string(merged), StatusFeedback, s.millis(), key)
		wake = err == nil
		return err
	})
	return wake, err
}

func mergeFailures(earlier, later []Failure) []Failure {
	seen := map[Failure]bool{}
	var out []Failure
	for _, f := range append(append([]Failure{}, earlier...), later...) {
		f.Kind, f.Detail = truncate(f.Kind, 64), truncate(f.Detail, 1000)
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	if len(out) > maxArtifactFailures {
		out = out[len(out)-maxArtifactFailures:]
	}
	if out == nil {
		out = []Failure{}
	}
	return out
}

// PendingCount is how many sent prompts still wait for the agent.
func (s *Store) PendingCount(ctx context.Context, key string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM prompts WHERE session_key = ? AND delivered_at IS NULL`, key).Scan(&n)
	return n, err
}

// Transcript returns the session's last limit conversation entries, oldest
// first.
func (s *Store) Transcript(ctx context.Context, key string, limit int) ([]ChatEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, role, kind, text, anchor, attachments, created_at, delivered_at, working_at, done_at, receipt FROM (
		SELECT * FROM chat_entries WHERE session_key = ? ORDER BY id DESC LIMIT ?
	) ORDER BY id`, key, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatEntry
	for rows.Next() {
		var e ChatEntry
		var anchor, attachments string
		var created int64
		var delivered, working, done sql.NullInt64
		if err := rows.Scan(&e.ID, &e.Role, &e.Kind, &e.Text, &anchor, &attachments, &created, &delivered, &working, &done, &e.Receipt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(attachments), &e.Attachments); err != nil {
			return nil, err
		}
		for _, stamp := range []struct {
			v   sql.NullInt64
			dst *time.Time
		}{{delivered, &e.DeliveredAt}, {working, &e.WorkingAt}, {done, &e.DoneAt}} {
			if stamp.v.Valid {
				*stamp.dst = time.UnixMilli(stamp.v.Int64)
			}
		}
		if anchor != "" {
			e.Anchor = &Anchor{}
			if err := json.Unmarshal([]byte(anchor), e.Anchor); err != nil {
				return nil, err
			}
		}
		e.CreatedAt = time.UnixMilli(created)
		out = append(out, e)
	}
	return out, rows.Err()
}

// AddAgentReply adds the agent's reply to the conversation. It is refused
// once the session ended.
func (s *Store) AddAgentReply(ctx context.Context, key, text string) (int64, error) {
	text = strings.TrimRight(truncate(text, promptMax), " \t\r\n")
	if strings.TrimSpace(text) == "" {
		return 0, ErrEmpty
	}
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := openSession(ctx, tx, key); err != nil {
			return err
		}
		now := s.millis()
		res, err := tx.ExecContext(ctx, `INSERT INTO chat_entries (session_key, role, kind, text, created_at) VALUES (?, 'agent', 'message', ?, ?)`, key, text, now)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		// A reply concludes everything the agent has been handed.
		_, err = tx.ExecContext(ctx, `UPDATE chat_entries SET done_at = ? WHERE session_key = ? AND role = 'user' AND receipt = '' AND delivered_at IS NOT NULL AND done_at IS NULL`, now, key)
		return err
	})
	return id, err
}

// bound collapses whitespace runs, trims, and shortens to max runes with an
// ellipsis.
func bound(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

// truncate shortens s to at most max bytes without splitting a UTF-8
// sequence.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}
