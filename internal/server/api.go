package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/riftsin/redline/internal/paths"
	"github.com/riftsin/redline/internal/review"
	"github.com/riftsin/redline/internal/store"
)

// HealthResponse is what GET /health returns.
type HealthResponse struct {
	OK             bool       `json:"ok"`
	App            string     `json:"app"`
	Version        string     `json:"version"`
	BuildID        string     `json:"build_id"`
	StateID        string     `json:"state_id"`
	StateDir       string     `json:"state_dir"`
	Hosts          []string   `json:"hosts"`
	NetworkWarning string     `json:"network_warning,omitempty"`
	Listeners      []Listener `json:"listeners"`
}

// Listener is a session's current poll.
type Listener struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// AppName is what /health reports, so the CLI only ever adopts, replaces
// or stops a redline server.
const AppName = "redline"

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	resp := HealthResponse{
		OK:             true,
		App:            AppName,
		Version:        s.info.Version,
		BuildID:        s.info.BuildID,
		StateID:        s.info.StateID,
		StateDir:       s.info.StateDir,
		Hosts:          s.Hosts(),
		NetworkWarning: s.plan.Warning,
		Listeners:      []Listener{},
	}
	for key, label := range s.m.Listeners() {
		resp.Listeners = append(resp.Listeners, Listener{Key: key, Label: label})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) shutdown(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&body)
	reason := body.Reason
	if reason != "upgrade" && reason != "restart" {
		reason = "stop"
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "shutting-down"})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	s.requestStop(reason)
}

// SessionSummary describes an open session to the CLI.
type SessionSummary struct {
	Key            string `json:"key"`
	File           string `json:"file"`
	Status         string `json:"status"`
	URL            string `json:"url"`
	PendingPrompts int    `json:"pending_prompts"`
	Listener       string `json:"listener,omitempty"`
}

func (s *Server) sessionURL(key string) string {
	return s.plan.BaseURL(s.port) + "/session/" + key
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.m.Store.ActiveSessions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "SERVER_ERROR", err.Error())
		return
	}
	listeners := s.m.Listeners()
	out := []SessionSummary{}
	for _, sess := range sessions {
		pending, _ := s.m.Store.PendingCount(r.Context(), sess.Key)
		out = append(out, SessionSummary{
			Key: sess.Key, File: sess.File, Status: sess.Status, URL: s.sessionURL(sess.Key),
			PendingPrompts: pending, Listener: listeners[sess.Key],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

// OpenResponse is what POST /api/sessions returns.
type OpenResponse struct {
	Key            string `json:"key"`
	File           string `json:"file"`
	URL            string `json:"url"`
	Status         string `json:"status"` // "opened" or "user-ended"
	NetworkWarning string `json:"network_warning,omitempty"`
}

func (s *Server) openSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		File   string `json:"file"`
		Reopen bool   `json:"reopen"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	file, err := paths.CanonicalHTML(body.File)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	key := paths.SessionKey(file)
	_, res, err := s.m.Open(r.Context(), key, file, body.Reopen)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "SERVER_ERROR", err.Error())
		return
	}
	status := "opened"
	if res == store.UserEnded {
		status = "user-ended"
	}
	writeJSON(w, http.StatusOK, OpenResponse{Key: key, File: file, URL: s.sessionURL(key), Status: status, NetworkWarning: s.plan.Warning})
}

// sessionFor resolves a CLI-supplied file to its session key, writing the
// error response when it cannot.
func (s *Server) sessionFor(w http.ResponseWriter, ctx context.Context, file string) (string, string, bool) {
	canonical, err := paths.CanonicalHTML(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return "", "", false
	}
	key := paths.SessionKey(canonical)
	if _, err := s.m.Store.Session(ctx, key); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no review session for "+canonical)
		return "", "", false
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "SERVER_ERROR", err.Error())
		return "", "", false
	}
	return key, canonical, true
}

func (s *Server) reply(w http.ResponseWriter, r *http.Request) {
	var body struct {
		File string `json:"file"`
		Text string `json:"text"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	key, _, ok := s.sessionFor(w, r.Context(), body.File)
	if !ok {
		return
	}
	switch err := s.m.Reply(r.Context(), key, body.Text); {
	case errors.Is(err, store.ErrEmpty):
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "the reply text is empty")
	case errors.Is(err, store.ErrEnded):
		sess, _ := s.m.Store.Session(r.Context(), key)
		writeJSON(w, http.StatusConflict, map[string]string{"status": "ended", "ended_by": sess.EndedBy})
	case err != nil:
		writeError(w, http.StatusInternalServerError, "SERVER_ERROR", err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "accepted"})
	}
}

