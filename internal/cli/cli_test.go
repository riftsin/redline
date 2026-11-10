package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	for _, flag := range []string{"--version", "-v", "-V"} {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{flag}, &stdout, &stderr); code != 0 {
			t.Fatalf("%s: exit %d, stderr %q", flag, code, stderr.String())
		}
		if got := strings.TrimSpace(stdout.String()); got != Version() {
			t.Fatalf("%s: printed %q, want %q", flag, got, Version())
		}
	}
}
