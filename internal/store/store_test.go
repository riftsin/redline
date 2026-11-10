package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "redline.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustOpen(t *testing.T, s *Store, key string) *Session {
	t.Helper()
	sess, res, err := s.OpenSession(context.Background(), key, "/tmp/"+key+".html", false)
	if err != nil || res != Opened {
		t.Fatalf("open %s: %v %v", key, res, err)
	}
	return sess
}

func TestOpenUsesWALAndMigratesOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "redline.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v", mode, err)
	}
	s.Close()

	// Reopening an up-to-date database must not re-run migrations.
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version < 1 {
		t.Fatalf("user_version = %d, %v", version, err)
	}
}

func TestUserEndedSessionNeedsReopen(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustOpen(t, s, "k")

	if _, err := s.SendQueue(ctx, SendRequest{Key: "k", Message: "", Snapshot: "", End: true}); err != nil {
		t.Fatal(err)
	}
	sess, res, err := s.OpenSession(ctx, "k", "/tmp/k.html", false)
	if err != nil || res != UserEnded || sess.Status != StatusEnded {
		t.Fatalf("plain reopen of a user-ended session: %v %+v %v", res, sess, err)
	}
	// An agent end afterwards must not downgrade the user end.
	if sess, err := s.EndSession(ctx, "k", EndedByAgent); err != nil || sess.EndedBy != EndedByUser {
		t.Fatalf("agent end after user end: %+v %v", sess, err)
	}
	sess, res, err = s.OpenSession(ctx, "k", "/tmp/k.html", true)
	if err != nil || res != Opened || sess.Status != StatusOpen || sess.EndedBy != "" {
		t.Fatalf("--reopen: %v %+v %v", res, sess, err)
	}
}

func TestAgentEndedSessionReopensPlainly(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustOpen(t, s, "k")
	if _, err := s.EndSession(ctx, "k", EndedByAgent); err != nil {
		t.Fatal(err)
	}
	if sess, res, err := s.OpenSession(ctx, "k", "/tmp/k.html", false); err != nil || res != Opened || sess.Status != StatusOpen {
		t.Fatalf("reopen after agent end: %v %+v %v", res, sess, err)
	}
}