func (s *Server) agentEnd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		File string `json:"file"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	key, _, ok := s.sessionFor(w, r.Context(), body.File)
	if !ok {
		return
	}
	sess, err := s.m.End(r.Context(), key, store.EndedByAgent)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "SERVER_ERROR", err.Error())
		return
	}
	s.m.AgentReplied(key)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ended", "ended_by": sess.EndedBy})
}

// PollResponse is the final JSON of /api/poll.
type PollResponse struct {
	Status           string          `json:"status"`
	Prompts          []store.Prompt  `json:"prompts,omitempty"`
	ArtifactFailures []store.Failure `json:"artifact_failures,omitempty"`
	DOMSnapshot      string          `json:"dom_snapshot,omitempty"`
	SessionEnded     bool            `json:"session_ended,omitempty"`
	EndedBy          string          `json:"ended_by,omitempty"`
	Code             string          `json:"code,omitempty"`
	Error            string          `json:"error,omitempty"`
	Listener         string          `json:"listener,omitempty"`
}

// PollStateHeader tells the CLI the poll is waiting, which is when an
// optional readiness chime may play.
const PollStateHeader = "Redline-Poll-State"

const heartbeatEvery = 5 * time.Second

// poll is `redline poll`: a long poll that returns when the reviewer sends
// feedback, the session ends, or every review page stays away past the
// disconnect grace. The response streams a space now and then as a
// heartbeat (a failed write reveals a client that went away) and ends with
// the JSON result.
func (s *Server) poll(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	owner := strings.TrimSpace(q.Get("owner"))
	takeover := q.Get("takeover") == "1"
	var reply string
	if r.Method == http.MethodPost {
		var body struct {
			AgentReply *string `json:"agent_reply"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		if body.AgentReply != nil {
			if strings.TrimSpace(*body.AgentReply) == "" {
				writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "the agent reply text is empty")
				return
			}
			reply = *body.AgentReply
		} else if !takeover {
			writeError(w, http.StatusMethodNotAllowed, "VALIDATION_ERROR", "POST /api/poll requires agent_reply or takeover=1")
			return
		}
	} else if takeover {
		writeError(w, http.StatusMethodNotAllowed, "VALIDATION_ERROR", "a poll takeover requires POST")
		return
	}
	if strings.EqualFold(owner, "none") || strings.HasPrefix(owner, "-") {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "--owner none is reserved; pass a different listener label")
		return
	}
	var timeout time.Duration
	if v := q.Get("timeout_ms"); v != "" {
		ms, err := strconv.Atoi(v)
		if err != nil || ms < 0 {
			writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "timeout_ms must be a non-negative integer")
			return
		}
		timeout = time.Duration(ms) * time.Millisecond
	}

	key, file, ok := s.sessionFor(w, r.Context(), q.Get("file"))
	if !ok {
		return
	}

	// Once the poll holds the session the response is streamed; until
	// then errors are ordinary JSON responses.
	var mu sync.Mutex
	started := false
	flusher, _ := w.(http.Flusher)
	start := func(waiting bool) {
		if started {
			return
		}
		started = true
		w.Header().Set("Content-Type", "application/json")
		if waiting {
			w.Header().Set(PollStateHeader, "listening")
		}
		w.WriteHeader(http.StatusOK)
	}
	writeChunk := func(b []byte) error {
		mu.Lock()
		defer mu.Unlock()
		start(false)
		if _, err := w.Write(b); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	req := review.PollRequest{
		Key: key, File: file, Owner: owner, Takeover: takeover, AgentReply: reply, Timeout: timeout,
		HeartbeatEvery: heartbeatEvery,
		Heartbeat:      func() error { return writeChunk([]byte(" ")) },
		Waiting: func() {
			mu.Lock()
			start(true)
			mu.Unlock()
			_ = writeChunk([]byte(" "))
		},
	}
	res, err := s.m.Poll(r.Context(), req, func(fb store.Feedback) error {
		b, err := json.Marshal(PollResponse{
			Status: "feedback", Prompts: fb.Prompts, ArtifactFailures: fb.ArtifactFailures,
			DOMSnapshot: fb.DOMSnapshot, SessionEnded: fb.SessionEnded, EndedBy: fb.EndedBy,
		})
		if err != nil {
			return err
		}
		return writeChunk(b)
	})

	var active *review.ListenerActiveError
	switch {
	case errors.As(err, &active):
		writeJSON(w, http.StatusConflict, PollResponse{
			Status: "error", Code: "LISTENER_ACTIVE", Listener: active.Label,
			Error: "another poll (" + active.Label + ") is already listening for " + filepath.Base(file),
		})
	case errors.Is(err, review.ErrClientGone):
		// Nobody to answer.
	case err != nil:
		if started {
			_ = writeChunk(mustJSON(PollResponse{Status: "error", Code: "SERVER_ERROR", Error: err.Error()}))
		} else {
			writeError(w, http.StatusInternalServerError, "SERVER_ERROR", err.Error())
		}
	case res.Outcome == review.OutcomeFeedback:
		// Already written by deliver.
	case res.Outcome == review.OutcomeReplaced:
		_ = writeChunk(mustJSON(PollResponse{Status: "error", Code: "LISTENER_REPLACED", Error: "another poll took over this session (--takeover)"}))
	default:
		_ = writeChunk(mustJSON(PollResponse{Status: res.Outcome, EndedBy: res.EndedBy}))
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
