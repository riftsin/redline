package review

import (
	"context"
	"errors"
	"time"

	"github.com/riftsin/redline/internal/store"
)

// Poll outcomes besides delivered feedback.
const (
	OutcomeFeedback            = "feedback"
	OutcomeEnded               = "ended"
	OutcomeMissing             = "missing"
	OutcomeTimeout             = "waiting"
	OutcomeBrowserDisconnected = "browser_disconnected"
	OutcomeReplaced            = "replaced"
)

// PollRequest describes one `redline poll`.
type PollRequest struct {
	Key, File string
	Owner     string
	Takeover  bool
	// AgentReply, when non-empty, is posted to the conversation once the
	// poll holds the session, concluding the agent's delivered work.
	AgentReply string
	// Timeout is a test/debug escape hatch; zero waits indefinitely.
	Timeout time.Duration
	// Heartbeat, when set, is called every HeartbeatEvery while waiting; an
	// error means the client went away.
	Heartbeat      func() error
	HeartbeatEvery time.Duration
	// Waiting, when set, is called once the poll is about to wait (there
	// was nothing to deliver right away).
	Waiting func()
}

// PollResult is how a poll ended.
type PollResult struct {
	Outcome  string
	Feedback store.Feedback // OutcomeFeedback only
	EndedBy  string         // OutcomeEnded only
}

// ErrClientGone means the poll's client disconnected before an answer.
var ErrClientGone = errors.New("poll client went away")

// Poll claims the session's poll, waits until there is something to tell
// the agent, and hands it to deliver. Feedback is marked delivered when
// taken; if deliver fails it is restored for the next poll, so nothing the
// reviewer sent is ever lost.
func (m *Manager) Poll(ctx context.Context, req PollRequest, deliver func(store.Feedback) error) (PollResult, error) {
	// Subscribe before claiming and taking, so no wake-up slips between.
	sub := m.Bus.Subscribe(req.Key)
	defer sub.Close()

	token, err := m.ClaimPoll(req.Key, req.File, req.Owner, req.Takeover)
	if err != nil {
		return PollResult{}, err
	}
	if req.AgentReply != "" {
		if _, err := m.Store.AddAgentReply(ctx, req.Key, req.AgentReply); err == nil {
			m.AgentReplied(req.Key)
			m.Bus.Publish(Event{Type: TranscriptChanged, Key: req.Key})
		}
	}

	var timeout <-chan time.Time
	if req.Timeout > 0 {
		t := time.NewTimer(req.Timeout)
		defer t.Stop()
		timeout = t.C
	}
	var beat <-chan time.Time
	if req.Heartbeat != nil && req.HeartbeatEvery > 0 {
		t := time.NewTicker(req.HeartbeatEvery)
		defer t.Stop()
		beat = t.C
	}

	announced, disconnected, timedOut := false, false, false
	for {
		fb, err := m.Store.TakeFeedback(context.WithoutCancel(ctx), req.Key)
		if err != nil {
			m.ReleasePoll(req.Key, token, false)
			return PollResult{}, err
		}
		switch fb.Status {
		case "feedback":
			if err := deliver(fb); err != nil {
				_ = m.Store.RestoreFeedback(context.WithoutCancel(ctx), req.Key, fb)
				m.ReleasePoll(req.Key, token, false)
				m.Bus.Publish(Event{Type: FeedbackReady, Key: req.Key})
				return PollResult{}, ErrClientGone
			}
			m.ReleasePoll(req.Key, token, !fb.SessionEnded)
			return PollResult{Outcome: OutcomeFeedback, Feedback: fb}, nil
		case "ended":
			m.ReleasePoll(req.Key, token, false)
			return PollResult{Outcome: OutcomeEnded, EndedBy: fb.EndedBy}, nil
		case "missing":
			m.ReleasePoll(req.Key, token, false)
			return PollResult{Outcome: OutcomeMissing}, nil
		}
		switch {
		case disconnected:
			m.ReleasePoll(req.Key, token, false)
			return PollResult{Outcome: OutcomeBrowserDisconnected}, nil
		case timedOut:
			m.ReleasePoll(req.Key, token, false)
			return PollResult{Outcome: OutcomeTimeout}, nil
		}
		if !announced {
			announced = true
			if req.Waiting != nil {
				req.Waiting()
			}
		}

		// Wait for the next change.
	wait:
		for {
			select {
			case ev := <-sub.C:
				switch {
				case ev.Type == PollReplaced && ev.Token == token:
					// The new holder owns the session now; nothing to release.
					return PollResult{Outcome: OutcomeReplaced}, nil
				case ev.Type == BrowserDisconnected:
					disconnected = true
					break wait
				case ev.Type == FeedbackReady || ev.Type == SessionChanged:
					break wait
				}
			case <-beat:
				if err := req.Heartbeat(); err != nil {
					m.ReleasePoll(req.Key, token, false)
					return PollResult{}, ErrClientGone
				}
			case <-timeout:
				timedOut = true
				break wait
			case <-ctx.Done():
				m.ReleasePoll(req.Key, token, false)
				return PollResult{}, ErrClientGone
			}
		}
	}
}
