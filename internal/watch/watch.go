// Package watch notices when a file changes on disk by polling its size and
// modification time. The standard library has no file-event API, and
// polling a handful of reviewed files costs almost nothing; it also handles
// editors that save by renaming a temp file over the original, since only
// the path is watched.
package watch

import (
	"crypto/sha256"
	"os"
	"time"
)

// Interval is how often a watched file is checked.
const Interval = 200 * time.Millisecond

// Settle is how long a file must stay unchanged after a change before it is
// reported, so a save written in several steps reloads once.
const Settle = 120 * time.Millisecond

// recent is how close to now a modification time must be for its file to be
// hashed too. Some filesystems keep modification times on a coarse clock,
// so a quick same-size rewrite can leave size and time unchanged; only the
// content tells. Older files are compared by size and time alone, which
// keeps polling cheap.
const recent = 2 * time.Second

type stamp struct {
	exists bool
	size   int64
	mod    time.Time
	hash   [sha256.Size]byte // set only for recently modified files
}

func statOf(path string) stamp {
	info, err := os.Stat(path)
	if err != nil {
		return stamp{}
	}
	s := stamp{exists: true, size: info.Size(), mod: info.ModTime()}
	if time.Since(s.mod) < recent {
		if data, err := os.ReadFile(path); err == nil {
			s.hash = sha256.Sum256(data)
		}
	}
	return s
}

// same reports whether two observations show the same file. A content hash
// is compared only when both have one: a file aging out of "recent" stops
// being hashed without having changed.
func same(a, b stamp) bool {
	if a.exists != b.exists || a.size != b.size || !a.mod.Equal(b.mod) {
		return false
	}
	var zero [sha256.Size]byte
	if a.hash == zero || b.hash == zero {
		return true
	}
	return a.hash == b.hash
}

// File calls onChange (from its own goroutine) each time path settles into
// a new state, until stop is called. A file that is deleted and recreated
// counts as changed.
func File(path string, onChange func()) (stop func()) {
	done := make(chan struct{})
	// The baseline is taken before returning, so a write right after File
	// returns is always a change.
	reported := statOf(path)
	go func() {
		ticker := time.NewTicker(Interval)
		defer ticker.Stop()
		var pending *stamp
		var pendingSince time.Time
		for {
			select {
			case <-done:
				return
			case now := <-ticker.C:
				current := statOf(path)
				switch {
				case same(current, reported):
					pending = nil
				case pending == nil || !same(*pending, current):
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
