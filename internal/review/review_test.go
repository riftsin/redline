package review

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/riftsin/redline/internal/store"
)

type fixture struct {
	m         *Manager
	key, file string
}

func newFixture(t *testing.T, grace time.Duration) fixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "redline.db"))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "plan.html")
	if err := os.WriteFile(file, []byte("<p>v1</p>"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(st, grace)
	t.Cleanup(func() { m.Close(); st.Close() })
	f := fixture{m: m, key: "k", file: file}
	if _, _, err := m.Open(context.Background(), f.key, f.file, false); err != nil {
		t.Fatal(err)
	}
	return f
}

type pollReturn struct {
	res       PollResult
	err       error
	delivered []store.Feedback
}

// startPoll runs a poll in the background and reports once it waits (or
// returns without waiting).
func (f fixture) startPoll(t *testing.T, ctx context.Context, req PollRequest) <-chan pollReturn {
	t.Helper()
	req.Key, req.File = f.key, f.file
	waiting := make(chan struct{})
	req.Waiting = func() { close(waiting) }
	out := make(chan pollReturn, 1)
	go func() {
		var r pollReturn
		r.res, r.err = f.m.Poll(ctx, req, func(fb store.Feedback) error {
			r.delivered = append(r.delivered, fb)
			return nil
		})
		out <- r
	}()
	select {
	case <-waiting:
	case r := <-out:
		out <- r
	case <-time.After(5 * time.Second):
		t.Fatal("poll neither waited nor returned")
	}
	return out
}

func await(t *testing.T, c <-chan pollReturn) pollReturn {
	t.Helper()
	select {
	case r := <-c:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not return")
	}
	return pollReturn{}
}

func TestPollWakesOnSendAndMarksWorking(t *testing.T) {
	f := newFixture(t, time.Minute)
	ctx := context.Background()
	done := f.startPoll(t, ctx, PollRequest{Owner: "codex"})
	if p := f.m.Presence(f.key); p.State != Listening || p.Owner != "codex" {
		t.Fatalf("presence while waiting = %+v", p)
	}

	if _, err := f.m.Send(ctx, store.SendRequest{Key: f.key, Message: "make it blue"}); err != nil {
		t.Fatal(err)
	}
	r := await(t, done)
	if r.err != nil || r.res.Outcome != OutcomeFeedback || len(r.delivered) != 1 || r.delivered[0].Prompts[0].Prompt != "make it blue" {
		t.Fatalf("poll = %+v", r)
	}
	if p := f.m.Presence(f.key); p.State != Working {
		t.Fatalf("presence after delivery = %+v", p)
	}

	// The agent's reply on the next poll concludes the work.
	again := f.startPoll(t, ctx, PollRequest{AgentReply: "Done, it is blue now."})
	if p := f.m.Presence(f.key); p.State != Listening {
		t.Fatalf("presence after reply = %+v", p)
	}
	chat, _ := f.m.Store.Transcript(ctx, f.key, 10)
	if last := chat[len(chat)-1]; last.Role != "agent" || last.Text != "Done, it is blue now." {
		t.Fatalf("transcript tail = %+v", last)
	}
	if _, err := f.m.End(ctx, f.key, store.EndedByAgent); err != nil {
		t.Fatal(err)
	}
	if r := await(t, again); r.res.Outcome != OutcomeEnded || r.res.EndedBy != store.EndedByAgent {
		t.Fatalf("poll after end = %+v", r)
	}
}

func TestPollReturnsQueuedFeedbackImmediately(t *testing.T) {
	f := newFixture(t, time.Minute)
	ctx := context.Background()
	if _, err := f.m.Send(ctx, store.SendRequest{Key: f.key, Message: "already sent"}); err != nil {
		t.Fatal(err)
	}
	r := await(t, f.startPoll(t, ctx, PollRequest{}))
	if r.res.Outcome != OutcomeFeedback {
		t.Fatalf("poll = %+v", r)
	}
}

func TestSecondPollRefusedUnlessTakeover(t *testing.T) {
	f := newFixture(t, time.Minute)
	ctx := context.Background()
	first := f.startPoll(t, ctx, PollRequest{Owner: "first"})

	var active *ListenerActiveError
	_, err := f.m.Poll(ctx, PollRequest{Key: f.key, File: f.file}, func(store.Feedback) error { return nil })
	if !errors.As(err, &active) || active.Label != "first" {
		t.Fatalf("second poll: %v", err)
	}

	second := f.startPoll(t, ctx, PollRequest{Owner: "second", Takeover: true})
	if r := await(t, first); r.res.Outcome != OutcomeReplaced {
		t.Fatalf("displaced poll = %+v", r)
	}
	if p := f.m.Presence(f.key); p.Owner != "second" {
		t.Fatalf("the displaced poll's exit must not clear the new holder: %+v", p)
	}
	if _, err := f.m.Send(ctx, store.SendRequest{Key: f.key, Message: "for the second"}); err != nil {
		t.Fatal(err)
	}
	if r := await(t, second); r.res.Outcome != OutcomeFeedback {
		t.Fatalf("taking-over poll = %+v", r)
	}
}

func TestFailedDeliveryIsRestored(t *testing.T) {
	f := newFixture(t, time.Minute)
	ctx := context.Background()
	if _, err := f.m.Send(ctx, store.SendRequest{Key: f.key, Message: "do not lose me"}); err != nil {
		t.Fatal(err)
	}
	_, err := f.m.Poll(ctx, PollRequest{Key: f.key, File: f.file}, func(store.Feedback) error { return errors.New("broken pipe") })
	if !errors.Is(err, ErrClientGone) {
		t.Fatalf("poll error = %v", err)
	}
	if p := f.m.Presence(f.key); p.State != Idle {
		t.Fatalf("presence after failed delivery = %+v", p)
	}
	r := await(t, f.startPoll(t, ctx, PollRequest{}))
	if r.res.Outcome != OutcomeFeedback || r.delivered[0].Prompts[0].Prompt != "do not lose me" {
		t.Fatalf("next poll = %+v", r)
	}
}

func TestClientGoneReleasesPoll(t *testing.T) {
	f := newFixture(t, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	done := f.startPoll(t, ctx, PollRequest{})
	cancel()
	if r := await(t, done); !errors.Is(r.err, ErrClientGone) {
		t.Fatalf("poll = %+v", r)
	}
	if p := f.m.Presence(f.key); p.State != Idle {
		t.Fatalf("presence = %+v", p)
	}
}

func TestDisconnectGrace(t *testing.T) {
	f := newFixture(t, 50*time.Millisecond)
	ctx := context.Background()

	// A poll that starts before any page connected keeps waiting.
	done := f.startPoll(t, ctx, PollRequest{})
	leave := f.m.ReviewerConnected(f.key, f.file)

	// A reload (leave and come back within the grace) keeps the poll.
	leave()
	back := f.m.ReviewerConnected(f.key, f.file)
	time.Sleep(150 * time.Millisecond)
	select {
	case r := <-done:
		t.Fatalf("poll returned during a reload: %+v", r)
	default:
	}

	back()
	if r := await(t, done); r.res.Outcome != OutcomeBrowserDisconnected {
		t.Fatalf("poll after every page left = %+v", r)
	}
	if sess, _ := f.m.Store.Session(ctx, f.key); sess.Status == store.StatusEnded {
		t.Fatal("a disconnect must not end the session")
	}
}

func TestDisconnectGraceOff(t *testing.T) {
	f := newFixture(t, 0)
	done := f.startPoll(t, context.Background(), PollRequest{Timeout: 200 * time.Millisecond})
	f.m.ReviewerConnected(f.key, f.file)()
	if r := await(t, done); r.res.Outcome != OutcomeTimeout {
		t.Fatalf("with grace off the poll must wait for its own timeout, got %+v", r)
	}
}

func TestArtifactChangeBumpsRevision(t *testing.T) {
	f := newFixture(t, time.Minute)
	sub := f.m.Bus.Subscribe(f.key)
	defer sub.Close()
	if err := os.WriteFile(f.file, []byte("<p>v2</p>"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-sub.C:
			if ev.Type == Reload {
				sess, _ := f.m.Store.Session(context.Background(), f.key)
				if sess.ArtifactRevision != 1 {
					t.Fatalf("revision = %d", sess.ArtifactRevision)
				}
				return
			}
		case <-deadline:
			t.Fatal("no reload event")
		}
	}
}

func TestDisconnectGraceParsing(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"":       DefaultDisconnectGrace,
		"junk":   DefaultDisconnectGrace,
		"-5":     DefaultDisconnectGrace,
		"0":      0,
		"OFF":    0,
		" 1500 ": 1500 * time.Millisecond,
	} {
		if got := DisconnectGrace(in); got != want {
			t.Errorf("DisconnectGrace(%q) = %v, want %v", in, got, want)
		}
	}
}
