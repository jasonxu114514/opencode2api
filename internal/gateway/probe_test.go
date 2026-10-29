package gateway

import (
	"testing"

	"opencode2api/internal/identity"
)

// TestProbeRequestIDsAreFresh pins the identity a probe presents upstream. A
// pinned project or session is one quota bucket shared with every deployment
// that runs the same probe, so a charged bucket on a popular exit made every
// probe 429 while live traffic on the same process and egress succeeded.
func TestProbeRequestIDsAreFresh(t *testing.T) {
	first := probeRequestIDs()
	second := probeRequestIDs()

	if first.Session == second.Session {
		t.Errorf("two probes reused session %q", first.Session)
	}
	if first.Project == second.Project {
		t.Errorf("two probes reused project %q", first.Project)
	}
	for _, ids := range []identity.RequestIDs{first, second} {
		if ids.Session == "" || ids.Project == "" {
			t.Fatalf("incomplete identity: %+v", ids)
		}
		// CanonicalSessionID leaves an official session untouched, so an
		// idempotent round trip means the probe already sends the shape the
		// free tier accepts instead of relying on the gateway to re-encode it.
		if reencoded := identity.CanonicalSessionID(ids.Session); reencoded != ids.Session {
			t.Errorf("probe session %q is not canonical (re-encoded to %q)", ids.Session, reencoded)
		}
		if len(ids.Project) != 40 {
			t.Errorf("probe project %q is not a 40-character id", ids.Project)
		}
	}
}
