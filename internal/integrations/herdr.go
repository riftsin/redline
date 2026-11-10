package integrations

import (
	"context"
	"os/exec"
	"time"
)

// HerdrChimeEnabled reports whether a waiting poll should notify Herdr: only
// inside a Herdr-managed pane (HERDR_ENV=1) and only when the user opted in
// with REDLINE_HERDR_CHIME=1.
func HerdrChimeEnabled(getenv func(string) string) bool {
	return getenv("HERDR_ENV") == "1" && getenv("REDLINE_HERDR_CHIME") == "1"
}

// HerdrChime shows a Herdr notification with a chime telling the user the
// review is waiting for them. It runs in the background; a missing herdr
// binary or any failure is ignored and never affects the poll.
func HerdrChime() {
	path, err := exec.LookPath("herdr")
	if err != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, path, "notification", "show", "redline review ready",
			"--body", "The artifact is open and redline is waiting for your feedback.",
			"--sound", "request").Run()
	}()
}
