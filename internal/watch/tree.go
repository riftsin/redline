package watch

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TreeInterval is how often a watched folder is scanned; slower than a
// single file because a scan walks every file.
const TreeInterval = 500 * time.Millisecond

// MaxTreeFiles bounds a folder scan, so opting in on a huge folder cannot
// make the server busy.
const MaxTreeFiles = 2000

// skipName reports files and folders a tree watch ignores: hidden ones,
// dependency folders, and redline's own exports.
func skipName(name string, dir bool) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	if dir {
		return name == "node_modules"
	}
	return strings.HasSuffix(name, ".export.html")
}

func scanTree(root, exclude string) map[string]stamp {
	out := map[string]stamp{}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path != root && skipName(d.Name(), d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() || path == exclude {
			return nil
		}
		if len(out) >= MaxTreeFiles {
			return filepath.SkipAll
		}
		if info, err := os.Stat(path); err == nil {
			out[path] = stamp{exists: true, size: info.Size(), mod: info.ModTime()}
		}
		return nil
	})
	return out
}

func sameTree(a, b map[string]stamp) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w.size != v.size || !w.mod.Equal(v.mod) {
			return false
		}
	}
	return true
}

// Tree calls onChange (from its own goroutine) each time any file under
// root other than exclude settles into a new state, until stop is called.
// The baseline is taken before Tree returns.
func Tree(root, exclude string, onChange func()) (stop func()) {
	done := make(chan struct{})
	reported := scanTree(root, exclude)
	go func() {
		ticker := time.NewTicker(TreeInterval)
		defer ticker.Stop()
		var pending map[string]stamp
		var pendingSince time.Time
		for {
			select {
			case <-done:
				return
			case now := <-ticker.C:
				current := scanTree(root, exclude)
				switch {
				case sameTree(current, reported):
					pending = nil
				case pending == nil || !sameTree(pending, current):
					pending, pendingSince = current, now
				case now.Sub(pendingSince) >= Settle:
					reported, pending = current, nil
					onChange()
				}
			}
		}
	}()
	return func() { close(done) }
}
