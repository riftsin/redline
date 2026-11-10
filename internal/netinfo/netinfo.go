// Package netinfo decides where the server listens and which hostnames it
// answers to.
//
// Loopback (127.0.0.1) is always served: it is the address every CLI probes
// and the port's ownership lock. On top of it:
//
//   - Tailscale (default). When Tailscale runs with an IPv4 address and a
//     valid MagicDNS name, the server also listens on that tailnet address
//     and session links use the MagicDNS name, so a phone on the same
//     tailnet can open them. Never a wildcard address.
//   - REDLINE_HOST overrides detection with one concrete address. Wildcards
//     are reduced to loopback, and a name resolving to an all-interfaces
//     address is refused.
//
// REDLINE_LINK_HOST overrides the host used in session links, and
// REDLINE_ALLOWED_HOSTS (whitespace-separated, or "*") extends the Host
// allowlist, for example behind a reverse proxy.
//
// Listening beyond loopback exposes an unauthenticated server that serves
// local files to every device that can reach it, so that network must be
// trusted.
package netinfo

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultPort is redline's port unless REDLINE_PORT says otherwise. lavish-axi
// uses 4387, so both can run side by side.
const DefaultPort = 4389

// Loopback is the address every server listens on.
var Loopback = netip.MustParseAddr("127.0.0.1")

// Port returns REDLINE_PORT, or DefaultPort when it is unset or invalid.
func Port() int {
	if p, err := strconv.Atoi(strings.TrimSpace(os.Getenv("REDLINE_PORT"))); err == nil && p > 0 && p < 65536 {
		return p
	}
	return DefaultPort
}

// Plan is where the server listens and what it answers to.
type Plan struct {
	// Extra lists the addresses served in addition to Loopback.
	Extra []netip.Addr
	// LinkHost is the host put in session links.
	LinkHost string
	// AllowAllHosts disables the Host allowlist (REDLINE_ALLOWED_HOSTS=*).
	AllowAllHosts bool
	allowed       map[string]bool
	// Warning explains a fallback to loopback, if any.
	Warning string
	// Tailscale is set when the plan came from Tailscale detection.
	Tailscale *Tailscale
}

// Tailscale is this machine's tailnet identity.
type Tailscale struct {
	IP      netip.Addr
	DNSName string
}

// Env reads environment variables; tests substitute a map.
type Env func(string) string

// Resolve builds the plan from the environment, probing Tailscale unless
// REDLINE_HOST is set. status returns `tailscale status --json` output, or
// nil when Tailscale is unavailable.
func Resolve(env Env, status func() []byte) Plan {
	var plan Plan
	if explicit := strings.TrimSpace(env("REDLINE_HOST")); explicit != "" {
		plan = resolveExplicit(explicit)
	} else {
		plan = resolveTailscale(status())
	}
	if link := strings.TrimSpace(env("REDLINE_LINK_HOST")); link != "" {
		plan.LinkHost = link
	}

	extras := strings.Fields(env("REDLINE_ALLOWED_HOSTS"))
	if len(extras) == 1 && extras[0] == "*" {
		plan.AllowAllHosts = true
		return plan
	}
	plan.allowed = map[string]bool{}
	add := func(h string) { plan.allowed[strings.ToLower(strings.Trim(h, "[]"))] = true }
	for _, h := range []string{"localhost", "127.0.0.1", "::1"} {
		add(h)
	}
	for _, ip := range plan.Extra {
		add(ip.String())
	}
	if plan.Tailscale != nil {
		add(plan.Tailscale.DNSName)
	}
	add(plan.LinkHost)
	for _, h := range extras {
		add(h)
	}
	return plan
}

func loopbackPlan(warning string) Plan {
	return Plan{LinkHost: "127.0.0.1", Warning: warning}
}

