package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"opencode2api/internal/jsonutil"
)

// messageIDPattern matches a user message id minted by the official client's
// Identifier.ascending(): "msg_" + 12 hex time characters + 14 Base62.
var messageIDPattern = regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// userMessages pins one user message id to each user turn of a session.
var userMessages = &messageStore{turns: map[string]messageEntry{}, latest: map[string]messageEntry{}}

// UserMessageID returns the user message id the v1 lane receives as
// x-opencode-request, which is the id the OpenCode 1.x client reports there.
// The client mints it when the user sends a message and repeats it on every
// request of that turn (the agent loop's tool steps and the title request), so
// the gateway keeps one id per user turn of a session rather than one per
// request. A genuine id from an official client is kept verbatim.
func UserMessageID(r *http.Request, body map[string]any, session string) string {
	if id := r.Header.Get("x-opencode-request"); messageIDPattern.MatchString(id) {
		return id
	}
	return userMessages.idFor(session, userTurn(body), time.Now())
}

// userTurn fingerprints the latest turn the user typed: how many user turns the
// conversation holds and what the last one said. Tool results travel as user
// messages on the Anthropic wire, so a user message carrying one belongs to the
// agent loop, not to a new turn. An empty fingerprint means the request
// continues a turn it does not repeat (a stateful Responses continuation).
func userTurn(body map[string]any) string {
	if input, ok := body["input"].(string); ok && input != "" {
		return turnFingerprint(1, input)
	}
	count := 0
	var last any
	for _, field := range []string{"messages", "input"} {
		for _, raw := range jsonutil.SliceAt(body, field) {
			item, ok := raw.(map[string]any)
			if !ok || jsonutil.StringAt(item, "role") != "user" || carriesToolResult(item["content"]) {
				continue
			}
			count++
			last = item["content"]
		}
	}
	if count == 0 {
		return ""
	}
	return turnFingerprint(count, last)
}

// turnFingerprint identifies a user turn by its position among the user turns
// of the conversation and the hash of its content.
func turnFingerprint(count int, content any) string {
	encoded, _ := json.Marshal(content)
	sum := sha256.Sum256(encoded)
	return strconv.Itoa(count) + ":" + hex.EncodeToString(sum[:8])
}

// carriesToolResult reports whether content is a block list holding a
// tool_result block.
func carriesToolResult(content any) bool {
	blocks, ok := content.([]any)
	if !ok {
		return false
	}
	for _, raw := range blocks {
		if block, ok := raw.(map[string]any); ok && jsonutil.StringAt(block, "type") == "tool_result" {
			return true
		}
	}
	return false
}

// messageTTL bounds how long an idle turn keeps its id, and messageStoreLimit
// how many turns are remembered before idle ones are dropped.
const (
	messageTTL        = 6 * time.Hour
	messageStoreLimit = 8192
)

type messageEntry struct {
	id   string
	seen time.Time
}

// messageStore remembers the id of every recent user turn, keyed by session
// and turn so interleaved conversations of one session (a client's title
// request beside its agent loop) each keep their own id, plus the latest id of
// each session for requests that carry no user turn of their own.
type messageStore struct {
	mu     sync.Mutex
	turns  map[string]messageEntry
	latest map[string]messageEntry
}

// idFor returns the id pinned to the turn of session, minting one when the
// turn is unknown or idle for longer than messageTTL. An empty turn reuses the
// session's latest id and never pins a new turn.
func (s *messageStore) idFor(session, turn string, now time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := session + "\x00" + turn
	entry, ok := s.turns[key]
	if turn == "" {
		entry, ok = s.latest[session]
	}
	if !ok || now.Sub(entry.seen) > messageTTL {
		entry = messageEntry{id: AscendingID("msg", now)}
	}
	entry.seen = now
	if turn != "" {
		s.turns[key] = entry
	}
	s.latest[session] = entry
	if len(s.turns) > messageStoreLimit || len(s.latest) > messageStoreLimit {
		s.prune(now)
	}
	return entry.id
}

// prune drops idle turns, then arbitrary ones if every turn is still live,
// until each map is down to three quarters of messageStoreLimit.
func (s *messageStore) prune(now time.Time) {
	for _, entries := range []map[string]messageEntry{s.turns, s.latest} {
		for key, entry := range entries {
			if now.Sub(entry.seen) > messageTTL {
				delete(entries, key)
			}
		}
		for key := range entries {
			if len(entries) <= messageStoreLimit*3/4 {
				break
			}
			delete(entries, key)
		}
	}
}

// ascendingClock mirrors the official client's Identifier, whose counter
// restarts every millisecond and is shared by every id it mints.
var ascendingClock struct {
	sync.Mutex
	last    int64
	counter int64
}

// AscendingID mints an id in the official client's Identifier.ascending()
// layout: prefix + "_" + the low 48 bits of (ms<<12 | counter) as 12 hex
// characters + 14 random Base62 characters.
func AscendingID(prefix string, now time.Time) string {
	ms := now.UnixMilli()
	ascendingClock.Lock()
	if ms != ascendingClock.last {
		ascendingClock.last, ascendingClock.counter = ms, 0
	}
	ascendingClock.counter++
	value := uint64(ms)<<12 | uint64(ascendingClock.counter)
	ascendingClock.Unlock()
	timePart := hex.EncodeToString([]byte{
		byte(value >> 40), byte(value >> 32), byte(value >> 24),
		byte(value >> 16), byte(value >> 8), byte(value),
	})
	return prefix + "_" + timePart + randomBase62(14)
}

// randomBase62 returns length random Base62 characters. It panics when the
// system random source fails.
func randomBase62(length int) string {
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	for i, b := range buf {
		buf[i] = base62Alphabet[int(b)%62]
	}
	return string(buf)
}
