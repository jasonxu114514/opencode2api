// Package freequota counts accepted free-model requests per egress for the
// current UTC day and records a per-egress allowance floor.
//
// The upstream exposes no remaining-quota endpoint and no rate-limit headers.
// Two facts are observable: N accepted requests prove the allowance is at
// least N, and a genuine daily-limit refusal proves usage reached it. Shape
// refusals share the free-limit body, so a refusal teaches nothing unless its
// Retry-After points at the next UTC midnight; short throttles and refusals
// without that hint teach nothing either.
package freequota

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// saveInterval bounds how often counters reach disk. The snapshot served to
	// operators is read from memory and is always current.
	saveInterval = 2 * time.Second
	// minRetryAfter separates a daily reset from a transient throttle.
	minRetryAfter = time.Minute
	// midnightSkew bounds how close a Retry-After must land to the next UTC
	// midnight to count as a daily-quota refusal.
	midnightSkew = 5 * time.Minute
)

// Options configures the estimate. AssumedDailyLimit is the per-egress
// allowance shown until an egress teaches its own; zero shows unknown.
type Options struct{ AssumedDailyLimit int }

// Event is one finished upstream attempt for a free model. Body is only read
// for a 429 and must already be restored by the caller.
type Event struct {
	Time           time.Time
	Egress         string
	Status         int
	Body           []byte
	RetryAfter     time.Duration
	TransportError bool
}

// Scope is one egress's estimate for the current day.
type Scope struct {
	Egress      string `json:"egress"`
	Used        int    `json:"used"`
	Limit       int    `json:"limit"`
	LimitSource string `json:"limit_source"`
	Remaining   *int   `json:"remaining"`
	Exhausted   bool   `json:"exhausted"`
}

// Snapshot is the whole estimate for the current UTC day.
type Snapshot struct {
	ResetsAt          time.Time `json:"resets_at"`
	AssumedDailyLimit int       `json:"assumed_daily_limit"`
	TotalUsed         int       `json:"total_used"`
	Scopes            []Scope   `json:"scopes"`
}