func resolveExplicit(host string) Plan {
	switch host {
	case "0.0.0.0", "::", "[::]", "*":
		return loopbackPlan("")
	}
	ip, err := lookup(host)
	switch {
	case err != nil:
		return loopbackPlan("REDLINE_HOST " + host + " does not resolve; serving loopback only for now.")
	case ip == Loopback:
		return loopbackPlan("")
	case ip.IsUnspecified():
		return loopbackPlan("REDLINE_HOST " + host + " resolves to all interfaces; staying on loopback.")
	}
	return Plan{Extra: []netip.Addr{ip}, LinkHost: host}
}

func lookup(host string) (netip.Addr, error) {
	bare := strings.Trim(host, "[]")
	if ip, err := netip.ParseAddr(bare); err == nil {
		return ip.Unmap(), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", bare)
	if err != nil {
		return netip.Addr{}, err
	}
	return ips[0].Unmap(), nil
}

func resolveTailscale(raw []byte) Plan {
	ts, state := ParseTailscaleStatus(raw)
	switch state {
	case TailscaleReady:
		return Plan{Extra: []netip.Addr{ts.IP}, LinkHost: ts.DNSName, Tailscale: ts}
	case TailscaleNoMagicDNS:
		return loopbackPlan("Tailscale is running but MagicDNS is unavailable, so there is no phone access. redline remains available on loopback.")
	}
	return loopbackPlan("")
}

// AllowedHost reports whether host (from a Host header, without port) is one
// this server answers to.
func (p Plan) AllowedHost(host string) bool {
	if p.AllowAllHosts {
		return true
	}
	if host == "" {
		return false
	}
	return p.allowed[strings.ToLower(strings.Trim(host, "[]"))]
}

// BaseURL returns the base of session links.
func (p Plan) BaseURL(port int) string {
	return "http://" + net.JoinHostPort(p.LinkHost, strconv.Itoa(port))
}

// TailscaleState classifies `tailscale status --json` output.
type TailscaleState int

const (
	TailscaleDown TailscaleState = iota
	TailscaleNoMagicDNS
	TailscaleReady
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// ParseTailscaleStatus reads `tailscale status --json` output.
func ParseTailscaleStatus(raw []byte) (*Tailscale, TailscaleState) {
	if len(raw) == 0 {
		return nil, TailscaleDown
	}
	var status struct {
		BackendState string
		TailscaleIPs []string
		Self         struct {
			DNSName      string
			TailscaleIPs []string
		}
	}
	if json.Unmarshal(raw, &status) != nil || status.BackendState != "Running" {
		return nil, TailscaleDown
	}
	var ip netip.Addr
	for _, s := range append(status.Self.TailscaleIPs, status.TailscaleIPs...) {
		if a, err := netip.ParseAddr(strings.TrimSpace(s)); err == nil && a.Is4() {
			ip = a
			break
		}
	}
	if !ip.IsValid() {
		return nil, TailscaleDown
	}
	name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(status.Self.DNSName), "."))
	labels := strings.Split(name, ".")
	valid := len(name) <= 253 && len(labels) >= 2
	for _, l := range labels {
		valid = valid && dnsLabel.MatchString(l)
	}
	if !valid {
		return nil, TailscaleNoMagicDNS
	}
	return &Tailscale{IP: ip, DNSName: name}, TailscaleReady
}

// tailscaleCandidates lists where the tailscale CLI may live.
func tailscaleCandidates() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"tailscale",
			"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
			"/opt/homebrew/bin/tailscale",
			"/usr/local/bin/tailscale",
		}
	case "windows":
		pf := os.Getenv("ProgramFiles")
		if pf == "" {
			pf = `C:\Program Files`
		}
		return []string{"tailscale", filepath.Join(pf, "Tailscale", "tailscale.exe")}
	}
	return []string{"tailscale", "/usr/bin/tailscale", "/usr/local/bin/tailscale"}
}

// TailscaleStatus runs `tailscale status --json` with the first candidate
// that resolves to an executable regular file. Unresolved candidates are
// never launched: even an attempted launch of a missing binary can trip
// managed-device software restrictions.
func TailscaleStatus() []byte {
	for _, candidate := range tailscaleCandidates() {
		path, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		out, err := exec.CommandContext(ctx, path, "status", "--json").Output()
		cancel()
		if err == nil {
			return out
		}
	}
	return nil
}