func TestQueueKeyReplaces(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustOpen(t, s, "k")
	for _, answer := range []string{"Postgres", "SQLite"} {
		if _, err := s.Enqueue(ctx, QueueItem{SessionKey: "k", Kind: "answer", Prompt: answer, QueueKey: "db"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Enqueue(ctx, QueueItem{SessionKey: "k", Kind: "annotation", Prompt: "note", Tag: "H2", Excerpt: "  Rollout \n plan "}); err != nil {
		t.Fatal(err)
	}
	q, err := s.Queue(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 2 || q[0].Prompt != "SQLite" || q[1].Tag != "h2" || q[1].Excerpt != "Rollout plan" {
		t.Fatalf("queue = %+v", q)
	}
}

func TestSendTakeRestoreDeliver(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustOpen(t, s, "k")
	target := json.RawMessage(`{"type":"text-range","text":"the quoted words"}`)
	if _, err := s.Enqueue(ctx, QueueItem{SessionKey: "k", Kind: "annotation", Prompt: "reword", Tag: "p", Selector: "main > p", Excerpt: "the quoted words and more", Target: target}); err != nil {
		t.Fatal(err)
	}

	if fb, _ := s.TakeFeedback(ctx, "k"); fb.Status != "waiting" {
		t.Fatalf("take before send = %q", fb.Status)
	}

	res, err := s.SendQueue(ctx, SendRequest{Key: "k", Message: "  also shorten it  ", Snapshot: "<snapshot/>", End: false})
	if err != nil || res.Sent != 2 || res.Session.Status != StatusFeedback {
		t.Fatalf("send: %+v %v", res, err)
	}
	if q, _ := s.Queue(ctx, "k"); len(q) != 0 {
		t.Fatalf("queue not cleared: %+v", q)
	}
	chat, _ := s.Transcript(ctx, "k", 100)
	if len(chat) != 2 || chat[0].Anchor == nil || chat[0].Anchor.Kind != "text" || chat[0].Anchor.Excerpt != "the quoted words" || chat[1].Anchor != nil || chat[1].Text != "also shorten it" {
		t.Fatalf("transcript = %+v", chat)
	}

	fb, err := s.TakeFeedback(ctx, "k")
	if err != nil || fb.Status != "feedback" || len(fb.Prompts) != 2 || fb.DOMSnapshot != "<snapshot/>" {
		t.Fatalf("take: %+v %v", fb, err)
	}
	if p := fb.Prompts[0]; p.Prompt != "reword" || p.Selector != "main > p" || string(p.Target) != string(target) {
		t.Fatalf("prompt 0 = %+v", p)
	}
	if p := fb.Prompts[1]; p.Kind != "message" || p.Prompt != "also shorten it" {
		t.Fatalf("prompt 1 = %+v", p)
	}
	if again, _ := s.TakeFeedback(ctx, "k"); again.Status != "waiting" {
		t.Fatalf("second take = %q, want waiting", again.Status)
	}

	// The response could not be written: everything comes back for the next poll.
	if err := s.RestoreFeedback(ctx, "k", fb); err != nil {
		t.Fatal(err)
	}
	redo, err := s.TakeFeedback(ctx, "k")
	if err != nil || redo.Status != "feedback" || len(redo.Prompts) != 2 || redo.DOMSnapshot != "<snapshot/>" {
		t.Fatalf("take after restore: %+v %v", redo, err)
	}
}

func TestSendDeliversSelectionAfterQueueAndBeforeMessage(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustOpen(t, s, "k")
	if _, err := s.Enqueue(ctx, QueueItem{SessionKey: "k", Kind: "annotation", Prompt: "queued first", Tag: "p", Selector: "p"}); err != nil {
		t.Fatal(err)
	}
	sel := &QueueItem{Kind: "answer", QueueKey: "ignored", Prompt: "  ", Tag: "DIV", Selector: "div#option-a", Excerpt: "Option A", Target: json.RawMessage(`not json`)}
	res, err := s.SendQueue(ctx, SendRequest{Key: "k", Message: "thanks", Selection: sel, End: true})
	if err != nil || res.Sent != 3 || !res.Ended {
		t.Fatalf("send: %+v %v", res, err)
	}
	fb, err := s.TakeFeedback(ctx, "k")
	if err != nil || len(fb.Prompts) != 3 {
		t.Fatalf("take: %+v %v", fb, err)
	}
	if p := fb.Prompts[1]; p.Kind != "annotation" || p.Prompt != "" || p.Tag != "div" || p.Selector != "div#option-a" || p.Text != "Option A" || len(p.Target) != 0 {
		t.Fatalf("selection prompt = %+v", p)
	}
	if fb.Prompts[0].Prompt != "queued first" || fb.Prompts[2].Kind != "message" {
		t.Fatalf("order = %+v", fb.Prompts)
	}
	chat, _ := s.Transcript(ctx, "k", 100)
	if len(chat) != 3 || chat[1].Anchor == nil || chat[1].Text != "" {
		t.Fatalf("transcript = %+v", chat)
	}
}

func TestRestoreKeepsNewerState(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustOpen(t, s, "k")
	if _, err := s.SendQueue(ctx, SendRequest{Key: "k", Message: "first", Snapshot: "old snapshot", End: false}); err != nil {
		t.Fatal(err)
	}
	fb, _ := s.TakeFeedback(ctx, "k")
	if _, err := s.SendQueue(ctx, SendRequest{Key: "k", Message: "second", Snapshot: "new snapshot", End: false}); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreFeedback(ctx, "k", fb); err != nil {
		t.Fatal(err)
	}
	all, _ := s.TakeFeedback(ctx, "k")
	if len(all.Prompts) != 2 || all.Prompts[0].Prompt != "first" || all.Prompts[1].Prompt != "second" || all.DOMSnapshot != "new snapshot" {
		t.Fatalf("after restore: %+v", all)
	}
}

func TestSendAndEndDeliversThenEnds(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustOpen(t, s, "k")
	if _, err := s.SendQueue(ctx, SendRequest{Key: "k", Message: "last words", Snapshot: "", End: true}); err != nil {
		t.Fatal(err)
	}
	fb, _ := s.TakeFeedback(ctx, "k")
	if fb.Status != "feedback" || !fb.SessionEnded || fb.EndedBy != EndedByUser || len(fb.Prompts) != 1 {
		t.Fatalf("final take: %+v", fb)
	}
	if next, _ := s.TakeFeedback(ctx, "k"); next.Status != "ended" || next.EndedBy != EndedByUser {
		t.Fatalf("take after final feedback: %+v", next)
	}
}

func TestEndedSessionRefusesWrites(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustOpen(t, s, "k")
	if _, err := s.EndSession(ctx, "k", EndedByAgent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(ctx, QueueItem{SessionKey: "k", Kind: "message", Prompt: "x"}); !errors.Is(err, ErrEnded) {
		t.Errorf("enqueue: %v", err)
	}
	if _, err := s.SendQueue(ctx, SendRequest{Key: "k", Message: "x", Snapshot: "", End: false}); !errors.Is(err, ErrEnded) {
		t.Errorf("send: %v", err)
	}
	if _, err := s.AddAgentReply(ctx, "k", "x"); !errors.Is(err, ErrEnded) {
		t.Errorf("reply: %v", err)
	}
	if _, err := s.AddAgentReply(ctx, "missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("reply to missing session: %v", err)
	}
}

func TestAgentReply(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustOpen(t, s, "k")
	if _, err := s.AddAgentReply(ctx, "k", " \n "); !errors.Is(err, ErrEmpty) {
		t.Fatalf("blank reply: %v", err)
	}
	if _, err := s.AddAgentReply(ctx, "k", "Done:\n- moved the section\n\n"); err != nil {
		t.Fatal(err)
	}
	chat, _ := s.Transcript(ctx, "k", 10)
	if len(chat) != 1 || chat[0].Role != "agent" || chat[0].Text != "Done:\n- moved the section" {
		t.Fatalf("transcript = %+v", chat)
	}
}

func TestArtifactFailures(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustOpen(t, s, "k")
	f := Failure{Kind: "asset", Detail: "style.css 404"}
	for range 3 {
		if wake, err := s.RecordArtifactFailures(ctx, "k", []Failure{f}); err != nil || !wake {
			t.Fatalf("record: %v %v", wake, err)
		}
	}
	fb, _ := s.TakeFeedback(ctx, "k")
	if fb.Status != "feedback" || len(fb.ArtifactFailures) != 1 || len(fb.Prompts) != 0 {
		t.Fatalf("take: %+v", fb)
	}

	if _, err := s.EndSession(ctx, "k", EndedByAgent); err != nil {
		t.Fatal(err)
	}
	if wake, err := s.RecordArtifactFailures(ctx, "k", []Failure{f}); err != nil || wake {
		t.Fatalf("failures on an ended session must not wake the agent: %v %v", wake, err)
	}
}

func TestAnchorFor(t *testing.T) {
	cell := AnchorFor(QueueItem{Kind: "annotation", Tag: "td", Excerpt: "12ms", Target: json.RawMessage(`{"type":"table-cell","rowLabel":"Redis","columnLabel":"Latency"}`)})
	if cell == nil || cell.Kind != "cell" || cell.Excerpt != "Redis → Latency" {
		t.Fatalf("cell anchor = %+v", cell)
	}
	unnamed := AnchorFor(QueueItem{Kind: "annotation", Tag: "td", Excerpt: "12ms", Target: json.RawMessage(`{"type":"table-cell"}`)})
	if unnamed == nil || unnamed.Kind != "element" || unnamed.Label != "<td>" {
		t.Fatalf("unnamed cell falls back to the element: %+v", unnamed)
	}
	long := AnchorFor(QueueItem{Kind: "annotation", Tag: "p", Excerpt: strings.Repeat("word ", 100)})
	if n := len([]rune(long.Excerpt)); n != excerptMax || !strings.HasSuffix(long.Excerpt, "…") {
		t.Fatalf("excerpt is %d runes: %q", n, long.Excerpt)
	}
	if AnchorFor(QueueItem{Kind: "message", Prompt: "hi"}) != nil {
		t.Fatal("messages have no anchor")
	}
}

func TestDeliveryReceipts(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustOpen(t, s, "k")
	receipt := func() ChatEntry {
		t.Helper()
		chat, err := s.Transcript(ctx, "k", 10)
		if err != nil || len(chat) == 0 {
			t.Fatalf("transcript: %v %v", chat, err)
		}
		return chat[0]
	}

	if _, err := s.SendQueue(ctx, SendRequest{Key: "k", Message: "make it blue"}); err != nil {
		t.Fatal(err)
	}
	if e := receipt(); e.Receipt != ReceiptLive || !e.DeliveredAt.IsZero() {
		t.Fatalf("sent, not yet seen: %+v", e)
	}
	// Working needs a delivery first.
	if worked, _ := s.MarkWorking(ctx, "k"); worked {
		t.Fatal("an undelivered note cannot be worked on")
	}

	fb, _ := s.TakeFeedback(ctx, "k")
	if e := receipt(); e.DeliveredAt.IsZero() {
		t.Fatalf("taken by a poll but not Seen: %+v", e)
	}
	// The poll's response never reached the agent: Seen is undone.
	if err := s.RestoreFeedback(ctx, "k", fb); err != nil {
		t.Fatal(err)
	}
	if e := receipt(); !e.DeliveredAt.IsZero() {
		t.Fatalf("restored delivery still Seen: %+v", e)
	}

	if _, err := s.TakeFeedback(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if worked, _ := s.MarkWorking(ctx, "k"); !worked {
		t.Fatal("artifact change after delivery should mark Working")
	}
	if _, err := s.AddAgentReply(ctx, "k", "Blue now."); err != nil {
		t.Fatal(err)
	}
	if e := receipt(); e.WorkingAt.IsZero() || e.DoneAt.IsZero() {
		t.Fatalf("after reply: %+v", e)
	}

	// A note the agent ends a session on stays Seen; reopening freezes it.
	if _, err := s.SendQueue(ctx, SendRequest{Key: "k", Message: "one more"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TakeFeedback(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EndSession(ctx, "k", EndedByAgent); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.OpenSession(ctx, "k", "/tmp/k.html", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendQueue(ctx, SendRequest{Key: "k", Message: "new round"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TakeFeedback(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAgentReply(ctx, "k", "Done with the new round."); err != nil {
		t.Fatal(err)
	}
	chat, _ := s.Transcript(ctx, "k", 10)
	for _, e := range chat {
		switch e.Text {
		case "one more":
			if e.Receipt != ReceiptClosed || !e.DoneAt.IsZero() {
				t.Errorf("the previous round's note: %+v", e)
			}
		case "new round":
			if e.Receipt != ReceiptLive || e.DoneAt.IsZero() {
				t.Errorf("this round's note: %+v", e)
			}
		}
	}
}

func TestEditQueued(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustOpen(t, s, "k")
	id, _ := s.Enqueue(ctx, QueueItem{SessionKey: "k", Kind: "annotation", Prompt: "old", Tag: "h1", Selector: "h1"})
	if err := s.EditQueued(ctx, "k", id, "new text"); err != nil {
		t.Fatal(err)
	}
	q, _ := s.Queue(ctx, "k")
	if len(q) != 1 || q[0].Prompt != "new text" || q[0].Selector != "h1" {
		t.Fatalf("queue = %+v", q)
	}
	if err := s.EditQueued(ctx, "k", id+1, "x"); !errors.Is(err, ErrNoQueueItem) {
		t.Fatalf("edit of a missing note: %v", err)
	}
}

func TestAttachmentsTravelWithPrompts(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustOpen(t, s, "k")
	ref := AttachmentRef{ID: strings.Repeat("a", 64), Name: "shot.png"}
	if _, err := s.Enqueue(ctx, QueueItem{SessionKey: "k", Kind: "annotation", Prompt: "see", Attachments: []AttachmentRef{ref}}); err != nil {
		t.Fatal(err)
	}
	describe := func(r AttachmentRef) (PromptAttachment, bool) {
		return PromptAttachment{ID: r.ID, Name: r.Name, Path: "/state/attachments/" + r.ID + ".png", Mime: "image/png"}, true
	}
	if _, err := s.SendQueue(ctx, SendRequest{Key: "k", Describe: describe}); err != nil {
		t.Fatal(err)
	}
	fb, _ := s.TakeFeedback(ctx, "k")
	if len(fb.Prompts) != 1 || len(fb.Prompts[0].Attachments) != 1 || fb.Prompts[0].Attachments[0].Path == "" {
		t.Fatalf("prompt = %+v", fb.Prompts)
	}
	chat, _ := s.Transcript(ctx, "k", 10)
	if len(chat[0].Attachments) != 1 || chat[0].Attachments[0].Name != "shot.png" {
		t.Fatalf("chat = %+v", chat[0])
	}

	// A gone image refuses the send instead of losing the note silently.
	if _, err := s.Enqueue(ctx, QueueItem{SessionKey: "k", Kind: "annotation", Prompt: "x", Attachments: []AttachmentRef{ref}}); err != nil {
		t.Fatal(err)
	}
	gone := func(AttachmentRef) (PromptAttachment, bool) { return PromptAttachment{}, false }
	if _, err := s.SendQueue(ctx, SendRequest{Key: "k", Describe: gone}); !errors.Is(err, ErrAttachmentGone) {
		t.Fatalf("send with a gone image: %v", err)
	}
	if q, _ := s.Queue(ctx, "k"); len(q) != 1 {
		t.Fatal("a refused send must keep the queue")
	}
	tooMany := make([]AttachmentRef, MaxAttachmentsPerPrompt+1)
	for i := range tooMany {
		tooMany[i] = ref
	}
	if _, err := s.Enqueue(ctx, QueueItem{SessionKey: "k", Kind: "annotation", Attachments: tooMany}); !errors.Is(err, ErrTooManyAttachments) {
		t.Fatalf("too many images: %v", err)
	}
}

func TestRecordChange(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustOpen(t, s, "k")
	if added, err := s.RecordChange(ctx, "k", Change{Revision: 1, Selectors: []string{"h1#title", ""}}); err != nil || !added {
		t.Fatalf("record: %v %v", added, err)
	}
	if added, _ := s.RecordChange(ctx, "k", Change{Revision: 1, Rewrite: true}); added {
		t.Fatal("the first report of a revision wins")
	}
	changes, _ := s.Changes(ctx, "k")
	if len(changes) != 1 || len(changes[0].Selectors) != 1 || changes[0].Rewrite {
		t.Fatalf("changes = %+v", changes)
	}
}
