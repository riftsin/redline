// Package server is redline's background HTTP server: the CLI's control API
// and long poll, the review pages and their live updates, and the artifact
// route that serves reviewed files into a sandboxed iframe.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/riftsin/redline/internal/attachments"
	"github.com/riftsin/redline/internal/netinfo"
	"github.com/riftsin/redline/internal/review"
	"github.com/riftsin/redline/web"
)

// Info identifies a running server to the CLI.
type Info struct {
	Version string
	// BuildID distinguishes two local (devel) builds of the same version.
	BuildID string
	// StateID identifies the state directory, so a CLI never adopts a
	// server that keeps its state somewhere else.
	StateID  string
	StateDir string
}

// Server is one running redline server.
type Server struct {
	m    *review.Manager
	att  *attachments.Store // nil disables image attachments
	plan netinfo.Plan
	port int
	info Info

	handler http.Handler
	stop    chan string // shutdown reason
	stopped sync.Once

	mu      sync.Mutex
	servers []*http.Server
	hosts   []string
}

// New builds a server around a review manager. att may be nil, which
// disables image attachments.
func New(m *review.Manager, att *attachments.Store, plan netinfo.Plan, port int, info Info) *Server {
	s := &Server{m: m, att: att, plan: plan, port: port, info: info, stop: make(chan string, 1)}
	s.handler = s.guard(s.routes())
	return s
}

// Handler is the server's full HTTP handler, guards included.
func (s *Server) Handler() http.Handler { return s.handler }

// Stopping is closed-over by Run; it yields the shutdown reason once a
// client asked the server to stop.
func (s *Server) Stopping() <-chan string { return s.stop }

func (s *Server) requestStop(reason string) {
	s.stopped.Do(func() { s.stop <- reason })
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(web.Static, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", noCache(http.FileServerFS(static))))

	// The CLI's control channel.
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("POST /shutdown", s.shutdown)
	mux.HandleFunc("GET /api/sessions", s.listSessions)
	mux.HandleFunc("POST /api/sessions", s.openSession)
	mux.HandleFunc("POST /api/reply", s.reply)
	mux.HandleFunc("POST /api/end", s.agentEnd)
	mux.HandleFunc("GET /api/poll", s.poll)
	mux.HandleFunc("POST /api/poll", s.poll)

	// Review pages and their actions.
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /session/{key}", s.reviewPage)
	mux.HandleFunc("GET /events/{key}", s.events)
	mux.HandleFunc("POST /api/{key}/queue", s.enqueue)
	mux.HandleFunc("POST /api/{key}/queue/{id}/remove", s.removeQueued)
	mux.HandleFunc("POST /api/{key}/queue/{id}/edit", s.editQueued)
	mux.HandleFunc("POST /api/{key}/attachments", s.uploadAttachment)
	mux.HandleFunc("GET /attachments/{id}", s.attachment)
	mux.HandleFunc("GET /api/{key}/sources", s.sources)
	mux.HandleFunc("POST /api/{key}/changes", s.recordChange)
	mux.HandleFunc("POST /api/{key}/send", s.send)
	mux.HandleFunc("POST /api/{key}/end", s.userEnd)
	mux.HandleFunc("POST /api/{key}/artifact-failures", s.artifactFailures)

	// The artifact and its sibling assets, framed by the review page.
	mux.HandleFunc("GET /artifact/{key}/{path...}", s.artifact)
	return mux
}

// noCache makes the browser revalidate embedded files, so a server upgrade
// is picked up by the next page load.
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}

// Serve listens on loopback (the port lock: failing here fails the server)
// and on the plan's extra addresses, retrying any extra address that cannot
// be bound yet. It returns once every listener has stopped.
func (s *Server) Serve(ctx context.Context) error {
	loopback, err := net.Listen("tcp", netip.AddrPortFrom(netinfo.Loopback, uint16(s.port)).String())
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	s.serveOn(loopback, &wg)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, addr := range s.plan.Extra {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.bindWithRetry(ctx, addr, &wg)
		}()
	}

	select {
	case <-ctx.Done():
	case reason := <-s.stop:
		log.Printf("shutting down: %s", reason)
	}
	cancel()
	s.closeAll()
	wg.Wait()
	return nil
}

func (s *Server) serveOn(l net.Listener, wg *sync.WaitGroup) {
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.Default(),
	}
	s.mu.Lock()
	s.servers = append(s.servers, srv)
	host, _, _ := net.SplitHostPort(l.Addr().String())
	s.hosts = append(s.hosts, host)
	s.mu.Unlock()
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("listener %s: %v", l.Addr(), err)
		}
	}()
}

// bindWithRetry keeps trying an extra address (a tailnet address can appear
// after the server started) until it binds or the server stops.
func (s *Server) bindWithRetry(ctx context.Context, addr netip.Addr, wg *sync.WaitGroup) {
	delay := time.Second
	for {
		l, err := net.Listen("tcp", netip.AddrPortFrom(addr, uint16(s.port)).String())
		if err == nil {
			s.serveOn(l, wg)
			return
		}
		log.Printf("listen on %s: %v (retrying)", addr, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 30*time.Second)
	}
}

func (s *Server) closeAll() {
	s.mu.Lock()
	servers := append([]*http.Server(nil), s.servers...)
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, srv := range servers {
		// Long polls and event streams never finish on their own; give
		// ordinary requests a moment, then close everything.
		if err := srv.Shutdown(ctx); err != nil {
			srv.Close()
		}
	}
}

// Hosts lists the addresses currently served.
func (s *Server) Hosts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.hosts...)
}

// -- JSON helpers -------------------------------------------------------------

const maxBody = 1 << 20

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// apiError is the error shape every JSON endpoint uses.
type apiError struct {
	Status string `json:"status"`
	Code   string `json:"code"`
	Error  string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiError{Status: "error", Code: code, Error: message})
}

func itoa(n int) string { return strconv.Itoa(n) }
