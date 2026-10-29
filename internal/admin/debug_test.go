package admin

import (
	"testing"

	"opencode2api/internal/identity"
)

// TestBenchSessionIsFresh pins the session the Playground bench sends upstream.
// The bench replays one prompt per click, so a session derived from the prompt
// would reuse a single bucket until it is charged and every later click would
// answer 429 while live traffic succeeded.
func TestBenchSessionIsFresh(t *testing.T) {
	first := benchSession()
	second := benchSession()

	if first == "" {
		t.Fatal("empty session")
	}
	if first == second {
		t.Errorf("two bench requests reused session %q", first)
	}
	for _, session := range []string{first, second} {
		if reencoded := identity.CanonicalSessionID(session); reencoded != session {
			t.Errorf("bench session %q is not canonical (re-encoded to %q)", session, reencoded)
		}
	}
}
