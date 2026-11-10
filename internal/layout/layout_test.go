package layout

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func finding(kind, sel string, px float64) Finding {
	return Finding{Kind: kind, Selector: sel, Axis: "horizontal", OverflowPx: px, ViewportWidth: 1280, Severity: "error"}
}

func pass(revision int, complete bool, fs ...Finding) Pass {
	return Pass{Complete: complete, TargetPresenceComplete: complete, ViewportWidth: 1280, Revision: revision, At: t0.Add(time.Duration(revision) * time.Minute), Findings: fs}
}

func statuses(ws []Warning) string {
	var s []string
	for _, w := range ws {
		s = append(s, w.Status)
	}
	return strings.Join(s, ",")
}

func TestLifecycle(t *testing.T) {
	clip := finding("clipped-text", "div.card > p", 40)

	ws, changed := Apply(nil, pass(1, true, clip))
	if !changed || statuses(ws) != Open || ws[0].Component != "p" || ActiveCount(ws) != 1 {
		t.Fatalf("detected: %+v", ws)
	}
	// The identical finding again on the same revision changes nothing.
	if _, changed := Apply(ws, pass(1, true, clip)); changed {
		t.Fatal("a repeat pass rewrote the record")
	}
	// Worse on a later revision: one record, updated.
	ws, _ = Apply(ws, pass(2, true, finding("clipped-text", "div.card > p", 90)))
	if len(ws) != 1 || ws[0].OverflowPx != 90 || ws[0].LastSeenRevision != 2 {
		t.Fatalf("updated: %+v", ws)
	}
	// Absent from a pass on the same revision: not a repair.
	if ws2, _ := Apply(ws, pass(2, true)); statuses(ws2) != Open {
		t.Fatalf("same-revision absence: %s", statuses(ws2))
	}
	// An incomplete pass keeps it as unverified.
	ws, _ = Apply(ws, pass(3, false))
	if statuses(ws) != Unverified || ActiveCount(ws) != 1 {
		t.Fatalf("incomplete: %s", statuses(ws))
	}
	// A pass for another viewport class says nothing about it.
	mobile := pass(4, true)
	mobile.ViewportWidth = 390
	if ws2, _ := Apply(ws, mobile); statuses(ws2) != Unverified {
		t.Fatalf("other viewport: %s", statuses(ws2))
	}
	// Queue it: still counted, not selectable again.
	ws, queued := Queue(ws, []string{ws[0].ID}, 3, t0)
	if len(queued) != 1 || statuses(ws) != Queued || ws[0].Selectable() || ActiveCount(ws) != 1 {
		t.Fatalf("queued: %+v", ws)
	}
	// Still there on a newer revision: recurring, selectable again.
	ws, _ = Apply(ws, pass(4, true, clip))
	if statuses(ws) != Recurring || !ws[0].Selectable() {
		t.Fatalf("recurring: %s", statuses(ws))
	}
	// Gone from a complete pass on a newer revision: resolved.
	ws, _ = Apply(ws, pass(5, true))
	if statuses(ws) != Resolved || ActiveCount(ws) != 0 || !ws[0].QueuedAt.IsZero() {
		t.Fatalf("resolved: %+v", ws[0])
	}
	// Back again later: returned.
	ws, _ = Apply(ws, pass(6, true, clip))
	if statuses(ws) != Reopened {
		t.Fatalf("returned: %s", statuses(ws))
	}
}

func TestDismissLastsOneRevision(t *testing.T) {
	ws, _ := Apply(nil, pass(1, true, finding("overlapping-text", "#intro", 0)))
	ws, ok := Dismiss(ws, ws[0].ID, 1, t0)
	if !ok || statuses(ws) != Dismissed || ActiveCount(ws) != 0 {
		t.Fatalf("dismissed: %s", statuses(ws))
	}
	if ws2, _ := Apply(ws, pass(1, true, finding("overlapping-text", "#intro", 0))); statuses(ws2) != Dismissed {
		t.Fatal("dismissal must hold for the revision it was made on")
	}
	ws, _ = Apply(ws, pass(2, true, finding("overlapping-text", "#intro", 0)))
	if statuses(ws) != Open {
		t.Fatalf("a newer revision still detecting it brings it back: %s", statuses(ws))
	}
}

func TestFingerprintIgnoresMagnitudeAndViewportWidth(t *testing.T) {
	a := Fingerprint("clipped-text", "p", "desktop")
	if a != Fingerprint("clipped-text", " p ", "desktop") || a == Fingerprint("clipped-text", "p", "mobile") {
		t.Fatal("fingerprint identity")
	}
}

func TestObsoleteAndEnv(t *testing.T) {
	mobile := pass(1, true, finding("clipped-text", "p", 10))
	mobile.ViewportWidth = 390
	ws, _ := Apply(nil, mobile)
	ws, changed := MarkObsolete(ws, ViewportClassesFromEnv("desktop"), 1, t0)
	if !changed || statuses(ws) != Obsolete || ws[0].ObsoleteReason == "" {
		t.Fatalf("obsolete: %+v", ws)
	}
	if got := strings.Join(ViewportClassesFromEnv(" Mobile, bogus,mobile"), ","); got != "mobile" {
		t.Fatalf("classes = %s", got)
	}
	if got := strings.Join(ViewportClassesFromEnv(""), ","); got != "mobile,compact,desktop" {
		t.Fatalf("default classes = %s", got)
	}
}

func TestPrompt(t *testing.T) {
	ws, _ := Apply(nil, pass(1, true, finding("page-horizontal-overflow", "html", 120)))
	_, queued := Queue(ws, []string{ws[0].ID}, 1, t0)
	text, target := Prompt(queued)
	if !strings.Contains(text, "Fix this layout issue") || !strings.Contains(text, "Page scrolls sideways - The page is 120px wider than the 1280px viewport") {
		t.Fatalf("prompt:\n%s", text)
	}
	var tg struct {
		Type     string
		Warnings []struct{ ID string }
	}
	if json.Unmarshal(target, &tg) != nil || tg.Type != "layout-warnings" || len(tg.Warnings) != 1 {
		t.Fatalf("target = %s", target)
	}
}

func TestSevereOnly(t *testing.T) {
	f := finding("clipped-text", "p", 10)
	f.Severity = "warning"
	if ws, _ := Apply(nil, pass(1, true, f)); len(ws) != 0 {
		t.Fatal("only severe findings are stored")
	}
}
