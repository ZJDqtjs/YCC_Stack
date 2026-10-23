package main

import (
	"path/filepath"
	"testing"
)

// Two launchers on one machine share --work, so the cached credential that lets a
// node re-join without a fresh launch code must not be shareable between them.
func TestNodeSecretIsScopedToNodeID(t *testing.T) {
	work := t.TempDir()
	const secret = "0123456789abcdef0123456789abcdef"

	a := &agent{id: "web-aaaa", work: work}
	a.setSecret(secret)
	if a.nodeSecret() != secret {
		t.Fatalf("secret after setSecret = %q", a.nodeSecret())
	}

	b := &agent{id: "web-bbbb", work: work}
	b.loadSecret()
	if b.nodeSecret() != "" {
		t.Fatalf("node web-bbbb loaded web-aaaa's credential: %q", b.nodeSecret())
	}

	restarted := &agent{id: "web-aaaa", work: work}
	restarted.loadSecret()
	if restarted.nodeSecret() != secret {
		t.Fatalf("returning node got %q, want the cached credential", restarted.nodeSecret())
	}
	if got := filepath.Base(restarted.secretFile()); got != "node-web-aaaa.key" {
		t.Fatalf("credential file = %s", got)
	}
}
