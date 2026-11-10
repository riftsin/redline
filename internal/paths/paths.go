// Package paths owns redline's filesystem identity and locations.
//
// A review session is identified by the canonical (absolute, symlink-free)
// path of its HTML file, so two spellings of one file are one session. The
// session key derived from that path is not a secret: anyone who knows the
// path can compute it, so nothing may treat key possession as authorization.
package paths

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// StateDir is redline's state directory: REDLINE_STATE_DIR, or ~/.redline.
func StateDir() (string, error) {
	if dir := os.Getenv("REDLINE_STATE_DIR"); dir != "" {
		return filepath.Abs(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".redline"), nil
}

// SessionKey derives the session key for a canonical file path: the first 16
// hex characters of its SHA-256.
func SessionKey(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])[:16]
}

// IsHTMLPath reports whether path ends in .html or .htm, in any case.
func IsHTMLPath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".html", ".htm":
		return true
	}
	return false
}

// ErrNotHTML is returned for a path that is not an existing regular HTML file.
var ErrNotHTML = errors.New("not an HTML file (.html or .htm)")

// CanonicalHTML validates an artifact path and returns its canonical form.
// Both the requested name and the symlink-resolved path must be HTML files,
// so "preview.html -> /etc/passwd" cannot adopt /etc as a serving tree.
func CanonicalHTML(path string) (string, error) {
	if path == "" {
		return "", errors.New("an HTML file path is required")
	}
	if !IsHTMLPath(path) {
		return "", fmt.Errorf("%s is %w", path, ErrNotHTML)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%s does not exist", path)
		}
		return "", err
	}
	if !IsHTMLPath(canonical) {
		return "", fmt.Errorf("%s resolves to %s, which is %w", path, canonical, ErrNotHTML)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	return canonical, nil
}

// Inside reports whether the canonical path is dir or lies inside it.
func Inside(path, dir string) bool {
	if path == dir {
		return true
	}
	return strings.HasPrefix(path, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}

// ErrOutsideTree is returned for an asset path that escapes the artifact's
// directory, lexically or through a symlink.
var ErrOutsideTree = errors.New("outside the artifact's directory")

// ResolveAsset maps a URL path below an artifact (already unescaped, using
// "/" separators) to a canonical file inside dir, the artifact's directory.
func ResolveAsset(dir, urlPath string) (string, error) {
	rel := strings.TrimPrefix(urlPath, "/")
	if rel == "" {
		return "", ErrOutsideTree
	}
	for _, part := range strings.Split(rel, "/") {
		if part == ".." {
			return "", ErrOutsideTree
		}
	}
	candidate := filepath.Join(dir, filepath.FromSlash(rel))
	if !Inside(candidate, dir) {
		return "", ErrOutsideTree
	}
	canonical, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	if !Inside(canonical, dir) {
		return "", ErrOutsideTree
	}
	return canonical, nil
}
