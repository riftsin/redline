// Package watch notices when a file changes on disk by polling its size and
// modification time. The standard library has no file-event API, and
// polling a handful of reviewed files costs almost nothing; it also handles
// editors that save by renaming a temp file over the original, since only
// the path is watched.
package watch

import (
	"os"
	"time"
)

// Interval is how often a watched file is checked.
const Interval = 200 * time.Millisecond

// Settle is how long a file must stay unchanged after a change before it is
// reported, so a save written in several steps reloads once.
const Settle = 120 * time.Millisecond

type stamp struct {
	exists bool
	size   int64
	mod    time.Time
}

func statOf(path string) stamp {
	info, err := os.Stat(path)
	if err != nil {
		return stamp{}
	}
	return stamp{exists: true, size: info.Size(), mod: info.ModTime()}
}

// File calls onChange (from its own goroutine) each time path settles into
// a new state, until stop is called. A file that is deleted and recreated
// counts as changed.
func File(path string, onChange func()) (stop func()) {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(Interval)
		defer ticker.Stop()
		reported := statOf(path)
		var pending *stamp
		var pendingSince time.Time
		for {
			select {
			case <-done:
				return
			case now := <-ticker.C:
				current := statOf(path)
				switch {
				case current == reported:
					pending = nil
				case pending == nil || *pending != current:
					pending, pendingSince = &current, now
				case now.Sub(pendingSince) >= Settle:
					reported, pending = current, nil
					if current.exists {
						onChange()
					}
				}
			}
		}
	}()
	return func() { close(done) }
}
