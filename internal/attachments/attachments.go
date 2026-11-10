// Package attachments stores the images a reviewer attaches to notes and
// messages. Files are content-addressed (named by the SHA-256 of their
// bytes), owner-only, and validated by their magic bytes: the declared
// content type is never trusted. The agent receives each image as a local
// file path.
package attachments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // registers JPEG for DecodeConfig
	_ "image/png"  // registers PNG for DecodeConfig
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/riftsin/redline/internal/store"
)

// Limits, each overridable with an environment variable (see FromEnv).
type Limits struct {
	MaxBytes     int64         // per image (REDLINE_MAX_ATTACHMENT_BYTES)
	MaxDiskBytes int64         // all images (REDLINE_MAX_ATTACHMENT_DISK_MB); 0 disables
	TTL          time.Duration // unreferenced images expire (REDLINE_ATTACHMENT_TTL_MS); 0 disables
	// DeliveryGrace keeps an image the agent just received, so it can still
	// open the file.
	DeliveryGrace time.Duration
}

// DefaultLimits are used when the environment says nothing.
var DefaultLimits = Limits{
	MaxBytes:      10 << 20,
	MaxDiskBytes:  512 << 20,
	TTL:           7 * 24 * time.Hour,
	DeliveryGrace: 24 * time.Hour,
}

// FromEnv reads the limits from the environment. "0" or "off" disables the
// disk budget and the TTL; empty or invalid values keep the defaults.
func FromEnv(getenv func(string) string) Limits {
	l := DefaultLimits
	parse := func(name string) (int64, bool, bool) {
		v := strings.ToLower(strings.TrimSpace(getenv(name)))
		if v == "" {
			return 0, false, false
		}
		if v == "off" {
			return 0, true, true
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0, false, false
		}
		return n, n == 0, true
	}
	if n, off, ok := parse("REDLINE_MAX_ATTACHMENT_BYTES"); ok && !off {
		l.MaxBytes = n
	}
	if n, off, ok := parse("REDLINE_MAX_ATTACHMENT_DISK_MB"); ok {
		l.MaxDiskBytes = n << 20
		if off {
			l.MaxDiskBytes = 0
		}
	}
	if n, off, ok := parse("REDLINE_ATTACHMENT_TTL_MS"); ok {
		l.TTL = time.Duration(n) * time.Millisecond
		if off {
			l.TTL = 0
		}
	}
	return l
}

// Store keeps images on disk and their metadata in the database.
type Store struct {
	dir    string
	db     *store.Store
	limits Limits
}

// Open prepares the attachment directory (owner-only).
func Open(dir string, db *store.Store, limits Limits) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir, db: db, limits: limits}, nil
}

// Limits returns the store's limits.
func (s *Store) Limits() Limits { return s.limits }

var (
	// ErrTooLarge refuses an image over the per-image cap.
	ErrTooLarge = errors.New("image is too large")
	// ErrUnsupported refuses anything that is not a PNG, JPEG or WebP image.
	ErrUnsupported = errors.New("only PNG, JPEG and WebP images can be attached")
	// ErrDiskFull refuses an image the disk budget cannot hold.
	ErrDiskFull = errors.New("the image store is full")
)

var extByMime = map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp"}

var idPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidID reports whether id can name an attachment.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// Save reads one image (refusing anything over the cap before keeping its
// bytes), validates it, and stores it under its content hash.
func (s *Store) Save(ctx context.Context, r io.Reader) (store.Attachment, error) {
	data, err := io.ReadAll(io.LimitReader(r, s.limits.MaxBytes+1))
	if err != nil {
		return store.Attachment{}, err
	}
	if int64(len(data)) > s.limits.MaxBytes {
		return store.Attachment{}, ErrTooLarge
	}
	mime, width, height, err := Inspect(data)
	if err != nil {
		return store.Attachment{}, err
	}
	sum := sha256.Sum256(data)
	a := store.Attachment{ID: hex.EncodeToString(sum[:]), Mime: mime, Bytes: int64(len(data)), Width: width, Height: height}

	path := s.path(a.ID, mime)
	if _, err := os.Stat(path); err != nil {
		if err := s.makeRoom(ctx, a.Bytes); err != nil {
			return store.Attachment{}, err
		}
		tmp, err := os.CreateTemp(s.dir, "upload-*")
		if err != nil {
			return store.Attachment{}, err
		}
		if err := tmp.Chmod(0o600); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return store.Attachment{}, err
		}
		if _, err := tmp.Write(data); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return store.Attachment{}, err
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmp.Name())
			return store.Attachment{}, err
		}
		if err := os.Rename(tmp.Name(), path); err != nil {
			os.Remove(tmp.Name())
			return store.Attachment{}, err
		}
	}
	if err := s.db.SaveAttachment(ctx, a); err != nil {
		return store.Attachment{}, err
	}
	return a, nil
}

func (s *Store) path(id, mime string) string {
	return filepath.Join(s.dir, id+"."+extByMime[mime])
}

// Path returns the image's file and metadata.
func (s *Store) Path(ctx context.Context, id string) (string, store.Attachment, error) {
	if !ValidID(id) {
		return "", store.Attachment{}, store.ErrNotFound
	}
	a, err := s.db.AttachmentByID(ctx, id)
	if err != nil {
		return "", a, err
	}
	p := s.path(a.ID, a.Mime)
	if _, err := os.Stat(p); err != nil {
		return "", a, store.ErrNotFound
	}
	return p, a, nil
}

