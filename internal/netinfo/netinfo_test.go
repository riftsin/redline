package netinfo

import (
	"net/netip"
	"testing"
)

func envOf(m map[string]string) Env {
	return func(k string) string { return m[k] }
}

func noTailscale() []byte { return nil }

const runningStatus = `{
  "BackendState": "Running",
  "Self": {"DNSName": "MacBook.tail1234.ts.net.", "TailscaleIPs": ["fd7a:115c::1", "100.64.0.7"]}
}`

func TestResolveTailscale(t *testing.T) {
	plan := Resolve(envOf(nil), func() []byte { return []byte(runningStatus) })
	if len(plan.Extra) != 1 || plan.Extra[0] != netip.MustParseAddr("100.64.0.7") {
		t.Fatalf("extra listeners = %v", plan.Extra)
	}
	if plan.LinkHost != "macbook.tail1234.ts.net" || plan.Warning != "" {
		t.Fatalf("plan = %+v", plan)
	}
	for _, h := range []string{"localhost", "127.0.0.1", "[::1]", "100.64.0.7", "MACBOOK.tail1234.ts.net"} {
		if !plan.AllowedHost(h) {
			t.Errorf("%s should be allowed", h)
		}
	}
	if plan.AllowedHost("evil.example") || plan.AllowedHost("") {
		t.Error("unknown and empty hosts must be refused")
	}
}

func TestResolveTailscaleWithoutMagicDNS(t *testing.T) {
	status := `{"BackendState":"Running","Self":{"DNSName":"","TailscaleIPs":["100.64.0.7"]}}`
	plan := Resolve(envOf(nil), func() []byte { return []byte(status) })
	if len(plan.Extra) != 0 || plan.LinkHost != "127.0.0.1" || plan.Warning == "" {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestResolveTailscaleStopped(t *testing.T) {
	for _, status := range []string{``, `not json`, `{"BackendState":"Stopped","Self":{"TailscaleIPs":["100.64.0.7"]}}`} {
		plan := Resolve(envOf(nil), func() []byte { return []byte(status) })
		if len(plan.Extra) != 0 || plan.Warning != "" {
			t.Errorf("status %q: plan = %+v", status, plan)
		}
	}
}

func TestResolveExplicitHost(t *testing.T) {
	called := false
	plan := Resolve(envOf(map[string]string{"REDLINE_HOST": "192.168.1.20"}), func() []byte { called = true; return nil })
	if called {
		t.Error("REDLINE_HOST must skip Tailscale detection")
	}
	if len(plan.Extra) != 1 || plan.Extra[0].String() != "192.168.1.20" || plan.LinkHost != "192.168.1.20" {
		t.Fatalf("plan = %+v", plan)
	}

	for _, wildcard := range []string{"0.0.0.0", "::", "*"} {
		plan := Resolve(envOf(map[string]string{"REDLINE_HOST": wildcard}), noTailscale)
		if len(plan.Extra) != 0 {
			t.Errorf("wildcard %q must stay on loopback, got %v", wildcard, plan.Extra)
		}
	}
	plan = Resolve(envOf(map[string]string{"REDLINE_HOST": "127.0.0.1"}), noTailscale)
	if len(plan.Extra) != 0 {
		t.Errorf("loopback host added a listener: %v", plan.Extra)
	}
}

func TestAllowedHostsOverrides(t *testing.T) {
	plan := Resolve(envOf(map[string]string{"REDLINE_ALLOWED_HOSTS": " proxy.example  other.example "}), noTailscale)
	if !plan.AllowedHost("proxy.example") || !plan.AllowedHost("other.example") {
		t.Error("extra allowed hosts missing")
	}
	plan = Resolve(envOf(map[string]string{"REDLINE_ALLOWED_HOSTS": "*"}), noTailscale)
	if !plan.AllowedHost("anything.example") {
		t.Error("* must allow every host")
	}
	plan = Resolve(envOf(map[string]string{"REDLINE_LINK_HOST": "box.lan"}), noTailscale)
	if plan.LinkHost != "box.lan" || !plan.AllowedHost("box.lan") {
		t.Errorf("link host override: %+v", plan)
	}
}

func TestBaseURL(t *testing.T) {
	if got := (Plan{LinkHost: "::1"}).BaseURL(4389); got != "http://[::1]:4389" {
		t.Errorf("IPv6 base URL = %q", got)
	}
	if got := (Plan{LinkHost: "127.0.0.1"}).BaseURL(4389); got != "http://127.0.0.1:4389" {
		t.Errorf("base URL = %q", got)
	}
}
