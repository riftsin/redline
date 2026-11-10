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
