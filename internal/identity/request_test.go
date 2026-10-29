package identity

import "testing"

// TestCanonicalSessionIDRestartSalt verifies that a synthesized session is
// stable within one process but changes when the process salt changes, which is
// what a daemon restart does.
func TestCanonicalSessionIDRestartSalt(t *testing.T) {
	original := bootNonce
	t.Cleanup(func() { bootNonce = original })

	first := CanonicalSessionID("conversation seed")
	if again := CanonicalSessionID("conversation seed"); again != first {
		t.Fatalf("session id changed within one process: %q then %q", first, again)
	}
	if !isOfficialSessionID(first) {
		t.Fatalf("session id %q is not accepted by the free tier pattern", first)
	}

	bootNonce = []byte("different boot")
	if restarted := CanonicalSessionID("conversation seed"); restarted == first {
		t.Fatalf("session id survived a restart: %q", restarted)
	}
}

// TestCanonicalSessionIDKeepsOfficialID verifies that a client-minted official
// session is never re-salted, since its affinity belongs to the client.
func TestCanonicalSessionIDKeepsOfficialID(t *testing.T) {
	official := "ses_012345678fc0ABCDEFGHIJKLMN"
	if got := CanonicalSessionID(official); got != official {
		t.Fatalf("official session rewritten: %q, want %q", got, official)
	}
}
