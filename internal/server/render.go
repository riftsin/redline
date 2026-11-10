package server

import (
	"bytes"
	"context"
	"html/template"
	"net/url"
	"path/filepath"

	"github.com/riftsin/redline/internal/review"
	"github.com/riftsin/redline/internal/store"
	"github.com/riftsin/redline/web"
)

// Sandbox is the artifact iframe's sandbox: scripts and forms run, links and
// popups open in real tabs, but the artifact never gets same-origin access
// to the review page. Every /artifact response repeats it as a CSP header,
// so an artifact page that escapes into a popup stays opaque-origin.
const Sandbox = "allow-scripts allow-forms allow-popups allow-popups-to-escape-sandbox allow-downloads"

// transcriptLimit bounds how much conversation a page renders.
const transcriptLimit = 500

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	"presenceLabel": presenceLabel,
}).ParseFS(web.Templates, "templates/*.html"))

func presenceLabel(p review.Presence, ended bool) string {
	switch {
	case ended:
		return "Session ended"
	case p.State == review.Listening && p.Owner != "" && p.Owner != "agent-listener":
		return "Listening · " + p.Owner
	case p.State == review.Listening:
		return "Agent listening"
	case p.State == review.Working:
		return "Agent working…"
	}
	return "Agent not listening"
}

type queueView struct {
	ID     int64
	Kind   string
	Prompt string
	Anchor *store.Anchor
}

type reviewView struct {
	Key          string
	File         string
	Name         string
	ArtifactPath string
	FrameSrc     string
	Sandbox      string
	Revision     int
	Ended        bool
	EndedBy      string
	Presence     review.Presence
	Transcript   []store.ChatEntry
	Queue        []queueView
}

func (s *Server) reviewView(ctx context.Context, sess *store.Session) (*reviewView, error) {
	transcript, err := s.m.Store.Transcript(ctx, sess.Key, transcriptLimit)
	if err != nil {
		return nil, err
	}
	queue, err := s.m.Store.Queue(ctx, sess.Key)
	if err != nil {
		return nil, err
	}
	v := &reviewView{
		Key:        sess.Key,
		File:       sess.File,
		Name:       filepath.Base(sess.File),
		Sandbox:    Sandbox,
		Revision:   sess.ArtifactRevision,
		Ended:      sess.Status == store.StatusEnded,
		EndedBy:    sess.EndedBy,
		Presence:   s.m.Presence(sess.Key),
		Transcript: transcript,
	}
	v.ArtifactPath = "/artifact/" + sess.Key + "/" + url.PathEscape(v.Name)
	v.FrameSrc = v.ArtifactPath + "?rev=" + itoa(sess.ArtifactRevision)
	for _, it := range queue {
		v.Queue = append(v.Queue, queueView{ID: it.ID, Kind: it.Kind, Prompt: it.Prompt, Anchor: store.AnchorFor(it)})
	}
	return v, nil
}

func render(name string, data any) (string, error) {
	var b bytes.Buffer
	if err := templates.ExecuteTemplate(&b, name, data); err != nil {
		return "", err
	}
	return b.String(), nil
}
