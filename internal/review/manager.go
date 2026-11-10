package review

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/riftsin/redline/internal/store"
	"github.com/riftsin/redline/internal/watch"
)

// DefaultDisconnectGrace is how long a waiting poll keeps waiting after the
// last review page disconnected. It is long enough to survive a phone
// locking its screen and short enough to release an agent whose reviewer
// walked away.
const DefaultDisconnectGrace = 5 * time.Minute

// DisconnectGrace reads REDLINE_DISCONNECT_GRACE_MS: milliseconds, or "0"
// or "off" to never release a poll on disconnect. Empty or invalid values
// keep the default.
func DisconnectGrace(value string) time.Duration {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "off" {
		return 0
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil || ms < 0 {
		return DefaultDisconnectGrace
	}
	return time.Duration(ms) * time.Millisecond
}

// Presence states.
const (
	// Idle: no poll waits and no delivered feedback is being worked on.
	Idle = "idle"
	// Listening: a poll waits for feedback.
	Listening = "listening"
	// Working: a poll delivered feedback and the agent has not replied or
	// polled again yet.
	Working = "working"
)

// Presence is what the review page shows about the agent.
type Presence struct {
	State string
	Owner string // the listening poll's --owner label, if any
}

// Manager owns the live state of every session the server has touched.
type Manager struct {
	Store *store.Store
	Bus   *Bus
	Grace time.Duration

	mu        sync.Mutex
	sessions  map[string]*live
	nextToken uint64
}

type live struct {
	key, file string

	poll      *holder
	delivered bool
	reviewers int
	grace     *time.Timer
	stopWatch func()
}

type holder struct {
	token uint64
	owner string
}

// NewManager returns a manager over st.
func NewManager(st *store.Store, grace time.Duration) *Manager {
	return &Manager{Store: st, Bus: NewBus(), Grace: grace, sessions: map[string]*live{}}
}

// Close stops every file watcher and timer.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.sessions {
		if l.stopWatch != nil {
			l.stopWatch()
		}
		if l.grace != nil {
			l.grace.Stop()
		}
	}
	m.sessions = map[string]*live{}
}

// ensure returns the live state for a session, starting its file watcher.
// m.mu must be held.
func (m *Manager) ensure(key, file string) *live {
	if l, ok := m.sessions[key]; ok {
		return l
	}
	l := &live{key: key, file: file}
	l.stopWatch = watch.File(file, func() { m.artifactChanged(key) })
	m.sessions[key] = l
	return l
}

// Track makes sure the session's live state (and file watcher) exists.
func (m *Manager) Track(key, file string) {
	m.mu.Lock()
	m.ensure(key, file)
	m.mu.Unlock()
}

func (m *Manager) artifactChanged(key string) {
	if _, err := m.Store.BumpRevision(context.Background(), key); err != nil {
		log.Printf("record artifact change for %s: %v", key, err)
		return
	}
	m.Bus.Publish(Event{Type: Reload, Key: key})
	m.Bus.Publish(Event{Type: SessionChanged, Key: key})
}

// ListenerActiveError refuses a second poll on a session.
type ListenerActiveError struct {
	Label string
}

func (e *ListenerActiveError) Error() string {
	return "another poll (" + e.Label + ") is already listening for this session"
}

func label(owner string) string {
	if owner == "" {
		return "agent-listener"
	}
	return owner
}

// ClaimPoll makes the caller the session's only poll listener and returns
// its token. With takeover, the current holder is displaced (and told so
// with PollReplaced); without it, a held session refuses with
// *ListenerActiveError.
func (m *Manager) ClaimPoll(key, file, owner string, takeover bool) (uint64, error) {
	m.mu.Lock()
	l := m.ensure(key, file)
	previous := l.poll
	if previous != nil && !takeover {
		m.mu.Unlock()
		return 0, &ListenerActiveError{Label: label(previous.owner)}
	}
	// Install the successor before announcing the old holder's replacement,
	// so the old poll's exit can never clear the new listener.
	m.nextToken++
	l.poll = &holder{token: m.nextToken, owner: owner}
	l.delivered = false
	token := l.poll.token
	m.mu.Unlock()

	if previous != nil {
		m.Bus.Publish(Event{Type: PollReplaced, Key: key, Token: previous.token})
	}
	m.Bus.Publish(Event{Type: PresenceChanged, Key: key})
	return token, nil
}

