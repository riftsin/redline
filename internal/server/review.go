package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/riftsin/redline/internal/attachments"
	"github.com/riftsin/redline/internal/layout"
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
	regionLayout       = "layout"
)

var allRegions = []string{regionConversation, regionPresence, regionAgentBanner, regionEndedBanner, regionDockSummary, regionLayout}

func regionsFor(ev review.EventType) []string {
	switch ev {
	case review.QueueChanged:
		return []string{regionConversation, regionDockSummary}
	case review.TranscriptChanged:
		return []string{regionConversation}
	case review.PresenceChanged:
		// Undelivered notes say why they are not Seen yet, from presence.
		return []string{regionConversation, regionPresence, regionAgentBanner, regionDockSummary}
	case review.SessionChanged:
		return allRegions
	case review.LayoutChanged:
		return []string{regionLayout}
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
	Kind        string                `json:"kind"`
	Prompt      string                `json:"prompt"`
	Selector    string                `json:"selector"`
	Tag         string                `json:"tag"`
	Text        string                `json:"text"`
	Target      json.RawMessage       `json:"target"`
	QueueKey    string                `json:"queueKey"`
	Attachments []store.AttachmentRef `json:"attachments"`
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
		Attachments: body.Attachments,
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
		Message     string                `json:"message"`
		Attachments []store.AttachmentRef `json:"attachments"`
		Snapshot    string                `json:"snapshot"`
		End         bool                  `json:"end"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	res, err := s.m.Send(r.Context(), store.SendRequest{
		Key: sess.Key, Message: body.Message, MessageAttachments: body.Attachments, Snapshot: body.Snapshot, End: body.End,
	})
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
	case errors.Is(err, store.ErrNoQueueItem):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "that note was already sent or removed")
	case errors.Is(err, store.ErrTooManyAttachments):
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", fmt.Sprintf("attach at most %d images to one note", store.MaxAttachmentsPerPrompt))
	case errors.Is(err, store.ErrAttachmentGone):
		writeError(w, http.StatusConflict, "ATTACHMENT_GONE", "an attached image expired; remove it and attach it again")
	default:
		writeError(w, http.StatusInternalServerError, "SERVER_ERROR", err.Error())
	}
	return true
}

func (s *Server) editQueued(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "bad queue item id")
		return
	}
	var body struct {
		Prompt string `json:"prompt"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "a note cannot be empty; remove it instead")
		return
	}
	if s.actionError(w, s.m.EditQueued(r.Context(), sess.Key, id, body.Prompt)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "edited"})
}

// uploadAttachment stores one image sent as the raw request body. The
// declared content type is ignored: the bytes decide.
func (s *Server) uploadAttachment(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	if sess.Status == store.StatusEnded {
		writeError(w, http.StatusConflict, "ENDED", "this review session has ended")
		return
	}
	if s.att == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "image attachments are not available")
		return
	}
	if r.ContentLength > s.att.Limits().MaxBytes {
		// Refuse before reading, but drain a little so the browser sees the answer.
		_, _ = io.CopyN(io.Discard, r.Body, 1<<16)
		writeError(w, http.StatusRequestEntityTooLarge, "TOO_LARGE", tooLarge(s.att.Limits().MaxBytes))
		return
	}
	a, err := s.att.Save(r.Context(), r.Body)
	switch {
	case errors.Is(err, attachments.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "TOO_LARGE", tooLarge(s.att.Limits().MaxBytes))
		return
	case errors.Is(err, attachments.ErrUnsupported):
		writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED", attachments.ErrUnsupported.Error())
		return
	case errors.Is(err, attachments.ErrDiskFull):
		writeError(w, http.StatusInsufficientStorage, "DISK_FULL", "the image store is full; remove queued images or try later")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "SERVER_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": a.ID, "mime": a.Mime, "width": a.Width, "height": a.Height, "bytes": a.Bytes, "url": "/attachments/" + a.ID})
}

func tooLarge(max int64) string {
	return fmt.Sprintf("images can be at most %d MB", max>>20)
}

func (s *Server) attachment(w http.ResponseWriter, r *http.Request) {
	if s.att == nil {
		http.NotFound(w, r)
		return
	}
	path, a, err := s.att.Path(r.Context(), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	h := w.Header()
	h.Set("Content-Type", a.Mime)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, max-age=604800, immutable")
	h.Set("Content-Security-Policy", "default-src 'none'")
	http.ServeContent(w, r, "", a.CreatedAt, f)
}

// sources gives the review page the artifact before and after its latest
// change, so it can measure what changed.
func (s *Server) sources(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	src := s.m.Sources(sess.Key)
	writeJSON(w, http.StatusOK, map[string]any{"revision": src.Revision, "previous": src.Previous, "current": src.Current})
}

func (s *Server) recordChange(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var body struct {
		Revision  int      `json:"revision"`
		Selectors []string `json:"selectors"`
		Rewrite   bool     `json:"rewrite"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Revision < 1 || body.Revision > sess.ArtifactRevision {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "unknown revision")
		return
	}
	if s.actionError(w, s.m.RecordChange(r.Context(), sess.Key, store.Change{Revision: body.Revision, Selectors: body.Selectors, Rewrite: body.Rewrite})) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

// layoutPass records one audit pass from the review page. It never wakes
// the agent.
func (s *Server) layoutPass(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var body struct {
		Revision               int              `json:"revision"`
		Complete               bool             `json:"complete"`
		TargetPresenceComplete bool             `json:"target_presence_complete"`
		ViewportWidth          int              `json:"viewport_width"`
		Findings               []layout.Finding `json:"findings"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Revision < 0 || body.Revision > sess.ArtifactRevision || body.ViewportWidth <= 0 {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "bad audit pass")
		return
	}
	err := s.m.ApplyLayoutPass(r.Context(), sess.Key, layout.Pass{
		Complete: body.Complete, TargetPresenceComplete: body.TargetPresenceComplete, ViewportWidth: body.ViewportWidth,
		Revision: body.Revision, At: time.Now(), Findings: body.Findings,
	})
	if s.actionError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

func (s *Server) layoutQueue(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	n, err := s.m.QueueLayoutFixes(r.Context(), sess.Key, body.IDs)
	if s.actionError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "queued", "queued": n})
}

func (s *Server) layoutDismiss(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	if s.actionError(w, s.m.DismissLayoutWarning(r.Context(), sess.Key, r.PathValue("id"))) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "dismissed"})
}
