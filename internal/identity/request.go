// Package identity derives stable session identities.
//
// The 2026-10-14 Zen free-tier gate validates a descending-timestamp session
// id plus a client User-Agent, and it 403s requests that still carry the
// legacy x-opencode-request header. The identity that matters upstream is
// therefore the pair (x-opencode-session, x-opencode-session-id): Session is
// the conversation-affinity key and must keep the descending layout minted by
// CanonicalSessionID.
package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"regexp"

	"opencode2api/internal/jsonutil"
)

// RequestIDs carries the identity headers the upstream free tier inspects.
// Session is sent as both x-opencode-session and x-opencode-session-id; both
// headers are required since the 2026-10-14 gate. Request is retained for
// request-level tracing of this gateway only — it must NOT reach upstream.
type RequestIDs struct {
	Session       string
	Request       string
	Project       string
	ParentSession string
}

func DeriveRequestIDs(r *http.Request, body map[string]any) RequestIDs {
	signal := jsonutil.FirstString(
		r.Header.Get("x-opencode-session"),
		r.Header.Get("x-opencode-session-id"),
		r.Header.Get("x-session-affinity"),
		r.Header.Get("X-Session-Id"),
		r.Header.Get("x-session-id"),
		r.Header.Get("conversation-id"),
		jsonutil.StringAt(body, "conversation_id"),
		jsonutil.StringAt(body, "metadata", "session_id"),
	)
	if signal == "" {
		// Using the first user turn keeps a multi-turn conversation stable as its
		// history grows while separating conversations with different beginnings.
		signal = conversationSeed(body)
	}
	if signal == "" {
		signal = jsonutil.StringAt(body, "previous_response_id")
	}
	if signal == "" || signal == `{}` {
		signal = RandomID("fallback", 16)
	}
	session := CanonicalSessionID(signal)
	// The upstream free tier 403s canonical prj_* project ids (2026-10-14
	// probing) while accepting the literal "global" every official client
	// sends. Anything non-canonical also passes, so the project header is only
	// used as a local correlation hint and is no longer forwarded upstream.
	projectSignal := jsonutil.FirstString(r.Header.Get("x-opencode-project"), jsonutil.StringAt(body, "metadata", "project_id"))
	if projectSignal == "" {
		projectSignal = "opencode2api:default-project"
	}
	parentSession := jsonutil.FirstString(
		r.Header.Get("x-parent-session-id"),
		jsonutil.StringAt(body, "metadata", "parent_session_id"),
	)
	return RequestIDs{
		Session:       session,
		Request:       RandomID("req", 16),
		Project:       projectSignal,
		ParentSession: parentSession,
	}
}

func conversationSeed(body map[string]any) string {
	if input, ok := body["input"].(string); ok && input != "" {
		return input
	}
	for _, field := range []string{"messages", "input"} {
		for _, raw := range jsonutil.SliceAt(body, field) {
			item, ok := raw.(map[string]any)
			if !ok || jsonutil.StringAt(item, "role") != "user" {
				continue
			}
			encoded, _ := json.Marshal(item["content"])
			if len(encoded) > 0 && string(encoded) != "null" {
				return string(encoded)
			}
		}
	}
	return ""
}

func StableID(prefix, value string) string {
	sum := sha256.Sum256([]byte(prefix + "\x00" + value))
	return prefix + "_" + hex.EncodeToString(sum[:12])
}

// canonicalSessionPattern matches OpenCode's canonical session format:
// "ses_" + 12 lowercase hex timestamp characters + 14 Base62 characters.
// Since 2026-09-16 the Zen free tier rejects any other session shape with
// 403 FreeTierError.
var canonicalSessionPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// descendingTimePartPattern matches the time part the official client mints
// with Identifier.descending(): the 48-bit value ~(ts_ms<<12|counter) in hex.
// Bitwise NOT sets the low 12 bits, so a genuine time part ends in
// 0xFFF - counter. Probing the live endpoint shows the free tier accepts
// counters 1-63 (low 12 bits 0xFC0-0xFFF) and rejects everything beyond with
// 403 FreeTierError even when the ses_ shape is right, so the pattern accepts
// exactly that window.
var descendingTimePartPattern = regexp.MustCompile(`^[0-9a-f]{9}f[c-f][0-9a-f]$`)

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// CanonicalSessionID returns signal unchanged when it already carries an
// official OpenCode session minted by the client's Identifier.descending()
// (preserving upstream prompt-cache affinity). Any other downstream identity
// (UUIDs, foreign client sessions, legacy gateway sessions, conversation
// seeds) is deterministically re-encoded into that exact layout — inverted
// 48-bit millisecond timestamp with counter 1, so the low 12 hex digits are
// ffe — while keeping the same stable session per conversation. A re-encoded
// session passes the free tier's encoding check where the raw hash shape was
// rejected with 403 FreeTierError.
func CanonicalSessionID(signal string) string {
	if canonicalSessionPattern.MatchString(signal) {
		if descendingTimePartPattern.MatchString(signal[4:16]) {
			return signal
		}
		// Right shape but a foreign encoding (raw hash or ascending ID):
		// fall through and re-encode deterministically.
	}
	sum := sha256.Sum256([]byte("ses\x00" + signal))
	timePart := descendingTimePart(sum[:6])
	randomPart := base62Fixed(new(big.Int).SetBytes(sum[6:16]), 14)
	return "ses_" + timePart + randomPart
}

// descendingTimePart renders six hash bytes as the official client's
// Identifier.descending() layout: a 36-bit timestamp shifted into the top of
// a 48-bit word, ORed with counter 1, then bitwise-NOT-ed so the low 12 hex
// digits read ffe.
func descendingTimePart(hash []byte) string {
	current := (uint64(hash[0])<<28 | uint64(hash[1])<<20 | uint64(hash[2])<<12 |
		uint64(hash[3])<<4 | uint64(hash[4])>>4) << 12
	current |= 1
	inverted := ^current & 0xFFFFFFFFFFFF
	return hex.EncodeToString([]byte{
		byte(inverted >> 40), byte(inverted >> 32), byte(inverted >> 24),
		byte(inverted >> 16), byte(inverted >> 8), byte(inverted),
	})
}

func base62Fixed(n *big.Int, width int) string {
	base := big.NewInt(62)
	out := make([]byte, width)
	remainder := new(big.Int)
	for i := width - 1; i >= 0; i-- {
		n.DivMod(n, base, remainder)
		out[i] = base62Alphabet[remainder.Int64()]
	}
	return string(out)
}

func RandomID(prefix string, size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return prefix + "_" + hex.EncodeToString(buf)
}
