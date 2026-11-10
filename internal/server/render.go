package server

import (
	"bytes"
	"context"
	"encoding/json"
	"html/template"
	"net/url"
	"path/filepath"
	"time"

	"github.com/riftsin/redline/internal/markdown"
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
	ID          int64
	Kind        string
	Prompt      string
	Anchor      *store.Anchor
	Attachments []imageView
}

// imageView is an attached image's thumbnail.
type imageView struct {
	ID, Name, URL string
	Gone          bool
}

// receiptView is the Seen / Working / Done row under a sent note.
type receiptView struct {
	Steps []receiptStep
	// Pending explains why the note is not Seen yet.
	Pending string
}

type receiptStep struct {
	Emoji, Label, When string
	Done               bool
}

// entryView is one conversation bubble.
type entryView struct {
	ID          int64
	Role        string
	Text        string
	HTML        template.HTML // agent replies: rendered Markdown
	Anchor      *store.Anchor
	Attachments []imageView
	Receipt     *receiptView
	// Changes are the blocks the agent changed for this reply.
	Changes      []string
	ChangesJSON  string
	Rewrite      bool
	ChangesCount int
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
	Transcript   []entryView
	Queue        []queueView
	MaxImages    int
}

func (s *Server) images(ctx context.Context, refs []store.AttachmentRef) []imageView {
	var out []imageView
	for _, r := range refs {
		v := imageView{ID: r.ID, Name: r.Name, URL: "/attachments/" + r.ID}
		if s.att == nil {
			v.Gone = true
		} else if _, _, err := s.att.Path(ctx, r.ID); err != nil {
			v.Gone = true
		}
		out = append(out, v)
	}
	return out
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("15:04")
}

func receiptFor(e store.ChatEntry, presence review.Presence) *receiptView {
	if e.Role != "user" || e.Receipt == store.ReceiptNone {
		return nil
	}
	r := &receiptView{Steps: []receiptStep{
		{Emoji: "👀", Label: "Seen", When: stamp(e.DeliveredAt), Done: !e.DeliveredAt.IsZero()},
		{Emoji: "🛠️", Label: "Working", When: stamp(e.WorkingAt), Done: !e.WorkingAt.IsZero()},
		{Emoji: "✅", Label: "Done", When: stamp(e.DoneAt), Done: !e.DoneAt.IsZero()},
	}}
	if e.DeliveredAt.IsZero() && e.Receipt == store.ReceiptLive {
		switch presence.State {
		case review.Listening:
			r.Pending = "Delivering…"
		case review.Working:
			r.Pending = "Agent is busy; delivered on its next poll"
		default:
			r.Pending = "No agent is listening"
		}
	}
	return r
}

// changesFor groups recorded changes under the agent reply that followed
// them: each reply links to what the agent changed since its previous one.
func changesFor(transcript []store.ChatEntry, changes []store.Change) map[int64][]store.Change {
	out := map[int64][]store.Change{}
	ci := 0
	for _, e := range transcript {
		if e.Role != "agent" {
			continue
		}
		for ci < len(changes) && !changes[ci].CreatedAt.After(e.CreatedAt) {
			out[e.ID] = append(out[e.ID], changes[ci])
			ci++
		}
	}
	return out
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
	changes, err := s.m.Store.Changes(ctx, sess.Key)
	if err != nil {
		return nil, err
	}
	v := &reviewView{
		Key:       sess.Key,
		File:      sess.File,
		Name:      filepath.Base(sess.File),
		Sandbox:   Sandbox,
		Revision:  sess.ArtifactRevision,
		Ended:     sess.Status == store.StatusEnded,
		EndedBy:   sess.EndedBy,
		Presence:  s.m.Presence(sess.Key),
		MaxImages: store.MaxAttachmentsPerPrompt,
	}
	v.ArtifactPath = "/artifact/" + sess.Key + "/" + url.PathEscape(v.Name)
	v.FrameSrc = v.ArtifactPath + "?rev=" + itoa(sess.ArtifactRevision)

	byReply := changesFor(transcript, changes)
	for _, e := range transcript {
		ev := entryView{ID: e.ID, Role: e.Role, Text: e.Text, Anchor: e.Anchor, Attachments: s.images(ctx, e.Attachments), Receipt: receiptFor(e, v.Presence)}
		if e.Role == "agent" {
			ev.HTML = markdown.Render(e.Text)
			seen := map[string]bool{}
			for _, c := range byReply[e.ID] {
				ev.Rewrite = ev.Rewrite || c.Rewrite
				for _, sel := range c.Selectors {
					if !seen[sel] {
						seen[sel] = true
						ev.Changes = append(ev.Changes, sel)
					}
				}
			}
			ev.ChangesCount = len(ev.Changes)
			b, _ := json.Marshal(ev.Changes)
			ev.ChangesJSON = string(b)
		}
		v.Transcript = append(v.Transcript, ev)
	}
	for _, it := range queue {
		v.Queue = append(v.Queue, queueView{ID: it.ID, Kind: it.Kind, Prompt: it.Prompt, Anchor: store.AnchorFor(it), Attachments: s.images(ctx, it.Attachments)})
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
