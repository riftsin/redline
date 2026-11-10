package watch

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestFileReportsSettledChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.html")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	var changes atomic.Int32
	stop := File(path, func() { changes.Add(1) })
	defer stop()

	// A burst of writes reloads once.
	for _, v := range []string{"v2", "v22", "v222"} {
		if err := os.WriteFile(path, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Millisecond)
	}
	waitFor(t, func() bool { return changes.Load() == 1 })

	// An editor-style save: write a temp file and rename it over the original.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte("renamed into place"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return changes.Load() == 2 })

	time.Sleep(3 * Interval)
	if n := changes.Load(); n != 2 {
		t.Fatalf("a quiet file reported %d changes, want 2", n)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Some filesystems stamp modification times with a coarse clock, so a quick
// same-size rewrite can keep the old size and time. It is still a change.
func TestSameSizeRewriteWithinOneClockTick(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.html")
	if err := os.WriteFile(path, []byte("<p>v1</p>"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	var changes atomic.Int32
	stop := File(path, func() { changes.Add(1) })
	defer stop()

	if err := os.WriteFile(path, []byte("<p>v2</p>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return changes.Load() == 1 })
}

func TestTreeReportsSiblingChanges(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("plan.html", "<p>x</p>")
	write("css/site.css", "a{}")
	var changes atomic.Int32
	stop := Tree(root, filepath.Join(root, "plan.html"), func() { changes.Add(1) })
	defer stop()

	write("css/site.css", "a{color:red}")
	waitFor(t, func() bool { return changes.Load() == 1 })
	write("img/new.png", "png")
	waitFor(t, func() bool { return changes.Load() == 2 })

	// The artifact itself (watched on its own), hidden files, dependency
	// folders and exports are ignored.
	write("plan.html", "<p>changed</p>")
	write(".cache/x", "1")
	write("node_modules/lib/x.js", "1")
	write("plan.export.html", "export")
	time.Sleep(3 * TreeInterval)
	if n := changes.Load(); n != 2 {
		t.Fatalf("ignored files caused %d changes", n-2)
	}
}
