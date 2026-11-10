// Package layout is the passive layout-issues inbox: the lifecycle of the
// severe layout failures the review page's audit proves, as pure functions
// over stored records.
//
// Detection is passive: an audit pass never wakes the agent and never
// repairs anything. Findings land here as durable records the reviewer
// triages, and only an explicit "Queue selected fixes" turns them into an
// ordinary queued note. The lifecycle is deliberately conservative: a
// warning clears only on positive evidence - a newer artifact revision plus
// a complete pass for the same viewport class that no longer detects it.
// Absence of evidence (a failed pass, another viewport, the same revision)
// never clears anything.
//
// Adapted from lavish-axi (MIT, Copyright (c) 2026 Kun Chen).
package layout

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
	"time"
)

// Statuses.
const (
	Open       = "open"
	Queued     = "queued"
	Recurring  = "recurring"
	Unverified = "unverified"
	Reopened   = "reopened"
	Resolved   = "resolved"
	Dismissed  = "dismissed"
	Obsolete   = "obsolete"
)

var activeStatuses = map[string]bool{Open: true, Queued: true, Recurring: true, Unverified: true, Reopened: true}

// ViewportClasses are the audited viewport classes, narrowest first.
var ViewportClasses = []string{"mobile", "compact", "desktop"}

const (
	maxHistory         = 20
	maxSerialHistory   = 10
	maxStored          = 200
	maxQueuedPerPrompt = 50
)

// Event is one entry of a warning's history.
type Event struct {
	At       time.Time `json:"at"`
	Revision int       `json:"revision"`
	Event    string    `json:"event"`
	Note     string    `json:"note,omitempty"`
}

// Warning is one stored record.
type Warning struct {
	ID                string    `json:"id"` // the fingerprint
	Rule              string    `json:"rule"`
	Status            string    `json:"status"`
	Selector          string    `json:"selector"`
	Component         string    `json:"component"`
	Axis              string    `json:"axis"`
	OverflowPx        float64   `json:"overflow_px"`
	ViewportClass     string    `json:"viewport_class"`
	ViewportWidth     int       `json:"viewport_width"`
	FirstSeenAt       time.Time `json:"first_seen_at"`
	FirstSeenRevision int       `json:"first_seen_revision"`
	LastSeenAt        time.Time `json:"last_seen_at"`
	LastSeenRevision  int       `json:"last_seen_revision"`
	Observations      int       `json:"observations"`
	// QueuedAt (not QueuedRevision, which may legitimately be 0) marks a
	// repair request.
	QueuedAt          time.Time `json:"queued_at,omitzero"`
	QueuedRevision    int       `json:"queued_revision"`
	QueueAttempts     int       `json:"queue_attempts"`
	DismissedRevision int       `json:"dismissed_revision"`
	ObsoleteReason    string    `json:"obsolete_reason,omitempty"`
	History           []Event   `json:"history"`
}

// Finding is one severe failure reported by an audit pass.
type Finding struct {
	Selector      string  `json:"selector"`
	Kind          string  `json:"kind"`
	Axis          string  `json:"axis"`
	OverflowPx    float64 `json:"overflowPx"`
	ViewportWidth int     `json:"viewportWidth"`
	Severity      string  `json:"severity"`
}

// Pass is one browser audit pass.
type Pass struct {
	Complete               bool
	TargetPresenceComplete bool
	ViewportWidth          int
	Revision               int
	At                     time.Time
	Findings               []Finding
}

// ViewportClass classifies a viewport width.
func ViewportClass(width int) string {
	switch {
	case width <= 640:
		return "mobile"
	case width <= 1024:
		return "compact"
	}
	return "desktop"
}

// ViewportLabel names a viewport class for people.
func ViewportLabel(class string) string {
	switch class {
	case "mobile":
		return "Mobile"
	case "compact":
		return "Tablet / compact"
	}
	return "Desktop"
}

func normalize(s string) string { return strings.Join(strings.Fields(s), " ") }

// Fingerprint is a warning's stable identity: rule, target and viewport
// class. Magnitude is deliberately excluded, so a finding that gets worse
// updates its one record instead of adding a near-duplicate.
func Fingerprint(rule, target, viewportClass string) string {
	sum := sha256.Sum256([]byte(normalize(rule) + "|" + normalize(target) + "|" + normalize(viewportClass)))
	return hex.EncodeToString(sum[:])[:16]
}

