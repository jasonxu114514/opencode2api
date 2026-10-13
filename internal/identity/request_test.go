package identity

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// decodeSessionTime inverts a minted "ses_" time prefix back to the
// millisecond timestamp the OpenCode client encoded, mirroring the check
// upstream Zen applies to the free tier. It returns the decoded ms value.
func decodeSessionTime(t *testing.T, id string) uint64 {
	t.Helper()
	if !canonicalSessionPattern.MatchString(id) {
		t.Fatalf("session id does not match canonical shape: %q", id)
	}
	raw, err := strconv.ParseUint(id[4:16], 16, 64)
	if err != nil {
		t.Fatalf("time prefix is not hex: %v", err)
	}
	decoded := ^raw & 0xFFFFFFFFFFFF
	return decoded >> 12
}

// decodeRequestTime decodes a minted "msg_" prefix, which the official
// client writes ascending (unix_ms*0x1000+counter, no inversion).
func decodeRequestTime(t *testing.T, id string) uint64 {
	t.Helper()
	if !canonicalRequestPattern.MatchString(id) {
		t.Fatalf("request id does not match canonical shape: %q", id)
	}
	raw, err := strconv.ParseUint(id[4:16], 16, 64)
	if err != nil {
		t.Fatalf("time prefix is not hex: %v", err)
	}
	return (raw & 0xFFFFFFFFFFFF) >> 12
}

// truncatedMS returns the low 36 bits of now, the only part recoverable from
// a 48-bit encoded prefix (the encoding drops the top bits of unix_ms).
func truncatedMS(now time.Time) uint64 {
	return uint64(now.UnixMilli()) & 0xFFFFFFFFF
}

func TestCanonicalSessionIDPassthrough(t *testing.T) {
	// Genuine OpenCode CLI sessions must pass through untouched so upstream
	// prompt-cache affinity is preserved.
	valid := "ses_ecc985076ffe3FWJqsz05vQbUW"
	if got := CanonicalSessionID(valid); got != valid {
		t.Fatalf("canonical session was rewritten: got %q", got)
	}
}

func TestCanonicalSessionIDMintedTimestamp(t *testing.T) {
	before := truncatedMS(time.Now())
	id := CanonicalSessionID("some-downstream-conversation-seed")
	after := truncatedMS(time.Now())

	decoded := decodeSessionTime(t, id)
	// Decoding inverts the prefix once more, which restores ms mod 2^36
	// (the double negation cancels), so it must fall inside the window the
	// ID was minted in. A digest-derived prefix would land on a random
	// value instead.
	if decoded < before || decoded > after {
		t.Fatalf("decoded ms %d outside mint window [%d, %d] for %q", decoded, before, after, id)
	}
}

func TestCanonicalSessionIDStability(t *testing.T) {
	signal := "downstream-session-uuid-1234"
	first := CanonicalSessionID(signal)
	for i := 0; i < 5; i++ {
		if got := CanonicalSessionID(signal); got != first {
			t.Fatalf("same signal produced different sessions: %q vs %q", first, got)
		}
	}
	// The tail stays derived from the signal digest, so it is stable even
	// across cache evictions; only the minted time half changes.
	if other := CanonicalSessionID("a-different-conversation"); other == first {
		t.Fatalf("different signals collided: %q", first)
	}
}

func TestCanonicalSessionIDConcurrency(t *testing.T) {
	// Run with -race: the digest-keyed cache must be safe under parallel use.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			signal := fmt.Sprintf("concurrent-signal-%d", i%3)
			first := CanonicalSessionID(signal)
			for j := 0; j < 20; j++ {
				if got := CanonicalSessionID(signal); got != first {
					t.Errorf("signal %q unstable under concurrency: %q vs %q", signal, first, got)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestNewZenRequestIDShapeAndTimestamp(t *testing.T) {
	before := truncatedMS(time.Now())
	id := NewZenRequestID()
	after := truncatedMS(time.Now())

	decoded := decodeRequestTime(t, id)
	if decoded < before || decoded > after {
		t.Fatalf("decoded request ms %d outside mint window [%d, %d] for %q", decoded, before, after, id)
	}
}

func TestNewZenRequestIDUnique(t *testing.T) {
	seen := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		id := NewZenRequestID()
		if !canonicalRequestPattern.MatchString(id) {
			t.Fatalf("request id breaks canonical shape: %q", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate request id minted: %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestCanonicalShapesDocumented(t *testing.T) {
	// Pin the exact wire shapes the gateway promises upstream.
	if len("ses_"+strings.Repeat("f", 12)+strings.Repeat("0", 14)) != 30 {
		t.Fatal("session shape length drifted from 30 chars")
	}
	if len("msg_"+strings.Repeat("f", 12)+strings.Repeat("0", 14)) != 30 {
		t.Fatal("request shape length drifted from 30 chars")
	}
	minted := NewZenRequestID()
	if len(minted) != 30 {
		t.Fatalf("minted request id length %d, want 30: %q", len(minted), minted)
	}
	if !regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`).MatchString(minted) {
		t.Fatalf("minted request id fails shape check: %q", minted)
	}
}
