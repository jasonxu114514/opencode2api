// Package identity derives stable session identities.
//
// The 2026-10-14 Zen free-tier gate validates a descending-timestamp session
// id plus a client User-Agent, and the /inference gate 403s requests that
// carry the v1 lane's x-opencode-request header. The identity that matters
// upstream is therefore the pair (x-opencode-session, x-opencode-session-id):
// Session is the conversation-affinity key and must keep the descending layout
// minted by CanonicalSessionID; ChildSession preserves the client's own
// x-opencode-session-id when it differs, as it does for a subagent.
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
	"time"

	"opencode2api/internal/jsonutil"
)

// RequestIDs carries the identity headers the upstream free tier inspects.
// Session is the conversation-affinity key sent as x-opencode-session; the
// v2 lane sends ChildSession, the client's own x-opencode-session-id, as
// x-opencode-session-id so a subagent keeps its child id while affinity stays
// on the parent. The v1 lane sends Session as both headers. Message is the
// user message id the v1 lane receives as x-opencode-request; the v2 lane
// rejects that header. Request only identifies the request inside the
// gateway. Project is rendered per lane by the gateway.
type RequestIDs struct {
	Session       string
	ChildSession  string
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
	childSession := r.Header.Get("x-opencode-session-id")
	if childSession != "" {
		childSession = scopedSessionID(scope, childSession)
	}
	projectSignal := jsonutil.FirstString(r.Header.Get("x-opencode-project"), jsonutil.StringAt(body, "metadata", "project_id"))
	if projectSignal == "" {
		// A fresh project id per request. The free tier ledges its quota per
		// project id and egress, so one shared id is one shared bucket: the old
		// per-caller default arrived pre-burned on popular exits. A client that
		// names its project keeps that name.
		projectSignal = RandomProjectID()
	}
	parentSession := jsonutil.FirstString(
		r.Header.Get("x-parent-session-id"),
		jsonutil.StringAt(body, "metadata", "parent_session_id"),
	)
	if parentSession != "" {
		parentSession = scopedSessionID(scope, parentSession)
	}
	return RequestIDs{
		Session:       session,
		ChildSession:  childSession,
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

// bootNonce separates the sessions one gateway process derives from those its
// earlier runs derived. It is minted once per process and mixed into every
// synthesized session, so restarting the daemon re-mints the upstream session
// ids for the same conversations, while a single run keeps one stable id per
// conversation. Official client sessions bypass it and stay verbatim.
var bootNonce = mintBootNonce()

// mintBootNonce returns the per-process session salt. It panics when the
// system random source fails.
func mintBootNonce() []byte {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return buf
}

// CanonicalSessionID returns signal unchanged when it already carries an
// official OpenCode session minted by the client's Identifier.descending()
// (preserving upstream prompt-cache affinity). Any other downstream identity
// (UUIDs, foreign client sessions, legacy gateway sessions, conversation
// seeds) is re-encoded into that exact layout — inverted 48-bit millisecond
// timestamp with counter 1, so the low 12 hex digits are ffe — keeping one
// stable session per conversation for the life of the process. The upstream
// free tier also ties a session id to its risk/quota bookkeeping, so the
// re-encoding is salted with bootNonce: a restart yields fresh ids instead of
// resuming whatever the previous run's exits were charged for. A re-encoded
// session passes the free tier's encoding check where the raw hash shape was
// rejected with 403 FreeTierError.
//
// The timestamp must also stay plausible. The free tier derives a risk window
// from it: a session dated more than about a year from now, in either
// direction, is answered with HTTP 429 "Free usage limit reached" — the same
// message a genuinely exhausted IP receives — so a hash-derived timestamp
// (which landed in 1970) made every free request fail however many WARP exits
// were rotated. The encoding below therefore draws the timestamp from a slow
// clock instead of the hash: today's UTC midnight minus a hash-derived offset
// of at most sessionTimestampWindow. The id stays stable for a whole day, which
// is all the affinity a live conversation needs, and its date never leaves the
// window the upstream accepts.
func CanonicalSessionID(signal string) string {
	if isOfficialSessionID(signal) {
		return signal
	}
	hasher := sha256.New()
	hasher.Write([]byte("ses\x00"))
	hasher.Write(bootNonce)
	hasher.Write([]byte{0})
	hasher.Write([]byte(signal))
	sum := hasher.Sum(nil)
	timePart := descendingTimePart(time.Now(), sum[:6])
	randomPart := base62Fixed(new(big.Int).SetBytes(sum[6:16]), 14)
	return "ses_" + timePart + randomPart
}

// isOfficialSessionID reports whether signal is a session minted by the
// official client's Identifier.descending(). A value with the right ses_ shape
// but a foreign encoding (raw hash or ascending ID) is not official.
func isOfficialSessionID(signal string) bool {
	return canonicalSessionPattern.MatchString(signal) && descendingTimePartPattern.MatchString(signal[4:16])
}

// sessionTimestampWindow bounds how far before today a synthesized session's
// timestamp may fall. The upstream accepts a date roughly a year away in either
// direction and rejects anything beyond with HTTP 429, so a month leaves room
// for clock skew and long-lived processes while never approaching the edge.
const sessionTimestampWindow = 30 * 24 * time.Hour

// descendingTimePart renders a plausible recent timestamp in the official
// client's Identifier.descending() layout: a 36-bit timestamp shifted into the
// top of a 48-bit word, ORed with counter 1, then bitwise-NOT-ed so the low 12
// hex digits read ffe. The hash only picks the offset within
// sessionTimestampWindow; it never supplies the date itself.
func descendingTimePart(now time.Time, hash []byte) string {
	window := uint64(sessionTimestampWindow / time.Millisecond)
	offset := (uint64(hash[0])<<24 | uint64(hash[1])<<16 | uint64(hash[2])<<8 | uint64(hash[3])) % window
	base := now.UTC().Truncate(24 * time.Hour).Add(-time.Duration(offset) * time.Millisecond)
	current := uint64(base.UnixMilli())<<12 | 1
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

// RandomProjectID returns a project id in the shape the official CLI sends: the
// 40-character hex of a git root commit. It is minted per request when the
// client names no project, because the free tier ledges quota per project id
// and egress together. It panics when the system random source fails.
func RandomProjectID() string {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(buf)
}
