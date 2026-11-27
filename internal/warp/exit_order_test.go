package warp

import (
	"testing"
	"time"
)

func TestEndpointBlockGroupsPublishedPeers(t *testing.T) {
	cases := map[string]string{
		"[2606:4700:d0::a29f:c001]:2408": "2606:4700:d0::/64",
		"[2606:4700:d0::a29f:c002]:500":  "2606:4700:d0::/64",
		"162.159.192.1:2408":             "162.159.192.0/24",
		"162.159.192.7:1701":             "162.159.192.0/24",
		"[2001:db8:1:2::1]:2408":         "2001:db8:1:2::/64",
	}
	for endpoint, want := range cases {
		if got := endpointBlock(endpoint); got != want {
			t.Errorf("endpointBlock(%q) = %q, want %q", endpoint, got, want)
		}
	}
}

func TestInterleaveByBlockTriesEachBlockEarly(t *testing.T) {
	endpoints := []string{
		"[2606:4700:d0::a29f:c007]:2408",
		"[2606:4700:d0::a29f:c001]:2408",
		"[2606:4700:d0::a29f:c007]:500",
		"[2606:4700:d0::a29f:c001]:500",
		"162.159.192.7:2408",
		"162.159.192.1:2408",
		"162.159.192.7:500",
	}
	got := interleaveByBlock(endpoints)
	if len(got) != len(endpoints) {
		t.Fatalf("interleave dropped candidates: got %d, want %d", len(got), len(endpoints))
	}
	// The first IPv4 candidate must appear in the first round, before the second
	// IPv6 candidate: a plain walk spends a timeout per IPv6 candidate first.
	if got[1] != "162.159.192.7:2408" {
		t.Fatalf("second candidate = %q, want the first IPv4 candidate", got[1])
	}
	seen := make(map[string]bool, len(endpoints))
	for _, endpoint := range got {
		if seen[endpoint] {
			t.Fatalf("duplicate candidate %q in %v", endpoint, got)
		}
		seen[endpoint] = true
	}
}

func resetEndpointMemory() {
	endpointMemory.Lock()
	endpointMemory.lastGood = ""
	endpointMemory.dead = make(map[string]time.Time)
	endpointMemory.Unlock()
}

func TestOrderEndpointsByMemoryPrefersLastGoodBlock(t *testing.T) {
	resetEndpointMemory()
	defer resetEndpointMemory()

	rememberEndpoint("162.159.192.4:500")
	endpoints := []string{
		"[2606:4700:d0::a29f:c007]:2408",
		"[2606:4700:d0::a29f:c001]:2408",
		"162.159.192.9:2408",
		"162.159.192.3:500",
	}
	got := orderEndpointsByMemory(endpoints)
	if endpointBlock(got[0]) != "162.159.192.0/24" || endpointBlock(got[1]) != "162.159.192.0/24" {
		t.Fatalf("last good block was not moved first: %v", got)
	}
	if len(got) != len(endpoints) {
		t.Fatalf("ordering dropped candidates: %v", got)
	}
}

func TestOrderEndpointsByMemoryDemotesDeadBlocks(t *testing.T) {
	resetEndpointMemory()
	defer resetEndpointMemory()

	forgetBlock(endpointBlock("[2606:4700:d0::a29f:c001]:2408"))
	endpoints := []string{
		"[2606:4700:d0::a29f:c001]:2408",
		"[2606:4700:d0::a29f:c007]:2408",
		"162.159.192.1:2408",
	}
	got := orderEndpointsByMemory(endpoints)
	if endpointBlock(got[0]) != "162.159.192.0/24" {
		t.Fatalf("unreachable block was not demoted: %v", got)
	}
	if !endpointBlockDead(endpointBlock("[2606:4700:d0::a29f:c007]:2408")) {
		t.Fatal("dead block should report as dead for the whole /64")
	}
}

func TestEndpointBlockExpires(t *testing.T) {
	resetEndpointMemory()
	defer resetEndpointMemory()

	block := endpointBlock("162.159.192.1:500")
	forgetBlock(block)
	if !endpointBlockDead(block) {
		t.Fatal("block should be dead right after a failed sweep")
	}
	endpointMemory.Lock()
	endpointMemory.dead[block] = time.Now().Add(-time.Second)
	endpointMemory.Unlock()
	if endpointBlockDead(block) {
		t.Fatal("block should stop being dead once its TTL passed")
	}
}
