package server

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/riftsin/redline/internal/artifact"
	"github.com/riftsin/redline/internal/export"
	"github.com/riftsin/redline/internal/paths"
	"github.com/riftsin/redline/internal/store"
	"github.com/riftsin/redline/web"
)

// SDKTag is the one tag injected into a served artifact.
const SDKTag = `<script src="/static/sdk.js" data-redline-sdk></script>`

// artifact serves the reviewed HTML file (its bytes on disk plus SDKTag) and
// its sibling assets, confined by real path to the file's directory: a
// symlink next to the artifact cannot make this route serve an outside file.
func (s *Server) artifact(w http.ResponseWriter, r *http.Request) {
	sandboxHeaders(w)
	sess, err := s.m.Store.Session(r.Context(), r.PathValue("key"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	dir := filepath.Dir(sess.File)
	rel := r.PathValue("path")
	isArtifact := rel == filepath.Base(sess.File)

	real, err := paths.ResolveAsset(dir, rel)
	switch {
	case err == nil && real == sess.File:
		s.serveArtifact(w, r, sess)
	case err == nil:
		serveAsset(w, r, real)
	case isArtifact && errors.Is(err, fs.ErrNotExist):
		s.recordUnavailable(r.Context(), sess, filepath.Base(sess.File)+" no longer exists")
		http.Error(w, "The artifact no longer exists.", http.StatusNotFound)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) serveArtifact(w http.ResponseWriter, r *http.Request, sess *store.Session) {
	src, err := os.ReadFile(sess.File)
	if err != nil {
		s.recordUnavailable(r.Context(), sess, filepath.Base(sess.File)+" could not be read: "+err.Error())
		http.Error(w, "The artifact could not be read.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(artifact.InjectSDK(src, SDKTag))
}

// serveAsset serves a confined sibling file. http.ServeFile is avoided on
// purpose: it redirects any ".../index.html" request.
func serveAsset(w http.ResponseWriter, r *http.Request, real string) {
	f, err := os.Open(real)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// recordUnavailable wakes the agent: an artifact that cannot load leaves the
// reviewer nothing to review.
func (s *Server) recordUnavailable(ctx context.Context, sess *store.Session, detail string) {
	_ = s.m.RecordArtifactFailures(ctx, sess.Key, []store.Failure{{Kind: "artifact-unavailable", Detail: detail}})
}

func sandboxHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "sandbox "+Sandbox)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
}

// design serves the vendored Tailwind/DaisyUI files. They are loaded by
// sandboxed (opaque-origin) artifacts as plain stylesheets and scripts.
func (s *Server) design(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("asset")
	contentType, ok := web.DesignFiles[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(web.Design, "design/"+name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", "public, max-age=86400")
	h.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}

// exportArtifact downloads the portable copy of a session's artifact.
func (s *Server) exportArtifact(w http.ResponseWriter, r *http.Request) {
	sess, err := s.m.Store.Session(r.Context(), r.PathValue("key"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data, warnings, err := export.Bundle(sess.File, export.LimitsFromEnv(os.Getenv))
	if err != nil {
		http.Error(w, "The artifact could not be exported: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(export.FileName(sess.File), `"`, "")+`"`)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Redline-Export-Warnings", strconv.Itoa(len(warnings)))
	_, _ = w.Write(data)
}
