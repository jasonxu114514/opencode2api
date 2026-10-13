// Package identity derives stable session identities and unique request IDs.
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
	"sync"
	"sync/atomic"
	"time"

	"opencode2api/internal/jsonutil"
)

type RequestIDs struct {
	Session       string
	Request       string
	Project       string
	ParentSession string
}

func DeriveRequestIDs(r *http.Request, body map[string]any) RequestIDs {
	signal := jsonutil.FirstString(
		r.Header.Get("x-opencode-session"),
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
		Request:       NewZenRequestID(),
		Project:       StableID("prj", projectSignal),
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
// The Zen free tier decodes the leading 12 hex characters as an inverted
// millisecond timestamp and rejects any session whose prefix is not one with
// 403 FreeTierError, so the timestamp half must be minted, not hashed.
var canonicalSessionPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// canonicalRequestPattern mirrors the session shape for request IDs:
// "msg_" + 12 lowercase hex characters + 14 Base62 characters.
var canonicalRequestPattern = regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// sessionCacheLimit bounds the signal-to-session cache. Conversation seeds can
// be large, so entries are keyed by digest and the whole map is dropped when it
// fills, which costs affinity for the oldest conversations only.
const sessionCacheLimit = 8192

var (
	sessionCacheMu sync.Mutex
	sessionCache   = make(map[string]string)
	sessionCounter atomic.Uint64
)

// CanonicalSessionID returns signal unchanged when it already carries an
// official OpenCode session (preserving upstream prompt-cache affinity).
// Any other downstream identity (UUIDs, foreign client sessions, legacy
// gateway sessions, conversation seeds) gets a stable minted session so the
// same conversation keeps its key/proxy affinity across turns.
func CanonicalSessionID(signal string) string {
	if canonicalSessionPattern.MatchString(signal) {
		return signal
	}
	sum := sha256.Sum256([]byte("ses\x00" + signal))
	key := string(sum[:])
	sessionCacheMu.Lock()
	if id, ok := sessionCache[key]; ok {
		sessionCacheMu.Unlock()
		return id
	}
	sessionCacheMu.Unlock()
	randomPart := base62Fixed(new(big.Int).SetBytes(sum[6:16]), 14)
	id := "ses_" + mintedTimePrefix() + randomPart
	sessionCacheMu.Lock()
	defer sessionCacheMu.Unlock()
	if len(sessionCache) >= sessionCacheLimit {
		sessionCache = make(map[string]string)
	}
	// A concurrent first mint for the same signal may have stored an ID
	// while this one was being minted; converge on the stored value so
	// every caller for one conversation shares a single upstream session.
	if existing, ok := sessionCache[key]; ok {
		return existing
	}
	sessionCache[key] = id
	return id
}

// mintedTimePrefix renders the 12 hex characters OpenCode derives from
// ~(unix_ms*0x1000+counter) truncated to its low 48 bits, mirroring the
// descending ID scheme in the OpenCode client.
func mintedTimePrefix() string {
	counter := sessionCounter.Add(1) & 0xfff
	current := uint64(time.Now().UnixMilli())*0x1000 + counter
	return fmt.Sprintf("%012x", ^current&0xFFFFFFFFFFFF)
}

// NewZenRequestID mints an x-opencode-request value in the official client's
// ascending-timestamp shape ("msg_" + ascending 12 hex + 14 Base62), one per
// upstream request. The OpenCode client encodes request time as
// unix_ms*0x1000+counter without applying the session's bitwise inversion.
// Zen currently validates only the session's timestamp, but the request
// header is the next plausible checkpoint, and sending the canonical shape
// costs nothing while keeping the gateway indistinguishable from the CLI.
func NewZenRequestID() string {
	counter := sessionCounter.Add(1) & 0xfff
	current := uint64(time.Now().UnixMilli())*0x1000 + counter
	buf := make([]byte, 14)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	randomPart := base62Fixed(new(big.Int).SetBytes(buf), 14)
	return "msg_" + fmt.Sprintf("%012x", current&0xFFFFFFFFFFFF) + randomPart
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