type scopeState struct {
	Day       string `json:"day,omitempty"`
	Used      int    `json:"used,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Exhausted bool   `json:"exhausted,omitempty"`
}

type persisted struct {
	Scopes map[string]*scopeState `json:"scopes"`
}

// Tracker keeps per-egress counters for the current day. It is safe for
// concurrent use. An empty path makes the state memory-only.
type Tracker struct {
	mu       sync.Mutex
	saveMu   sync.Mutex
	opts     Options
	path     string
	scopes   map[string]*scopeState
	now      func() time.Time
	lastSave time.Time
}

// Open loads path. A missing or corrupt file yields an empty tracker: the file
// only holds an estimate, so it is never fatal.
func Open(path string, opts Options) *Tracker {
	t := &Tracker{
		opts:   opts,
		path:   strings.TrimSpace(path),
		scopes: make(map[string]*scopeState),
		now:    time.Now,
	}
	t.load()
	return t
}

// Configure updates the runtime options without discarding today's counts.
func (t *Tracker) Configure(opts Options) {
	t.mu.Lock()
	t.opts = opts
	t.mu.Unlock()
}

// Close flushes pending counters.
func (t *Tracker) Close() {
	if t.path == "" {
		return
	}
	t.save()
}

// Record accounts one attempt. Only accepted requests count as usage, and only
// a daily-quota refusal that follows usage on the same egress bounds its
// allowance; transport errors, blocks and other statuses spend nothing.
func (t *Tracker) Record(ev Event) {
	when := ev.Time
	if when.IsZero() {
		when = t.now()
	}
	when = when.UTC()
	day := when.Format("2006-01-02")
	egress := strings.TrimSpace(ev.Egress)
	if egress == "" {
		egress = "direct"
	}

	t.mu.Lock()
	scope := t.scopes[egress]
	if scope == nil {
		scope = &scopeState{}
		t.scopes[egress] = scope
	}
	if day < scope.Day {
		t.mu.Unlock()
		return
	}
	if day > scope.Day {
		scope.Day = day
		scope.Used = 0
		scope.Exhausted = false
	}

	switch {
	case ev.TransportError, ev.Status == http.StatusForbidden:
		// The attempt never reached a model: no usage, no evidence.
	case isSuccess(ev.Status):
		scope.Used++
		scope.Exhausted = false
	case isDailyLimit(ev.Status, ev.Body, ev.RetryAfter, when):
		// A refusal proves exhaustion only when the egress has already accepted
		// as much as its allowance. Below that the refusal can be scoped to one
		// conversation rather than this egress, so it teaches nothing.
		if scope.Used > 0 && (t.opts.AssumedDailyLimit <= 0 || scope.Used >= t.opts.AssumedDailyLimit) {
			if scope.Used > scope.Limit {
				scope.Limit = scope.Used
			}
			scope.Exhausted = true
		}
	}

	due := when.Sub(t.lastSave) >= saveInterval
	if due {
		t.lastSave = when
	}
	t.mu.Unlock()
	if due {
		t.save()
	}
}

// Snapshot renders the current UTC day. Only egresses with accepted requests
// today are listed; one that has no traffic today appears after its first.
func (t *Tracker) Snapshot() Snapshot {
	now := t.now().UTC()
	day := now.Format("2006-01-02")
	t.mu.Lock()
	defer t.mu.Unlock()
	result := Snapshot{
		ResetsAt:          nextReset(now),
		AssumedDailyLimit: t.opts.AssumedDailyLimit,
		Scopes:            []Scope{},
	}
	for egress, state := range t.scopes {
		if state.Day != day || state.Used <= 0 {
			continue
		}
		item := Scope{Egress: egress, Used: state.Used, Exhausted: state.Exhausted}
		switch {
		case state.Limit > 0:
			item.Limit, item.LimitSource = state.Limit, "learned"
		case t.opts.AssumedDailyLimit > 0:
			item.Limit, item.LimitSource = t.opts.AssumedDailyLimit, "assumed"
		default:
			item.LimitSource = "unknown"
		}
		if item.Limit > 0 {
			// A limit is a floor: accepted requests alone can prove more usage
			// than an older refusal taught.
			if item.Used > item.Limit {
				item.Limit = item.Used
			}
			remaining := item.Limit - item.Used
			if item.Exhausted {
				remaining = 0
			}
			item.Remaining = &remaining
		}
		result.TotalUsed += item.Used
		result.Scopes = append(result.Scopes, item)
	}
	sort.Slice(result.Scopes, func(i, j int) bool {
		if result.Scopes[i].Used != result.Scopes[j].Used {
			return result.Scopes[i].Used > result.Scopes[j].Used
		}
		return result.Scopes[i].Egress < result.Scopes[j].Egress
	})
	return result
}

func (t *Tracker) load() {
	if t.path == "" {
		return
	}
	data, err := os.ReadFile(t.path)
	if err != nil {
		return
	}
	var saved persisted
	if err := json.Unmarshal(data, &saved); err != nil || saved.Scopes == nil {
		return
	}
	for egress, state := range saved.Scopes {
		if state != nil && strings.TrimSpace(egress) != "" {
			t.scopes[egress] = state
		}
	}
}

func (t *Tracker) save() {
	if t.path == "" {
		return
	}
	// Serialize the whole write so a slow older save cannot rename over a newer
	// one and persist stale counters.
	t.saveMu.Lock()
	defer t.saveMu.Unlock()
	t.mu.Lock()
	data, err := json.MarshalIndent(persisted{Scopes: t.scopes}, "", "  ")
	t.mu.Unlock()
	if err != nil {
		return
	}
	data = append(data, '\n')
	dir := filepath.Dir(t.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	temp, err := os.CreateTemp(dir, ".freequota-*.tmp")
	if err != nil {
		return
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err = temp.Write(data); err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		_ = os.Rename(tempPath, t.path)
	}
}

func isSuccess(status int) bool { return status >= 200 && status < 300 }

func isFreeLimit(status int, body []byte) bool {
	if status != http.StatusTooManyRequests || len(body) == 0 {
		return false
	}
	lower := bytes.ToLower(body)
	return bytes.Contains(lower, []byte("freeusagelimiterror")) ||
		bytes.Contains(lower, []byte("free usage limit"))
}

func isDailyLimit(status int, body []byte, retryAfter time.Duration, when time.Time) bool {
	if !isFreeLimit(status, body) || retryAfter < minRetryAfter {
		return false
	}
	delta := nextReset(when).Sub(when.Add(retryAfter))
	if delta < 0 {
		delta = -delta
	}
	return delta <= midnightSkew
}

func nextReset(when time.Time) time.Time {
	utc := when.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day()+1, 0, 0, 0, 0, time.UTC)
}
