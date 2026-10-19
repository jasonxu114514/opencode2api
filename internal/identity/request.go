// Package identity derives stable session identities.
//
// The 2026-10-14 Zen free-tier gate validates a descending-timestamp session
// id plus a client User-Agent, and the /inference gate 403s requests that
// carry the v1 lane's x-opencode-request header. The identity that matters
// upstream is therefore the pair (x-opencode-session, x-opencode-session-id):
// Session is the conversation-affinity key and must keep the descending layout
// minted by CanonicalSessionID.
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
// Session is sent as both x-opencode-session and x-opencode-session-id.
// Message is the user message id the v1 lane receives as x-opencode-request;
// the v2 lane rejects that header. Request only identifies the request inside
// the gateway. Project is rendered per lane by the gateway (prj_* on the v1
// lane, bare 40-hex on the v2 lane).
type RequestIDs struct {
	Session       string
	Request       string
	Message       string
	Project       string
	ParentSession string
}

// DeriveRequestIDs derives the upstream identity of one request. scope names
// the authenticated caller (the server key fingerprint). Every session and
// project the gateway derives itself is mixed with it, so two callers whose
// conversations begin identically never share an upstream session, sticky
// provider, preferred key, or anonymous proxy. An official OpenCode session is
// kept verbatim because the client minted it uniquely.
func DeriveRequestIDs(r *http.Request, body map[string]any, scope string) RequestIDs {
	signal := jsonutil.FirstString(
		r.Header.Get("x-opencode-session"),
		r.Header.Get("x-opencode-session-id"),
		r.Header.Get("x-session-affinity"),
		r.Header.Get("x-session-id"),
		// Codex sends its conversation id in both underscore headers.
		r.Header.Get("session_id"),
		r.Header.Get("conversation-id"),
		r.Header.Get("conversation_id"),
		jsonutil.StringAt(body, "conversation_id"),
		jsonutil.StringAt(body, "metadata", "session_id"),
		claudeCodeSessionID(jsonutil.StringAt(body, "metadata", "user_id")),
	)
	if signal == "" {
		// The system prompt plus the first user turn keeps a multi-turn
		// conversation stable as its history grows while separating
		// conversations with different beginnings.
		signal = conversationSeed(body)
	}
	if signal == "" {
		signal = jsonutil.StringAt(body, "previous_response_id")
	}
	if signal == "" || signal == `{}` {
		signal = RandomID("fallback", 16)
	}
	session := scopedSessionID(scope, signal)
	projectSignal := jsonutil.FirstString(r.Header.Get("x-opencode-project"), jsonutil.StringAt(body, "metadata", "project_id"))
	if projectSignal == "" {
		// Without a client project every caller would otherwise share one
		// constant upstream project.
		projectSignal = "opencode2api:default-project"
		if scope != "" {
			projectSignal += ":" + scope
		}
	}
	parentSession := jsonutil.FirstString(
		r.Header.Get("x-parent-session-id"),
		jsonutil.StringAt(body, "metadata", "parent_session_id"),
	)
	return RequestIDs{
		Session:       session,
		Request:       RandomID("req", 16),
		Message:       UserMessageID(r, body, session),
		Project:       projectSignal,
		ParentSession: parentSession,
	}
}

// scopedSessionID keeps an official client-minted session verbatim and
// re-encodes every other signal within the caller's scope.
func scopedSessionID(scope, signal string) string {
	if isOfficialSessionID(signal) {
		return signal
	}
	if scope != "" {
		signal = scope + "\x00" + signal
	}
	return CanonicalSessionID(signal)
}

// claudeCodeSessionPattern extracts the per-session UUID Claude Code embeds in
// metadata.user_id ("user_<hash>_account_<uuid>_session_<uuid>").
var claudeCodeSessionPattern = regexp.MustCompile(`_session_([0-9A-Fa-f-]{36})$`)

// claudeCodeSessionID returns the session part of a Claude Code user id, which
// is either the underscore form above or a JSON object carrying session_id.
// The rest of the user id is per user, not per conversation, so it is never
// used as a session on its own.
func claudeCodeSessionID(userID string) string {
	if userID == "" {
		return ""
	}
	if match := claudeCodeSessionPattern.FindStringSubmatch(userID); match != nil {
		return match[1]
	}
	var value map[string]any
	if json.Unmarshal([]byte(userID), &value) == nil {
		return jsonutil.StringAt(value, "session_id")
	}
	return ""
}

// conversationSeed fingerprints how a conversation begins: its system prompt
// (Anthropic system, Responses instructions, or system/developer messages
// ahead of the first user turn) together with that first user turn. Agent
// clients often open with a boilerplate user turn such as an environment
// block, so the first user turn alone collides across unrelated users.
func conversationSeed(body map[string]any) string {
	prelude := make([]any, 0, 2)
	for _, field := range []string{"system", "instructions"} {
		if value := body[field]; value != nil {
			prelude = append(prelude, value)
		}
	}
	first, ok := firstUserTurn(body, &prelude)
	if !ok {
		return ""
	}
	encoded, err := json.Marshal(append(prelude, first))
	if err != nil {
		return ""
	}
	return string(encoded)
}

// firstUserTurn returns the content of the first user turn and appends the
// content of every system or developer message ahead of it to prelude.
func firstUserTurn(body map[string]any, prelude *[]any) (any, bool) {
	if input, ok := body["input"].(string); ok && input != "" {
		return input, true
	}
	for _, field := range []string{"messages", "input"} {
		for _, raw := range jsonutil.SliceAt(body, field) {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch jsonutil.StringAt(item, "role") {
			case "system", "developer":
				*prelude = append(*prelude, item["content"])
			case "user":
				if content := item["content"]; content != nil {
					return content, true
				}
			}
		}
	}
	return nil, false
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
	if isOfficialSessionID(signal) {
		return signal
	}
	sum := sha256.Sum256([]byte("ses\x00" + signal))
	timePart := descendingTimePart(sum[:6])
	randomPart := base62Fixed(new(big.Int).SetBytes(sum[6:16]), 14)
	return "ses_" + timePart + randomPart
}

// isOfficialSessionID reports whether signal is a session minted by the
// official client's Identifier.descending(). A value with the right ses_ shape
// but a foreign encoding (raw hash or ascending ID) is not official.
func isOfficialSessionID(signal string) bool {
	return canonicalSessionPattern.MatchString(signal) && descendingTimePartPattern.MatchString(signal[4:16])
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

// base62Fixed renders n in the Base62 alphabet, zero-padded to width
// characters. It consumes n.
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

// RandomID returns prefix + "_" + size random bytes in hex. It panics when the
// system random source fails.
func RandomID(prefix string, size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return prefix + "_" + hex.EncodeToString(buf)
}
