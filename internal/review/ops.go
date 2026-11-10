package review

import (
	"context"

	"github.com/riftsin/redline/internal/store"
)

// The operations below change a session and announce the change on the
// bus. Every write that review pages or polls must notice goes through
// here, so no caller can forget to publish.

// Open opens or resumes a session and starts watching its file.
func (m *Manager) Open(ctx context.Context, key, file string, reopen bool) (*store.Session, store.OpenResult, error) {
	sess, res, err := m.Store.OpenSession(ctx, key, file, reopen)
	if err != nil {
		return nil, 0, err
	}
	m.Track(key, file)
	if res == store.Opened {
		m.Bus.Publish(Event{Type: SessionChanged, Key: key})
	}
	return sess, res, nil
}

// End ends a session as the user or the agent and wakes a waiting poll.
func (m *Manager) End(ctx context.Context, key, by string) (*store.Session, error) {
	sess, err := m.Store.EndSession(ctx, key, by)
	if err != nil {
		return nil, err
	}
	m.Bus.Publish(Event{Type: SessionChanged, Key: key})
	m.Bus.Publish(Event{Type: FeedbackReady, Key: key})
	return sess, nil
}

// Enqueue queues a note for the reviewer's next send.
func (m *Manager) Enqueue(ctx context.Context, item store.QueueItem) (int64, error) {
	id, err := m.Store.Enqueue(ctx, item)
	if err != nil {
		return 0, err
	}
	m.Bus.Publish(Event{Type: QueueChanged, Key: item.SessionKey})
	return id, nil
}

// RemoveQueued drops a queued note.
func (m *Manager) RemoveQueued(ctx context.Context, key string, id int64) error {
	if err := m.Store.RemoveQueued(ctx, key, id); err != nil {
		return err
	}
	m.Bus.Publish(Event{Type: QueueChanged, Key: key})
	return nil
}

// Send delivers the queue (and message) to the agent; with end, it also
// ends the session as the user.
func (m *Manager) Send(ctx context.Context, key, message, snapshot string, end bool) (store.SendResult, error) {
	res, err := m.Store.SendQueue(ctx, key, message, snapshot, end)
	if err != nil {
		return res, err
	}
	m.Bus.Publish(Event{Type: QueueChanged, Key: key})
	if res.Sent > 0 {
		m.Bus.Publish(Event{Type: TranscriptChanged, Key: key})
	}
	m.Bus.Publish(Event{Type: SessionChanged, Key: key})
	if res.Sent > 0 || end {
		m.Bus.Publish(Event{Type: FeedbackReady, Key: key})
	}
	return res, nil
}

// Reply posts an agent reply outside a poll (`redline reply`).
func (m *Manager) Reply(ctx context.Context, key, text string) error {
	if _, err := m.Store.AddAgentReply(ctx, key, text); err != nil {
		return err
	}
	m.AgentReplied(key)
	m.Bus.Publish(Event{Type: TranscriptChanged, Key: key})
	return nil
}

// RecordArtifactFailures records failures that leave nothing to review and
// wakes the agent's poll.
func (m *Manager) RecordArtifactFailures(ctx context.Context, key string, failures []store.Failure) error {
	wake, err := m.Store.RecordArtifactFailures(ctx, key, failures)
	if err != nil {
		return err
	}
	if wake {
		m.Bus.Publish(Event{Type: FeedbackReady, Key: key})
	}
	return nil
}
