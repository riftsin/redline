package paths

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tempDir returns a canonical temp dir (macOS puts TMPDIR behind /var -> /private/var).
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalHTML(t *testing.T) {
	dir := tempDir(t)
	plan := filepath.Join(dir, "plan.html")
	write(t, plan, "<p>x")
	write(t, filepath.Join(dir, "secret.txt"), "s")
	if err := os.Symlink(plan, filepath.Join(dir, "alias.HTM")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "secret.txt"), filepath.Join(dir, "trap.html")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "folder.html"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := CanonicalHTML(filepath.Join(dir, "sub", "..", "alias.HTM"))
	if err != nil || got != plan {
		t.Fatalf("alias: got %q, %v; want %q", got, err, plan)
	}

	for _, bad := range []string{
		"",
		filepath.Join(dir, "secret.txt"),
		filepath.Join(dir, "trap.html"),
		filepath.Join(dir, "missing.html"),
		filepath.Join(dir, "folder.html"),
	} {
		if got, err := CanonicalHTML(bad); err == nil {
			t.Errorf("CanonicalHTML(%q) = %q, want an error", bad, got)
		}
	}
	if _, err := CanonicalHTML(filepath.Join(dir, "trap.html")); !errors.Is(err, ErrNotHTML) {
		t.Errorf("symlink to a non-HTML file: err = %v, want ErrNotHTML", err)
	}
}

func TestSessionKey(t *testing.T) {
	k := SessionKey("/a/plan.html")
	if len(k) != 16 || strings.Trim(k, "0123456789abcdef") != "" {
		t.Fatalf("key %q is not 16 lowercase hex characters", k)
	}
	if k == SessionKey("/a/other.html") || k != SessionKey("/a/plan.html") {
		t.Fatal("keys must be stable and distinct per path")
	}
}

func TestResolveAsset(t *testing.T) {
	root := tempDir(t)
	dir := filepath.Join(root, "artifact")
	write(t, filepath.Join(dir, "img", "logo.png"), "png")
	write(t, filepath.Join(root, "outside.css"), "css")
	if err := os.Symlink(filepath.Join(root, "outside.css"), filepath.Join(dir, "escape.css")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "img", "logo.png"), filepath.Join(dir, "inside.png")); err != nil {
		t.Fatal(err)
	}

	for in, want := range map[string]string{
		"img/logo.png":  filepath.Join(dir, "img", "logo.png"),
		"/img/logo.png": filepath.Join(dir, "img", "logo.png"),
		"inside.png":    filepath.Join(dir, "img", "logo.png"),
	} {
		if got, err := ResolveAsset(dir, in); err != nil || got != want {
			t.Errorf("ResolveAsset(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "../outside.css", "img/../../outside.css", "escape.css"} {
		if got, err := ResolveAsset(dir, in); !errors.Is(err, ErrOutsideTree) {
			t.Errorf("ResolveAsset(%q) = %q, %v; want ErrOutsideTree", in, got, err)
		}
	}
	if _, err := ResolveAsset(dir, "missing.png"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing asset: err = %v, want not-exist", err)
	}
}

func TestStateDir(t *testing.T) {
	t.Setenv("REDLINE_STATE_DIR", "relative/state")
	got, err := StateDir()
	if err != nil || !filepath.IsAbs(got) || !strings.HasSuffix(got, filepath.Join("relative", "state")) {
		t.Fatalf("StateDir() = %q, %v", got, err)
	}
	t.Setenv("REDLINE_STATE_DIR", "")
	got, err = StateDir()
	if err != nil || filepath.Base(got) != ".redline" {
		t.Fatalf("default StateDir() = %q, %v", got, err)
	}
}
