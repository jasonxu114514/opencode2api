// Package freequota estimates the OpenCode free-model daily allowance per
// egress from the traffic this gateway already sends. The upstream exposes no
// remaining-quota endpoint and no rate-limit headers, so every number is an
// estimate: successes raise a floor, genuine daily-quota 429s supply a ceiling,
// and the tracker learns the smaller upper bound from the days it retained.
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

// Options configures the tracker. Disabled skips recording and persistence.
// AssumedDailyLimit is the starting per-egress allowance; zero starts unknown.
type Options struct {
	Disabled          bool
	AssumedDailyLimit int
}

// Event is one finished upstream attempt for a free model. Body is only read
// for 429 and 403 responses and must already be restored by the caller.
type Event struct {
	Time           time.Time
	Egress         string
	Model          string
	Status         int
	RetryAfter     time.Duration
	Body           []byte
	TransportError bool
}

// Snapshot is the WebUI view of the current UTC day.
type Snapshot struct {
	Enabled           bool            `json:"enabled"`
	AssumedDailyLimit int             `json:"assumed_daily_limit"`
	SharedLimit       int             `json:"shared_limit"`
	Date              string          `json:"date"`
	ResetsAt          time.Time       `json:"resets_at"`
	TotalUsed         int             `json:"total_used"`
	Scopes            []ScopeSnapshot `json:"scopes"`
}

// ScopeSnapshot is one egress's estimated state for the day. Aggregate fields
// (observed_days through history) cover every retained day; the rest describe
// today only.
type ScopeSnapshot struct {
	Egress            string       `json:"egress"`
	Used              int          `json:"used"`
	Limit             int          `json:"limit"`
	LimitSource       string       `json:"limit_source"`
	Remaining         *int         `json:"remaining"`
	Confidence        string       `json:"confidence"`
	Exhausted         bool         `json:"exhausted"`
	ExhaustedUntil    *time.Time   `json:"exhausted_until"`
	LastQuotaAt       *time.Time   `json:"last_quota_at"`
	RetryAfterSeconds int          `json:"retry_after_seconds"`
	LastStatus        int          `json:"last_status"`
	LastModel         string       `json:"last_model,omitempty"`
	LastAttemptAt     *time.Time   `json:"last_attempt_at"`
	ObservedDays      int          `json:"observed_days"`
	Successes         int          `json:"successes"`
	LifetimeUsed      int          `json:"lifetime_used"`
	Exhaustions       int          `json:"exhaustions"`
	LimitFloor        int          `json:"limit_floor"`
	LimitCeil         int          `json:"limit_ceil"`
	History           []HistoryDay `json:"history"`
}

// HistoryDay is one retained day of activity for an egress scope.
type HistoryDay struct {
	Date      string `json:"date"`
	Used      int    `json:"used"`
	Successes int    `json:"successes"`
	Quota429  int    `json:"quota_429"`
	Exhausted bool   `json:"exhausted"`
}