var (
	idPart    = regexp.MustCompile(`#([A-Za-z0-9_-]+)`)
	classPart = regexp.MustCompile(`\.([A-Za-z0-9_-]+)`)
	tagPart   = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9-]*)`)
)

// Component is the human-readable identity of a CSS path: its most specific
// id, else its first class, else its trailing tag.
func Component(selector string) string {
	parts := strings.Split(normalize(selector), ">")
	last := strings.TrimSpace(parts[len(parts)-1])
	if m := idPart.FindStringSubmatch(last); m != nil {
		return "#" + m[1]
	}
	if m := classPart.FindStringSubmatch(last); m != nil {
		return "." + m[1]
	}
	if m := tagPart.FindStringSubmatch(last); m != nil {
		return m[1]
	}
	return ""
}

// Active reports whether a warning still counts as unresolved.
func (w Warning) Active() bool { return activeStatuses[w.Status] }

// Outstanding reports whether a repair request is still waiting for a
// newer revision to be checked.
func (w Warning) Outstanding() bool {
	return w.Status == Queued || (w.Status == Unverified && !w.QueuedAt.IsZero())
}

// Selectable reports whether the reviewer can queue or dismiss it.
func (w Warning) Selectable() bool { return w.Active() && !w.Outstanding() }

// ActiveCount counts unresolved warnings.
func ActiveCount(ws []Warning) int {
	n := 0
	for _, w := range ws {
		if w.Active() {
			n++
		}
	}
	return n
}

func (w Warning) withHistory(at time.Time, revision int, event, note string) Warning {
	if len(note) > 200 {
		note = note[:200]
	}
	h := append(append([]Event(nil), w.History...), Event{At: at, Revision: revision, Event: event, Note: note})
	if len(h) > maxHistory {
		h = h[len(h)-maxHistory:]
	}
	w.History = h
	return w
}

func normalizeFindings(fs []Finding, viewportWidth int) []Finding {
	var out []Finding
	for _, f := range fs {
		if strings.ToLower(f.Severity) != "error" {
			continue
		}
		rule := normalize(f.Kind)
		if rule == "" {
			rule = "layout-failure"
		}
		if len(rule) > 64 {
			rule = rule[:64]
		}
		sel := normalize(f.Selector)
		if len(sel) > 300 {
			sel = sel[:300]
		}
		axis := "horizontal"
		if f.Axis == "vertical" {
			axis = "vertical"
		}
		width := f.ViewportWidth
		if width <= 0 {
			width = viewportWidth
		}
		px := f.OverflowPx
		if math.IsNaN(px) || math.IsInf(px, 0) || px < 0 {
			px = 0
		}
		out = append(out, Finding{Selector: sel, Kind: rule, Axis: axis, OverflowPx: math.Round(px), ViewportWidth: width, Severity: "error"})
		if len(out) == maxStored {
			break
		}
	}
	return out
}

// Apply folds one pass into the stored records and reports whether
// anything changed.
func Apply(ws []Warning, p Pass) ([]Warning, bool) {
	class := ViewportClass(p.ViewportWidth)
	observations := map[string]Finding{}
	var order []string
	for _, f := range normalizeFindings(p.Findings, p.ViewportWidth) {
		id := Fingerprint(f.Kind, f.Selector, class)
		if _, ok := observations[id]; !ok {
			observations[id] = f
			order = append(order, id)
		}
	}

	changed := false
	next := make([]Warning, 0, len(ws)+len(order))
	for _, w := range ws {
		// A pass for one viewport class says nothing about the others.
		if w.ViewportClass != class {
			next = append(next, w)
			continue
		}
		var updated Warning
		if f, ok := observations[w.ID]; ok {
			delete(observations, w.ID)
			updated = detected(w, f, p)
		} else if !p.Complete || !p.TargetPresenceComplete {
			// Absence is only evidence when the pass completed.
			updated = unverified(w, p)
		} else if p.Revision <= w.LastSeenRevision || (!w.Active() && w.Status != Dismissed) {
			// Absence within the same artifact revision is not a repair.
			updated = w
		} else {
			updated = resolved(w, p)
		}
		if !sameWarning(updated, w) {
			changed = true
		}
		next = append(next, updated)
	}
	for _, id := range order {
		f, ok := observations[id]
		if !ok {
			continue
		}
		changed = true
		next = append(next, Warning{
			ID: id, Rule: f.Kind, Status: Open, Selector: f.Selector, Component: Component(f.Selector),
			Axis: f.Axis, OverflowPx: f.OverflowPx, ViewportClass: class, ViewportWidth: f.ViewportWidth,
			FirstSeenAt: p.At, FirstSeenRevision: p.Revision, LastSeenAt: p.At, LastSeenRevision: p.Revision,
			Observations: 1,
		}.withHistory(p.At, p.Revision, "detected", ""))
	}
	return prune(next), changed
}

func detected(w Warning, f Finding, p Pass) Warning {
	status := detectedStatus(w, p.Revision)
	if status == w.Status && p.Revision <= w.LastSeenRevision && w.Selector == f.Selector && w.Axis == f.Axis &&
		w.OverflowPx == f.OverflowPx && w.ViewportWidth == f.ViewportWidth {
		// The identical finding on a revision already recorded changes nothing.
		return w
	}
	u := w
	u.Rule, u.Selector, u.Component, u.Axis = f.Kind, f.Selector, Component(f.Selector), f.Axis
	u.OverflowPx, u.ViewportWidth, u.LastSeenAt = f.OverflowPx, f.ViewportWidth, p.At
	u.LastSeenRevision = max(w.LastSeenRevision, p.Revision)
	u.Observations++
	u.Status = status
	if status == w.Status {
		return u
	}
	note := ""
	switch status {
	case Recurring:
		note = "still present after a newer artifact revision"
	case Reopened:
		note = "detected again after being resolved"
	}
	return u.withHistory(p.At, p.Revision, status, note)
}

func detectedStatus(w Warning, revision int) string {
	switch {
	case w.Status == Dismissed && revision <= w.DismissedRevision:
		return Dismissed
	case !w.QueuedAt.IsZero() && revision > w.QueuedRevision:
		return Recurring
	case !w.QueuedAt.IsZero():
		return Queued
	case w.Status == Resolved || w.Status == Reopened:
		return Reopened
	}
	return Open
}

func unverified(w Warning, p Pass) Warning {
	if !w.Active() || w.Status == Unverified {
		return w
	}
	w.Status = Unverified
	return w.withHistory(p.At, p.Revision, Unverified, "a check failed or was incomplete, so this warning was kept rather than cleared")
}

func resolved(w Warning, p Pass) Warning {
	w.Status = Resolved
	w.QueuedAt, w.QueuedRevision = time.Time{}, 0
	return w.withHistory(p.At, p.Revision, Resolved, "absent from a complete check on a newer artifact revision")
}

// Queue marks the selected warnings as queued for repair; they stay
// unresolved and counted. It returns the warnings it queued.
func Queue(ws []Warning, ids []string, revision int, at time.Time) ([]Warning, []Warning) {
	if len(ids) > maxQueuedPerPrompt {
		ids = ids[:maxQueuedPerPrompt]
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var queued []Warning
	next := make([]Warning, len(ws))
	for i, w := range ws {
		if want[w.ID] && w.Selectable() {
			w.Status, w.QueuedAt, w.QueuedRevision = Queued, at, revision
			w.QueueAttempts++
			w = w.withHistory(at, revision, Queued, "")
			queued = append(queued, w)
		}
		next[i] = w
	}
	return next, queued
}

// Dismiss hides a warning for the current artifact revision only; a later
// revision brings it back if it is still detected.
func Dismiss(ws []Warning, id string, revision int, at time.Time) ([]Warning, bool) {
	changed := false
	next := make([]Warning, len(ws))
	for i, w := range ws {
		if w.ID == id && w.Selectable() {
			w.Status, w.DismissedRevision = Dismissed, revision
			w = w.withHistory(at, revision, Dismissed, "dismissed for this artifact revision")
			changed = true
		}
		next[i] = w
	}
	return next, changed
}

// MarkObsolete closes active warnings of viewport classes no longer
// audited, with an explicit reason rather than reading as fixed.
func MarkObsolete(ws []Warning, classes []string, revision int, at time.Time) ([]Warning, bool) {
	keep := map[string]bool{}
	for _, c := range classes {
		keep[c] = true
	}
	changed := false
	next := make([]Warning, len(ws))
	for i, w := range ws {
		if !keep[w.ViewportClass] && w.Active() {
			reason := "the " + w.ViewportClass + " viewport is no longer audited, so this warning can no longer be re-checked"
			w.Status, w.ObsoleteReason = Obsolete, reason
			w = w.withHistory(at, revision, Obsolete, reason)
			changed = true
		}
		next[i] = w
	}
	return next, changed
}

// ViewportClassesFromEnv reads REDLINE_DIAGNOSTIC_VIEWPORTS (comma-separated
// classes; empty or invalid means all).
func ViewportClassesFromEnv(value string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		c := strings.ToLower(strings.TrimSpace(part))
		for _, known := range ViewportClasses {
			if c == known && !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	if len(out) == 0 {
		return append([]string(nil), ViewportClasses...)
	}
	return out
}

// prune keeps every unresolved record and trims the closed tail.
func prune(ws []Warning) []Warning {
	if len(ws) <= maxStored {
		return ws
	}
	active := ActiveCount(ws)
	room := max(0, maxStored-active)
	closedSeen := 0
	closedTotal := len(ws) - active
	var out []Warning
	for _, w := range ws {
		if w.Active() {
			out = append(out, w)
			continue
		}
		closedSeen++
		if closedSeen > closedTotal-room {
			out = append(out, w)
		}
	}
	return out
}

func sameWarning(a, b Warning) bool { return reflect.DeepEqual(a, b) }

// Title and Explanation describe a warning for people.
func (w Warning) Title() string {
	switch w.Rule {
	case "page-horizontal-overflow":
		return "Page scrolls sideways"
	case "clipped-text":
		return "Text cut off by its container"
	case "clipped-control":
		return "Control cut off by its container"
	case "viewport-unreachable-control":
		return "Control outside the viewport"
	case "viewport-unreachable-content":
		return "Text outside the viewport"
	case "overlapping-text":
		return "Text covered by another element"
	}
	return "Layout failure"
}

func px(v float64) string { return fmt.Sprintf("%dpx", int(math.Round(v))) }

func edge(axis string) string {
	if axis == "vertical" {
		return "bottom"
	}
	return "right"
}

// Explanation says what the browser proved.
func (w Warning) Explanation() string {
	switch w.Rule {
	case "page-horizontal-overflow":
		return fmt.Sprintf("The page is %s wider than the %s viewport, so content sits off-screen.", px(w.OverflowPx), px(float64(w.ViewportWidth)))
	case "clipped-text":
		return fmt.Sprintf("Rendered text crosses its container's %s edge by %s and is hidden.", edge(w.Axis), px(w.OverflowPx))
	case "clipped-control":
		return fmt.Sprintf("A required control crosses its container's %s edge by %s, so part of it cannot be used.", edge(w.Axis), px(w.OverflowPx))
	case "viewport-unreachable-control":
		return fmt.Sprintf("A required control sits %s outside the %s edge of the viewport and cannot be reached.", px(w.OverflowPx), edge(w.Axis))
	case "viewport-unreachable-content":
		return fmt.Sprintf("Rendered text sits %s outside the %s edge of the viewport and cannot be read.", px(w.OverflowPx), edge(w.Axis))
	case "overlapping-text":
		return "An opaque sibling covers nearly all of this text, so it cannot be read."
	}
	return "The browser proved a severe layout failure on this element."
}

// StatusLabel names a status for people.
func StatusLabel(status string) string {
	return map[string]string{
		Open: "Open", Queued: "Queued for fix", Recurring: "Still present", Unverified: "Unverified",
		Reopened: "Returned", Resolved: "Resolved", Dismissed: "Dismissed", Obsolete: "Obsolete",
	}[status]
}

// RecentHistory is the tail of a warning's history shown to people.
func (w Warning) RecentHistory() []Event {
	if len(w.History) <= maxSerialHistory {
		return w.History
	}
	return w.History[len(w.History)-maxSerialHistory:]
}

// Prompt is the agent-facing text and target for one queued batch.
func Prompt(queued []Warning) (string, []byte) {
	var lines []string
	for i, w := range queued {
		lines = append(lines, fmt.Sprintf("%d. [%s] %s - %s Target: %s. Viewport: %s (%dpx). Status: %s.",
			i+1, w.ID, w.Title(), w.Explanation(), orPage(w.Selector), ViewportLabel(w.ViewportClass), w.ViewportWidth, StatusLabel(w.Status)))
	}
	what := "this layout issue"
	if len(queued) != 1 {
		what = fmt.Sprintf("these %d layout issues", len(queued))
	}
	prompt := "Fix " + what + " the browser detected in this artifact:\n" + strings.Join(lines, "\n") +
		"\n\nApply every listed fix in one pass before saving so the review refreshes once. A queued layout issue is a repair request, not a resolved issue: redline only marks it resolved after a newer artifact revision and a complete check at the same viewport no longer detects it."

	type item struct {
		ID            string  `json:"id"`
		Rule          string  `json:"rule"`
		Selector      string  `json:"selector"`
		Component     string  `json:"component"`
		Axis          string  `json:"axis"`
		OverflowPx    float64 `json:"overflow_px"`
		ViewportClass string  `json:"viewport_class"`
		ViewportWidth int     `json:"viewport_width"`
	}
	target := struct {
		Type     string `json:"type"`
		Revision int    `json:"artifact_revision"`
		Warnings []item `json:"warnings"`
	}{Type: "layout-warnings"}
	for _, w := range queued {
		target.Revision = w.QueuedRevision
		target.Warnings = append(target.Warnings, item{w.ID, w.Rule, w.Selector, w.Component, w.Axis, w.OverflowPx, w.ViewportClass, w.ViewportWidth})
	}
	b, _ := json.Marshal(target)
	return prompt, b
}

func orPage(selector string) string {
	if selector == "" {
		return "(page)"
	}
	return selector
}