// HoldsPoll reports whether token is still the session's poll listener.
func (m *Manager) HoldsPoll(key string, token uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.sessions[key]
	return l != nil && l.poll != nil && l.poll.token == token
}

// ReleasePoll ends token's claim; it does nothing if token no longer holds
// the session. delivered says whether this poll handed feedback to the
// agent, which makes the agent's presence "working".
func (m *Manager) ReleasePoll(key string, token uint64, delivered bool) {
	m.mu.Lock()
	l := m.sessions[key]
	if l == nil || l.poll == nil || l.poll.token != token {
		m.mu.Unlock()
		return
	}
	l.poll = nil
	l.delivered = delivered
	stopTimer(l)
	m.mu.Unlock()
	m.Bus.Publish(Event{Type: PresenceChanged, Key: key})
}

// AgentReplied concludes delivered work: the agent is no longer "working".
func (m *Manager) AgentReplied(key string) {
	m.mu.Lock()
	l := m.sessions[key]
	changed := l != nil && l.delivered
	if changed {
		l.delivered = false
	}
	m.mu.Unlock()
	if changed {
		m.Bus.Publish(Event{Type: PresenceChanged, Key: key})
	}
}

// Presence returns the agent's presence for a session.
func (m *Manager) Presence(key string) Presence {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.sessions[key]
	switch {
	case l == nil:
		return Presence{State: Idle}
	case l.poll != nil:
		return Presence{State: Listening, Owner: label(l.poll.owner)}
	case l.delivered:
		return Presence{State: Working}
	}
	return Presence{State: Idle}
}

// Listeners returns each session's current poll label, by session key.
func (m *Manager) Listeners() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for key, l := range m.sessions {
		if l.poll != nil {
			out[key] = label(l.poll.owner)
		}
	}
	return out
}

// ReviewerConnected registers an open review page and returns the function
// to call when it disconnects. When the last page leaves while a poll waits,
// the grace period starts; if no page comes back in time, BrowserDisconnected
// lets the poll return. A poll that starts before any page connected keeps
// waiting: only a page leaving starts the clock.
func (m *Manager) ReviewerConnected(key, file string) (disconnected func()) {
	m.mu.Lock()
	l := m.ensure(key, file)
	l.reviewers++
	stopTimer(l)
	m.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			l.reviewers--
			if l.reviewers == 0 && l.poll != nil && m.Grace > 0 && l.grace == nil {
				l.grace = time.AfterFunc(m.Grace, func() { m.graceElapsed(l) })
			}
		})
	}
}

func (m *Manager) graceElapsed(l *live) {
	m.mu.Lock()
	l.grace = nil
	fire := l.poll != nil && l.reviewers == 0
	m.mu.Unlock()
	if fire {
		m.Bus.Publish(Event{Type: BrowserDisconnected, Key: l.key})
	}
}

// Reviewers returns how many review pages are connected to a session.
func (m *Manager) Reviewers(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.sessions[key]; l != nil {
		return l.reviewers
	}
	return 0
}

func stopTimer(l *live) {
	if l.grace != nil {
		l.grace.Stop()
		l.grace = nil
	}
}

// DisconnectGraceFromEnv reads REDLINE_DISCONNECT_GRACE_MS.
func DisconnectGraceFromEnv() time.Duration {
	return DisconnectGrace(os.Getenv("REDLINE_DISCONNECT_GRACE_MS"))
}
