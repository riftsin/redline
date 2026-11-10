//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shotPNG writes a small striped image to attach.
func shotPNG(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 120, 80))
	for y := 0; y < 80; y++ {
		for x := 0; x < 120; x++ {
			c := color.RGBA{230, 236, 245, 255}
			if (x/12+y/12)%2 == 0 {
				c = color.RGBA{70, 110, 180, 255}
			}
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "spacing.png")
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

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
				r.page.SetInputFiles("input[data-attach=composer]", shotPNG(t))
				r.page.Click("#composer-input")
				r.page.InsertText("Also, the screenshot shows the spacing I mean.")
				r.page.WaitFor(`document.querySelector('[data-chips=composer] .chip') && !document.querySelector('[data-chips=composer] .chip-status')`)
				shot("3-queued")

				r.page.Click("#send")
				awaitFeedback(t, feedback)
				html := strings.Replace(strings.Replace(artifactHTML, "%s", "", 1), "Launch plan", "Launch plan – March 3", 1)
				if err := os.WriteFile(r.file, []byte(html), 0o644); err != nil {
					t.Fatal(err)
				}
				r.page.WaitFor(`!document.getElementById('changes-chip').hidden`)
				if err := r.m.Reply(context.Background(), r.key, "Done:\n- the title is **bigger** and now says *March 3*\n- tightened the `h1` spacing\n\nAnything else?"); err != nil {
					t.Fatal(err)
				}
				r.page.WaitFor(`document.querySelector('.bubble.agent .changes-link')`)
				shot("4-conversation")

				if size.name == "desktop" {
					r.page.Click("#sidebar-toggle")
					shot("4b-collapsed")
					r.page.Click("#sidebar-toggle")
				}

				if _, err := r.m.End(context.Background(), r.key, "user"); err != nil {
					t.Fatal(err)
				}
				r.page.WaitFor(`document.getElementById('review').classList.contains('is-ended')`)
				shot("5-ended")
			})
		}
	}
}

// TestLayoutScreenshots captures the layout-issues drawer.
func TestLayoutScreenshots(t *testing.T) {
	out := os.Getenv("REDLINE_SCREENSHOT_DIR")
	if out == "" {
		t.Skip("set REDLINE_SCREENSHOT_DIR to capture screenshots")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name          string
		width, height int
		dark          bool
	}{{"desktop-light", 1280, 800, false}, {"desktop-dark", 1280, 800, true}, {"phone-light", 390, 844, false}} {
		t.Run(c.name, func(t *testing.T) {
			r := newReview(t, "")
			r.page.SetViewport(c.width, c.height, c.dark)
			if err := os.WriteFile(r.file, []byte(clippedPage), 0o644); err != nil {
				t.Fatal(err)
			}
			r.page.WaitFor(`!document.getElementById('layout-button').hidden`)
			r.page.Click("#layout-button")
			r.page.Click(".layout-item .layout-check")
			time.Sleep(400 * time.Millisecond)
			r.page.Screenshot(filepath.Join(out, "layout-"+c.name+".png"))
		})
	}
}