type scopeState struct {
	Used              int        `json:"used"`
	Successes         int        `json:"successes,omitempty"`
	Ceiling           int        `json:"ceiling,omitempty"`
	Quota429          int        `json:"quota_429,omitempty"`
	RetryAfterSeconds int        `json:"retry_after_seconds,omitempty"`
	LastQuotaAt       *time.Time `json:"last_quota_at,omitempty"`
	ExhaustedUntil    *time.Time `json:"exhausted_until,omitempty"`
	// QuotaModel is the model that produced the quota-shaped 429. A later
	// success on the same model proves the 429 was transient and clears the
	// exhaustion; another model may use a separate upstream bucket.
	QuotaModel    string     `json:"quota_model,omitempty"`
	LastStatus    int        `json:"last_status,omitempty"`
	LastModel     string     `json:"last_model,omitempty"`
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`
}

type dayState struct {
	Scopes map[string]*scopeState `json:"scopes"`
}

type persisted struct {
	Version int                  `json:"version"`
	Days    map[string]*dayState `json:"days"`
}

const (
	persistVersion = 2
	keepDays       = 30
	// midnightSkew is how close a 429's Retry-After must land to the next UTC
	// midnight to count as a genuine daily-quota exhaustion.
	midnightSkew = 5 * time.Minute
)

// Tracker keeps per-day, per-egress counters. It is safe for concurrent use.
// An empty path makes the state memory-only.
type Tracker struct {
	mu     sync.Mutex
	opts   Options
	path   string
	days   map[string]*dayState
	now    func() time.Time
	saveMu sync.Mutex
	dirty  chan struct{}
	done   chan struct{}
	closed sync.Once
	wg     sync.WaitGroup
}

// Open loads path, prunes old days, and starts the asynchronous writer. A
// missing or corrupt file yields an empty tracker: the file is only an
// estimate, so it is never fatal.
func Open(path string, opts Options) *Tracker {
	t := &Tracker{
		opts: opts,
		path: strings.TrimSpace(path),
		days: make(map[string]*dayState),
		now:  time.Now,
	}
	if t.path != "" {
		t.load()
		t.dirty = make(chan struct{}, 1)
		t.done = make(chan struct{})
		t.wg.Add(1)
		go t.flushLoop()
	}
	return t
}

// Configure updates the runtime options without discarding today's counts.
func (t *Tracker) Configure(opts Options) {
	t.mu.Lock()
	t.opts = opts
	t.mu.Unlock()
}

// Close stops the writer and flushes pending state.
func (t *Tracker) Close() {
	if t.path == "" {
		return
	}
	t.closed.Do(func() { close(t.done) })
	t.save()
	t.wg.Wait()
}

func (t *Tracker) flushLoop() {
	defer t.wg.Done()
	for {
		select {
		case <-t.dirty:
			t.save()
		case <-t.done:
			t.save()
			return
		}
	}
}

// Record accounts one attempt for a free model. Provider-billable statuses
// increment the day's used count, a genuine quota 429 records the observed
// ceiling and marks the scope exhausted until the next UTC midnight, and
// transport errors or 403s only refresh the last-result fields.
func (t *Tracker) Record(ev Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.opts.Disabled {
		return
	}
	when := ev.Time
	if when.IsZero() {
		when = t.now()
	}
	when = when.UTC()
	egress := strings.TrimSpace(ev.Egress)
	if egress == "" {
		egress = "direct"
	}

	quotaRejection := isFreeLimitRejection(ev.Status, ev.Body)
	genuineQuota := quotaRejection && nearMidnight(when, ev.RetryAfter)

	day := t.day(when)
	scope := day.Scopes[egress]
	if scope == nil {
		scope = &scopeState{}
		day.Scopes[egress] = scope
	}
	lastAttempt := when
	scope.LastAttemptAt = &lastAttempt
	scope.LastModel = ev.Model
	if ev.TransportError {
		scope.LastStatus = 0
	} else {
		scope.LastStatus = ev.Status
	}

	switch {
	case ev.TransportError:
		// The attempt never reached the provider, so it spent nothing.
	case ev.Status == http.StatusForbidden:
		// A block or auth-shaped rejection is not free-tier usage.
	case genuineQuota:
		scope.Quota429++
		if scope.Ceiling == 0 {
			scope.Ceiling = scope.Used
		}
		scope.QuotaModel = ev.Model
		scope.RetryAfterSeconds = int(ev.RetryAfter / time.Second)
		lastQuota := when
		scope.LastQuotaAt = &lastQuota
		reset := nextUTCReset(when)
		scope.ExhaustedUntil = &reset
	case quotaRejection:
		// Same body without a daily-reset hint: the gate rejected the session
		// shape, not the quota.
	case billable(ev.Status):
		if isSuccess(ev.Status) {
			scope.Successes++
		}
		scope.Used++
		if isSuccess(ev.Status) && scope.ExhaustedUntil != nil && ev.Model != "" && ev.Model == scope.QuotaModel {
			scope.ExhaustedUntil = nil
			scope.LastQuotaAt = nil
			scope.RetryAfterSeconds = 0
			scope.Ceiling = 0
			scope.QuotaModel = ""
		}
	}
	t.markDirty()
}

func (t *Tracker) day(when time.Time) *dayState {
	key := when.Format("2006-01-02")
	cutoff := when.UTC().AddDate(0, 0, -keepDays).Format("2006-01-02")
	for dayKey := range t.days {
		if dayKey < cutoff {
			delete(t.days, dayKey)
		}
	}
	day := t.days[key]
	if day == nil {
		day = &dayState{Scopes: make(map[string]*scopeState)}
		t.days[key] = day
	}
	if day.Scopes == nil {
		day.Scopes = make(map[string]*scopeState)
	}
	return day
}

// Snapshot renders the current UTC day joined with the learned history of every
// retained day.
func (t *Tracker) Snapshot() Snapshot {
	now := t.now().UTC()
	t.mu.Lock()
	defer t.mu.Unlock()
	date := now.Format("2006-01-02")
	result := Snapshot{
		Enabled:           !t.opts.Disabled,
		AssumedDailyLimit: t.opts.AssumedDailyLimit,
		Date:              date,
		ResetsAt:          nextUTCReset(now),
		Scopes:            []ScopeSnapshot{},
	}

	keys := make([]string, 0, len(t.days))
	for key := range t.days {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	historyCutoff := now.AddDate(0, 0, -13).Format("2006-01-02")

	items := make(map[string]*ScopeSnapshot)
	for _, key := range keys {
		day := t.days[key]
		if day == nil {
			continue
		}
		for egress, scope := range day.Scopes {
			if scope == nil || !scopeActive(scope) {
				continue
			}
			item := items[egress]
			if item == nil {
				item = &ScopeSnapshot{Egress: egress, History: []HistoryDay{}}
				items[egress] = item
			}
			item.ObservedDays++
			item.Successes += scope.Successes
			item.LifetimeUsed += scope.Used
			if scope.Quota429 > 0 {
				item.Exhaustions++
			}
			if scope.Successes > item.LimitFloor {
				item.LimitFloor = scope.Successes
			}
			if scope.Ceiling > 0 && (item.LimitCeil == 0 || scope.Ceiling < item.LimitCeil) {
				item.LimitCeil = scope.Ceiling
			}
			if key >= historyCutoff {
				item.History = append(item.History, HistoryDay{
					Date:      key,
					Used:      scope.Used,
					Successes: scope.Successes,
					Quota429:  scope.Quota429,
					Exhausted: scope.ExhaustedUntil != nil,
				})
			}
		}
	}

	var todayScopes map[string]*scopeState
	if day := t.days[date]; day != nil {
		todayScopes = day.Scopes
	}
	// A ceiling observed on any egress is the best available bound for an
	// egress the upstream has not exhausted yet.
	shared := 0
	for _, item := range items {
		if item.LimitCeil > shared {
			shared = item.LimitCeil
		}
	}
	result.SharedLimit = shared
	for egress, item := range items {
		if scope := todayScopes[egress]; scope != nil && scopeActive(scope) {
			item.Used = scope.Used
			item.RetryAfterSeconds = scope.RetryAfterSeconds
			item.LastStatus = scope.LastStatus
			item.LastModel = scope.LastModel
			if scope.LastAttemptAt != nil {
				last := scope.LastAttemptAt.UTC()
				item.LastAttemptAt = &last
			}
			if scope.ExhaustedUntil != nil {
				until := scope.ExhaustedUntil.UTC()
				item.ExhaustedUntil = &until
				item.Exhausted = until.After(now)
			}
			if scope.LastQuotaAt != nil {
				last := scope.LastQuotaAt.UTC()
				item.LastQuotaAt = &last
			}
		}
		limit, source := learnedLimit(item, t.opts.AssumedDailyLimit, shared)
		item.Limit = limit
		item.LimitSource = source
		item.Confidence = confidence(item, source)
		if limit > 0 {
			remaining := limit - item.Used
			if remaining < 0 {
				remaining = 0
			}
			item.Remaining = &remaining
		}
	}

	for _, item := range items {
		result.TotalUsed += item.Used
		result.Scopes = append(result.Scopes, *item)
	}
	sort.Slice(result.Scopes, func(i, j int) bool {
		if result.Scopes[i].Used != result.Scopes[j].Used {
			return result.Scopes[i].Used > result.Scopes[j].Used
		}
		return result.Scopes[i].Egress < result.Scopes[j].Egress
	})
	return result
}

// scopeActive reports whether a scope carries evidence worth reporting. A
// scope that only recorded last-attempt fields for a transport error or 403
// spent nothing and learned nothing, so it is not evidence.
func scopeActive(scope *scopeState) bool {
	return scope.Used > 0 || scope.Successes > 0 || scope.Ceiling > 0 ||
		scope.Quota429 > 0 || scope.ExhaustedUntil != nil
}

func learnedLimit(item *ScopeSnapshot, assumed, shared int) (int, string) {
	if item.LimitCeil > 0 {
		limit := item.LimitCeil
		if item.LimitFloor > limit {
			limit = item.LimitFloor
		}
		return limit, "learned"
	}
	if assumed > 0 && item.LimitFloor > assumed {
		return item.LimitFloor, "learned"
	}
	if shared > 0 {
		return shared, "learned"
	}
	if assumed > 0 {
		return assumed, "assumed"
	}
	return 0, "unknown"
}

func confidence(item *ScopeSnapshot, source string) string {
	switch {
	case item.Exhaustions >= 2 && item.LimitCeil > 0 &&
		absInt(item.LimitCeil-item.LimitFloor) <= maxInt(10, item.LimitCeil/20):
		return "high"
	case item.Exhaustions >= 1:
		return "medium"
	case source == "learned" || source == "assumed":
		return "low"
	}
	return "none"
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (t *Tracker) markDirty() {
	if t.dirty == nil {
		return
	}
	select {
	case t.dirty <- struct{}{}:
	default:
	}
}

func (t *Tracker) load() {
	data, err := os.ReadFile(t.path)
	if err != nil {
		return
	}
	var saved persisted
	if err := json.Unmarshal(data, &saved); err != nil || saved.Days == nil {
		return
	}
	cutoff := t.now().UTC().AddDate(0, 0, -keepDays).Format("2006-01-02")
	for key, day := range saved.Days {
		if key < cutoff || day == nil {
			continue
		}
		if day.Scopes == nil {
			day.Scopes = make(map[string]*scopeState)
		}
		t.days[key] = day
	}
}

func (t *Tracker) save() {
	t.saveMu.Lock()
	defer t.saveMu.Unlock()
	t.mu.Lock()
	data, err := json.MarshalIndent(persisted{Version: persistVersion, Days: t.days}, "", "  ")
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
	if err != nil {
		return
	}
	if err := os.Rename(tempPath, t.path); err != nil {
		return
	}
}

func isSuccess(status int) bool {
	return status >= 200 && status < 300
}

func billable(status int) bool {
	switch {
	case isSuccess(status):
		return true
	case status == http.StatusBadRequest,
		status == http.StatusNotFound,
		status == http.StatusTooManyRequests,
		status == 529:
		return true
	}
	return false
}

func isFreeLimitRejection(status int, body []byte) bool {
	if status != http.StatusTooManyRequests || len(body) == 0 {
		return false
	}
	lower := bytes.ToLower(body)
	return bytes.Contains(lower, []byte("freeusagelimiterror")) ||
		bytes.Contains(lower, []byte("free usage limit"))
}

func nearMidnight(when time.Time, retryAfter time.Duration) bool {
	// A hint shorter than a minute is a transient throttle, not a daily reset;
	// treating it as exhaustion would park a healthy exit until midnight.
	if retryAfter < time.Minute {
		return false
	}
	delta := nextUTCReset(when).Sub(when) - retryAfter
	if delta < 0 {
		delta = -delta
	}
	return delta <= midnightSkew
}

// GenuineQuota reports whether an attempt is a real daily-quota exhaustion:
// the free-limit error body plus a Retry-After that lands on the next UTC
// midnight. The gateway and the probe share this rule.
func GenuineQuota(status int, body []byte, retryAfter time.Duration, when time.Time) bool {
	return isFreeLimitRejection(status, body) && nearMidnight(when, retryAfter)
}

// ClassifyAttempt labels one observed attempt for operator display. It follows
// the same rules as Record: a genuine daily-quota 429 is "quota", the same
// error shape with a hint that misses the daily reset is "session_risk", and
// any other throttle is "rate_limited".
func ClassifyAttempt(status int, body []byte, retryAfter time.Duration, when time.Time, transportErr bool) string {
	switch {
	case transportErr:
		return "transport_error"
	case status == http.StatusUnauthorized:
		return "auth"
	case status == http.StatusForbidden:
		return "blocked"
	case status == http.StatusTooManyRequests:
		if isFreeLimitRejection(status, body) {
			if nearMidnight(when, retryAfter) {
				return "quota"
			}
			return "session_risk"
		}
		return "rate_limited"
	case status == http.StatusBadGateway, status == http.StatusServiceUnavailable, status == http.StatusGatewayTimeout:
		return "transport_error"
	case isSuccess(status):
		return "ok"
	}
	return "error"
}

func nextUTCReset(when time.Time) time.Time {
	utc := when.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day()+1, 0, 0, 0, 0, time.UTC)
}

// EgressLabel returns the stable label the gateway records for one attempt
// egress. WARP already labels itself "warp:<block>"; other transports are named
// by their configured proxy (or "direct").
func EgressLabel(proxyName, redacted string) string {
	if strings.HasPrefix(proxyName, "warp:") {
		return proxyName
	}
	if redacted = strings.TrimSpace(redacted); redacted != "" {
		return redacted
	}
	if proxyName = strings.TrimSpace(proxyName); proxyName != "" {
		return proxyName
	}
	return "direct"
}
