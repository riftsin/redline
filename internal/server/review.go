package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/riftsin/redline/internal/review"
	"github.com/riftsin/redline/internal/store"
)

type homeSession struct {
	Key, File, Name string
	Presence        review.Presence
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.m.Store.ActiveSessions(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var view struct{ Sessions []homeSession }
	for _, sess := range sessions {
		view.Sessions = append(view.Sessions, homeSession{Key: sess.Key, File: sess.File, Name: filepath.Base(sess.File), Presence: s.m.Presence(sess.Key)})
	}
	s.page(w, "home", view)
}

func (s *Server) page(w http.ResponseWriter, name string, data any) {
	html, err := render(name, data)
	if err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	// The review page frames the artifact but must never be framed itself.
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(html))
}

// session loads the session named by the {key} path value, writing a 404
// when there is none.
func (s *Server) session(w http.ResponseWriter, r *http.Request) (*store.Session, bool) {
	sess, err := s.m.Store.Session(r.Context(), r.PathValue("key"))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return nil, false
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	return sess, true
}

func (s *Server) reviewPage(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	s.m.Track(sess.Key, sess.File)
	view, err := s.reviewView(r.Context(), sess)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.page(w, "review", view)
}

// Regions of the review page that the server re-renders on change.
const (
	regionConversation = "conversation"
	regionPresence     = "presence"
	regionAgentBanner  = "agent-banner"
	regionEndedBanner  = "ended-banner"
	regionDockSummary  = "dock-summary"
)

var allRegions = []string{regionConversation, regionPresence, regionAgentBanner, regionEndedBanner, regionDockSummary}

func regionsFor(ev review.EventType) []string {
	switch ev {
	case review.QueueChanged:
		return []string{regionConversation, regionDockSummary}
	case review.TranscriptChanged:
		return []string{regionConversation}
	case review.PresenceChanged:
		return []string{regionPresence, regionAgentBanner, regionDockSummary}
	case review.SessionChanged:
		return allRegions
	}
	return nil
}

// sseKeepAlive keeps proxies from closing an idle event stream and reveals a
// browser that went away without closing its connection.
const sseKeepAlive = 15 * time.Second

// events is the review page's live connection: on connect and after every
// change it pushes the page state and freshly rendered regions. While at
// least one stream is open the session counts a reviewer as present.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	key := sess.Key
	sub := s.m.Bus.Subscribe(key)
	defer sub.Close()
	leave := s.m.ReviewerConnected(key, sess.File)
	defer leave()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(event string, data any) error {
		b, err := json.Marshal(data)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	push := func(regions []string, reload bool) error {
		sess, err := s.m.Store.Session(r.Context(), key)
		if err != nil {
			return err
		}
		view, err := s.reviewView(r.Context(), sess)
		if err != nil {
			return err
		}
		if err := send("state", map[string]any{
			"ended":    view.Ended,
			"endedBy":  view.EndedBy,
			"presence": view.Presence.State,
			"revision": view.Revision,
			"queued":   len(view.Queue),
		}); err != nil {
			return err
		}
		for _, region := range regions {
			html, err := render(region, view)
			if err != nil {
				return err
			}
			if err := send("region", map[string]string{"id": region, "html": html}); err != nil {
				return err
			}
		}
		if reload {
			return send("reload", map[string]any{"revision": view.Revision, "src": view.ArtifactPath + "?rev=" + strconv.Itoa(view.Revision)})
		}
		return nil
	}

	// A reconnecting page may have missed changes: resync everything.
	if err := push(allRegions, false); err != nil {
		return
	}

	keepAlive := time.NewTicker(sseKeepAlive)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev := <-sub.C:
			// Coalesce a burst of events into one update.
			wanted := map[string]bool{}
			reload := false
			for {
				for _, region := range regionsFor(ev.Type) {
					wanted[region] = true
				}
				reload = reload || ev.Type == review.Reload
				select {
				case ev = <-sub.C:
					continue
				default:
				}
				break
			}
			var regions []string
			for _, region := range allRegions {
				if wanted[region] {
					regions = append(regions, region)
				}
			}
			if len(regions) == 0 && !reload {
				continue
			}
			if err := push(regions, reload); err != nil {
				return
			}
		}
	}
}

// -- Review page actions --------------------------------------------------------

// noteRequest is a note from the annotation card, or an answer the artifact
// queued through window.redline.queuePrompt.
type noteRequest struct {
	Kind     string          `json:"kind"`
	Prompt   string          `json:"prompt"`
	Selector string          `json:"selector"`
	Tag      string          `json:"tag"`
	Text     string          `json:"text"`
	Target   json.RawMessage `json:"target"`
	QueueKey string          `json:"queueKey"`
}

func (s *Server) enqueue(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var body noteRequest
	if !readJSON(w, r, &body) {
		return
	}
	if body.Kind != "answer" {
		body.Kind = "annotation"
	}
	id, err := s.m.Enqueue(r.Context(), store.QueueItem{
		SessionKey: sess.Key, Kind: body.Kind, Prompt: body.Prompt, Selector: body.Selector,
		Tag: body.Tag, Excerpt: body.Text, Target: body.Target, QueueKey: body.QueueKey,
	})
	if s.actionError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "queued", "id": id})
}

func (s *Server) removeQueued(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "bad queue item id")
		return
	}
	if s.actionError(w, s.m.RemoveQueued(r.Context(), sess.Key, id)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var body struct {
		Message  string `json:"message"`
		Snapshot string `json:"snapshot"`
		End      bool   `json:"end"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	res, err := s.m.Send(r.Context(), sess.Key, body.Message, body.Snapshot, body.End)
	if s.actionError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "sent", "sent": res.Sent, "ended": res.Ended})
}

func (s *Server) userEnd(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	if _, err := s.m.End(r.Context(), sess.Key, store.EndedByUser); s.actionError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ended"})
}

func (s *Server) artifactFailures(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var body struct {
		Failures []store.Failure `json:"failures"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if len(body.Failures) > 20 {
		body.Failures = body.Failures[:20]
	}
	if s.actionError(w, s.m.RecordArtifactFailures(r.Context(), sess.Key, body.Failures)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

// actionError writes the response for a failed review page action.
func (s *Server) actionError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrEnded):
		writeError(w, http.StatusConflict, "ENDED", "this review session has ended")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such review session")
	default:
		writeError(w, http.StatusInternalServerError, "SERVER_ERROR", err.Error())
	}
	return true
}
