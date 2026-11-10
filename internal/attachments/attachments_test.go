package attachments

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/riftsin/redline/internal/store"
)

func newStore(t *testing.T, limits Limits) (*Store, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "redline.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := Open(filepath.Join(dir, "attachments"), db, limits)
	if err != nil {
		t.Fatal(err)
	}
	return s, db
}

func pngBytes(t *testing.T, w, h int, shade uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{shade, 0, 0, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// webpLossless is a minimal VP8L header for a 300x150 image (enough for
// Inspect, which reads only the header).
func webpLossless() []byte {
	w, h := uint32(300-1), uint32(150-1)
	bits := w | h<<14
	payload := []byte{0x2f, byte(bits), byte(bits >> 8), byte(bits >> 16), byte(bits >> 24), 0, 0, 0, 0, 0}
	data := []byte("RIFF\x00\x00\x00\x00WEBPVP8L")
	data = append(data, byte(len(payload)), 0, 0, 0)
	return append(data, payload...)
}

func TestInspect(t *testing.T) {
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, image.NewGray(image.Rect(0, 0, 40, 30)), nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name          string
		data          []byte
		mime          string
		width, height int
	}{
		{"png", pngBytes(t, 64, 32, 1), "image/png", 64, 32},
		{"jpeg", jpg.Bytes(), "image/jpeg", 40, 30},
		{"webp", webpLossless(), "image/webp", 300, 150},
	} {
		mime, w, h, err := Inspect(c.data)
		if err != nil || mime != c.mime || w != c.width || h != c.height {
			t.Errorf("%s: %s %dx%d %v", c.name, mime, w, h, err)
		}
	}
	for _, bad := range [][]byte{[]byte("GIF89a..."), []byte("<svg/>"), {}, []byte("\x89PNG\r\n\x1a\ntruncated")} {
		if _, _, _, err := Inspect(bad); !errors.Is(err, ErrUnsupported) {
			t.Errorf("Inspect(%q) = %v, want ErrUnsupported", bad, err)
		}
	}
}

func TestSaveDeduplicatesAndDescribes(t *testing.T) {
	s, _ := newStore(t, DefaultLimits)
	ctx := context.Background()
	data := pngBytes(t, 10, 10, 7)
	a, err := s.Save(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Save(ctx, bytes.NewReader(data))
	if err != nil || b.ID != a.ID {
		t.Fatalf("the same bytes must keep one identity: %v %v", a.ID, b.ID)
	}
	d, ok := s.Describe(store.AttachmentRef{ID: a.ID, Name: "shot.png"})
	if !ok || d.Mime != "image/png" || d.Width != 10 || d.Name != "shot.png" || filepath.Ext(d.Path) != ".png" {
		t.Fatalf("describe = %+v %v", d, ok)
	}
	info, err := os.Stat(d.Path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v %v", info.Mode(), err)
	}
	if _, ok := s.Describe(store.AttachmentRef{ID: "../../etc/passwd"}); ok {
		t.Fatal("an invalid id must not resolve")
	}
}

func TestSaveRefusesOversize(t *testing.T) {
	limits := DefaultLimits
	limits.MaxBytes = 100
	s, _ := newStore(t, limits)
	if _, err := s.Save(context.Background(), bytes.NewReader(pngBytes(t, 200, 200, 1))); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize: %v", err)
	}
}

func TestSweepKeepsReferencedImages(t *testing.T) {
	limits := DefaultLimits
	limits.TTL = time.Hour
	s, db := newStore(t, limits)
	ctx := context.Background()
	old, _ := s.Save(ctx, bytes.NewReader(pngBytes(t, 4, 4, 1)))
	kept, _ := s.Save(ctx, bytes.NewReader(pngBytes(t, 4, 4, 2)))

	if _, _, err := db.OpenSession(ctx, "k", "/tmp/k.html", false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Enqueue(ctx, store.QueueItem{SessionKey: "k", Kind: "annotation", Prompt: "see", Attachments: []store.AttachmentRef{{ID: kept.ID}}}); err != nil {
		t.Fatal(err)
	}
	n, err := s.Sweep(ctx, time.Now().Add(2*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("sweep removed %d, %v", n, err)
	}
	if _, ok := s.Describe(store.AttachmentRef{ID: old.ID}); ok {
		t.Error("the expired unreferenced image survived")
	}
	if _, ok := s.Describe(store.AttachmentRef{ID: kept.ID}); !ok {
		t.Error("a queued note's image was swept")
	}
}

func TestDiskBudgetEvictsUnreferenced(t *testing.T) {
	ctx := context.Background()
	first := pngBytes(t, 50, 50, 1)
	limits := DefaultLimits
	limits.MaxDiskBytes = int64(len(first)) + 10
	s, _ := newStore(t, limits)
	a, err := s.Save(ctx, bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, bytes.NewReader(pngBytes(t, 50, 50, 2))); err != nil {
		t.Fatalf("second save: %v", err)
	}
	if _, ok := s.Describe(store.AttachmentRef{ID: a.ID}); ok {
		t.Fatal("the oldest unreferenced image should have made room")
	}
}

func TestFromEnv(t *testing.T) {
	env := map[string]string{"REDLINE_ATTACHMENT_TTL_MS": "off", "REDLINE_MAX_ATTACHMENT_DISK_MB": "64", "REDLINE_MAX_ATTACHMENT_BYTES": "junk"}
	l := FromEnv(func(k string) string { return env[k] })
	if l.TTL != 0 || l.MaxDiskBytes != 64<<20 || l.MaxBytes != DefaultLimits.MaxBytes {
		t.Fatalf("limits = %+v", l)
	}
}

// Sending a note resolves its images inside the send's transaction; that
// must not need a second database connection (SQLite has one).
func TestSendWithImageDoesNotDeadlock(t *testing.T) {
	s, db := newStore(t, DefaultLimits)
	ctx := context.Background()
	a, err := s.Save(ctx, bytes.NewReader(pngBytes(t, 8, 8, 3)))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.OpenSession(ctx, "k", "/tmp/k.html", false); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := db.SendQueue(ctx, store.SendRequest{Key: "k", Message: "see", MessageAttachments: []store.AttachmentRef{{ID: a.ID, Name: "x.png"}}, Describe: s.Describe})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("send with an image deadlocked")
	}
	fb, _ := db.TakeFeedback(ctx, "k")
	if got := fb.Prompts[0].Attachments; len(got) != 1 || got[0].Width != 8 || got[0].Bytes == 0 || got[0].Name != "x.png" {
		t.Fatalf("attachments = %+v", got)
	}
}
