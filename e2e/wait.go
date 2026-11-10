//go:build e2e

package e2e

import (
	"testing"
	"time"
)

// waitUntil polls cond until it holds or DefaultTimeout passes.
func waitUntil(t testing.TB, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(DefaultTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for condition")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
