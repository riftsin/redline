package server

import (
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// guard applies the two request guards in order:
//
//  1. Host allowlist (DNS-rebinding defense): the Host header, and the
//     outermost X-Forwarded-Host behind a proxy, must be a name this server
//     answers to.
//  2. Origin guard: a state-changing request (anything but GET/HEAD/OPTIONS)
//     whose Origin, or else Referer, is present must come from this server.
//     Header-less CLI requests carry neither and pass; the Host allowlist
//     is their gate. A page on another origin can never queue feedback,
//     end a session or stop the server.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.plan.AllowedHost(stripPort(r.Host)) || !s.forwardedHostAllowed(r) {
			http.Error(w, "Forbidden host", http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if source := requestSource(r); source != "" && !sameOrigin(r, source) {
				http.Error(w, "Forbidden origin", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

var authorityPattern = regexp.MustCompile(`^(\[[0-9a-fA-F:.]+\]|[A-Za-z0-9.-]+)(:\d{1,5})?$`)

func (s *Server) forwardedHostAllowed(r *http.Request) bool {
	value := r.Header.Get("X-Forwarded-Host")
	if value == "" {
		return true
	}
	authority := strings.TrimSpace(strings.Split(value, ",")[0])
	return authorityPattern.MatchString(authority) && s.plan.AllowedHost(stripPort(authority))
}

// stripPort removes a port from a Host value, keeping IPv6 brackets off.
func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return strings.Trim(host, "[]")
}

func requestSource(r *http.Request) string {
	if origin := r.Header.Get("Origin"); origin != "" {
		return origin
	}
	return r.Header.Get("Referer")
}

// sameOrigin compares source's scheme and authority with this request's own
// (as seen by the client, through a proxy if any).
func sameOrigin(r *http.Request, source string) bool {
	u, err := url.Parse(source)
	if err != nil || u.Host == "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = strings.ToLower(strings.TrimSpace(strings.Split(proto, ",")[0]))
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	return strings.EqualFold(u.Scheme, scheme) && normalizeAuthority(u.Host, u.Scheme) == normalizeAuthority(host, scheme)
}

func normalizeAuthority(authority, scheme string) string {
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		host, port = strings.Trim(authority, "[]"), ""
	}
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	host = strings.ToLower(host)
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port == "" {
		return host
	}
	return host + ":" + port
}
