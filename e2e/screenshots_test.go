//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestScreenshots captures the review page in its main states for visual
// review. It runs only when REDLINE_SCREENSHOT_DIR names an output folder.
func TestScreenshots(t *testing.T) {
	out := os.Getenv("REDLINE_SCREENSHOT_DIR")
	if out == "" {
		t.Skip("set REDLINE_SCREENSHOT_DIR to capture screenshots")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, theme := range []struct {
		name string
		dark bool
	}{{"light", false}, {"dark", true}} {
		for _, size := range []struct {
			name          string
			width, height int
		}{{"desktop", 1280, 800}, {"phone", 390, 844}} {
			t.Run(theme.name+"-"+size.name, func(t *testing.T) {
				r := newReview(t, "")
				r.page.SetViewport(size.width, size.height, theme.dark)
				shot := func(state string) {
					time.Sleep(400 * time.Millisecond) // let the sheet finish sliding
					r.page.Screenshot(filepath.Join(out, size.name+"-"+theme.name+"-"+state+".png"))
				}
				shot("1-empty")

				r.page.ClickInFrame("/artifact/", "#title")
				r.page.WaitFor(cardOpen)
				r.page.InsertText("Make the title bigger and mention the date")
				shot("2-card")

				r.page.Press("Enter", 0)
				r.page.WaitFor(`document.querySelector('.queued-bubble')`)
				feedback := r.poll()
				r.page.WaitFor(`document.getElementById('presence').textContent.includes('listening')`)
				shot("3-queued")

				r.page.Click("#send")
				awaitFeedback(t, feedback)
				if err := r.m.Reply(context.Background(), r.key, "Done: the title is bigger and now says “Launch plan – March 3”.\nAnything else?"); err != nil {
					t.Fatal(err)
				}
				r.page.WaitFor(`document.querySelector('.bubble.agent')`)
				shot("4-conversation")

				if _, err := r.m.End(context.Background(), r.key, "user"); err != nil {
					t.Fatal(err)
				}
				r.page.WaitFor(`document.getElementById('review').classList.contains('is-ended')`)
				shot("5-ended")
			})
		}
	}
}