// Describe resolves a reference for the agent from the file alone. It runs
// inside the send's database transaction, so it must not use the database
// (SQLite has a single connection): the content-addressed file has
// everything the agent needs.
func (s *Store) Describe(ref store.AttachmentRef) (store.PromptAttachment, bool) {
	if !ValidID(ref.ID) {
		return store.PromptAttachment{}, false
	}
	for mime, ext := range extByMime {
		p := filepath.Join(s.dir, ref.ID+"."+ext)
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		info, statErr := f.Stat()
		head := make([]byte, 256<<10) // room for large EXIF blocks before a JPEG's size
		n, _ := io.ReadFull(f, head)
		f.Close()
		if statErr != nil || !info.Mode().IsRegular() {
			return store.PromptAttachment{}, false
		}
		gotMime, w, h, err := Inspect(head[:n])
		if err != nil || gotMime != mime {
			return store.PromptAttachment{}, false
		}
		return store.PromptAttachment{Path: p, Mime: mime, Width: w, Height: h, Bytes: info.Size(), Name: ref.Name, ID: ref.ID}, true
	}
	return store.PromptAttachment{}, false
}

// makeRoom evicts the oldest unreferenced images until need more bytes fit
// the disk budget.
func (s *Store) makeRoom(ctx context.Context, need int64) error {
	if s.limits.MaxDiskBytes <= 0 {
		return nil
	}
	all, err := s.db.Attachments(ctx)
	if err != nil {
		return err
	}
	var used int64
	for _, a := range all {
		used += a.Bytes
	}
	if used+need <= s.limits.MaxDiskBytes {
		return nil
	}
	refs, err := s.db.ReferencedAttachments(ctx, s.limits.DeliveryGrace)
	if err != nil {
		return err
	}
	for _, a := range all { // least recently used first
		if used+need <= s.limits.MaxDiskBytes {
			break
		}
		if refs[a.ID] {
			continue
		}
		if err := s.remove(ctx, a); err != nil {
			return err
		}
		used -= a.Bytes
	}
	if used+need > s.limits.MaxDiskBytes {
		return ErrDiskFull
	}
	return nil
}

func (s *Store) remove(ctx context.Context, a store.Attachment) error {
	if err := os.Remove(s.path(a.ID, a.Mime)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.db.DeleteAttachment(ctx, a.ID)
}

// Sweep removes unreferenced images unused for longer than the TTL. It
// never removes an image a queued note or an undelivered prompt still uses.
func (s *Store) Sweep(ctx context.Context, now time.Time) (int, error) {
	if s.limits.TTL <= 0 {
		return 0, nil
	}
	all, err := s.db.Attachments(ctx)
	if err != nil {
		return 0, err
	}
	refs, err := s.db.ReferencedAttachments(ctx, s.limits.DeliveryGrace)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, a := range all {
		if refs[a.ID] || now.Sub(a.UsedAt) < s.limits.TTL {
			continue
		}
		if err := s.remove(ctx, a); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// RunSweeper sweeps hourly until ctx ends.
func (s *Store) RunSweeper(ctx context.Context) {
	sweep := func() {
		if n, err := s.Sweep(ctx, time.Now()); err != nil {
			log.Printf("attachment sweep: %v", err)
		} else if n > 0 {
			log.Printf("attachment sweep removed %d expired images", n)
		}
	}
	sweep()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}

// Inspect identifies an image from its magic bytes and reads its size.
func Inspect(data []byte) (mime string, width, height int, err error) {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		mime = "image/png"
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		mime = "image/jpeg"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		w, h, err := webpSize(data)
		if err != nil {
			return "", 0, 0, err
		}
		return "image/webp", w, h, nil
	default:
		return "", 0, 0, ErrUnsupported
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", 0, 0, fmt.Errorf("%w (%v)", ErrUnsupported, err)
	}
	return mime, cfg.Width, cfg.Height, nil
}

// webpSize reads a WebP image's dimensions from its first chunk; the
// standard library has no WebP decoder.
func webpSize(data []byte) (int, int, error) {
	if len(data) < 30 {
		return 0, 0, ErrUnsupported
	}
	chunk := string(data[12:16])
	payload := data[20:]
	switch chunk {
	case "VP8 ": // lossy: frame tag, start code, then 14-bit sizes
		if len(payload) < 10 || payload[3] != 0x9d || payload[4] != 0x01 || payload[5] != 0x2a {
			return 0, 0, ErrUnsupported
		}
		w := int(binary.LittleEndian.Uint16(payload[6:8]) & 0x3fff)
		h := int(binary.LittleEndian.Uint16(payload[8:10]) & 0x3fff)
		return w, h, nil
	case "VP8L": // lossless: signature byte, then 14-bit sizes minus one
		if len(payload) < 5 || payload[0] != 0x2f {
			return 0, 0, ErrUnsupported
		}
		bits := binary.LittleEndian.Uint32(payload[1:5])
		return int(bits&0x3fff) + 1, int(bits>>14&0x3fff) + 1, nil
	case "VP8X": // extended: 24-bit canvas sizes minus one
		if len(payload) < 10 {
			return 0, 0, ErrUnsupported
		}
		w := int(payload[4]) | int(payload[5])<<8 | int(payload[6])<<16
		h := int(payload[7]) | int(payload[8])<<8 | int(payload[9])<<16
		return w + 1, h + 1, nil
	}
	return 0, 0, ErrUnsupported
}
