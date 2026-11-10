// Package review is the live side of review sessions: the event bus every
// open review page and waiting poll listens on, poll ownership, agent
// presence, connected reviewers and the disconnect grace period.
package review

import "sync"

// EventType names what changed in a session.
type EventType int

const (
	// QueueChanged: the reviewer's queued notes changed.
	QueueChanged EventType = iota
	// TranscriptChanged: the conversation gained or changed entries.
	TranscriptChanged
	// SessionChanged: status, ended_by or revision changed.
	SessionChanged
	// FeedbackReady: feedback (or a fatal artifact failure, or an end)
	// waits for the agent's poll.
	FeedbackReady
	// PresenceChanged: the agent's presence changed.
	PresenceChanged
	// BrowserDisconnected: every review page stayed away past the grace
	// period while a poll was waiting.
	BrowserDisconnected
	// Reload: the artifact file changed on disk.
	Reload
	// PollReplaced: another poll took over; Token names the displaced one.
	PollReplaced
)

// Event is one notification on a session's bus.
type Event struct {
	Type  EventType
	Key   string
	Token uint64 // PollReplaced only
}

// Bus fans events out to a session's subscribers. Delivery never blocks the
// publisher: each subscriber has a buffer, and a subscriber that falls
// behind loses its oldest events rather than stalling the session.
// Subscribers treat every event as a "something changed" signal and re-read
// the state they care about, so a lost event never loses a state change as
// long as a later one arrives.
type Bus struct {
	mu   sync.Mutex
	subs map[string]map[*Subscription]struct{}
}

// Subscription receives one session's events.
type Subscription struct {
	C   <-chan Event
	c   chan Event
	key string
	bus *Bus
}

// NewBus returns an empty bus.
func NewBus() *Bus {
	return &Bus{subs: map[string]map[*Subscription]struct{}{}}
}

// Subscribe starts receiving key's events. Call Close when done.
func (b *Bus) Subscribe(key string) *Subscription {
	c := make(chan Event, 64)
	s := &Subscription{C: c, c: c, key: key, bus: b}
	b.mu.Lock()
	if b.subs[key] == nil {
		b.subs[key] = map[*Subscription]struct{}{}
	}
	b.subs[key][s] = struct{}{}
	b.mu.Unlock()
	return s
}

// Close stops the subscription.
func (s *Subscription) Close() {
	b := s.bus
	b.mu.Lock()
	delete(b.subs[s.key], s)
	if len(b.subs[s.key]) == 0 {
		delete(b.subs, s.key)
	}
	b.mu.Unlock()
}

// Publish sends ev to every subscriber of ev.Key.
func (b *Bus) Publish(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs[ev.Key] {
		select {
		case s.c <- ev:
		default:
			// The subscriber is not keeping up. Drop the oldest event to
			// make room so the newest state change is never the one lost.
			select {
			case <-s.c:
			default:
			}
			select {
			case s.c <- ev:
			default:
			}
		}
	}
}
