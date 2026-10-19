package telemetry

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"opencode2api/internal/protocol"
)

// metricBucket holds the request metrics of one minute. Metrics keeps 60 of
// them, indexed by minute modulo 60, to cover the last hour.
type metricBucket struct {
	minute        int64
	total         uint64
	success       uint64
	errors        uint64
	duration      uint64
	histogram     [11]uint64
	endpoints     map[string]uint64
	models        map[string]uint64
	tiers         map[string]uint64
	statuses      map[string]uint64
	usageRequests uint64
	usageReported uint64
	tokens        TokenCounts
	usageModels   map[string]TokenCounts
	usageTiers    map[string]TokenCounts
	usageSessions map[string]TokenCounts
	usageKeys     map[string]ServerKeyUsage
}

// reset clears the bucket for reuse by minute.
func (b *metricBucket) reset(minute int64) {
	*b = metricBucket{
		minute: minute, endpoints: make(map[string]uint64), models: make(map[string]uint64),
		tiers: make(map[string]uint64), statuses: make(map[string]uint64),
		usageModels: make(map[string]TokenCounts), usageTiers: make(map[string]TokenCounts),
		usageSessions: make(map[string]TokenCounts), usageKeys: make(map[string]ServerKeyUsage),
	}
}

// TokenCounts sums the token usage the upstream reported.
type TokenCounts struct {
	Input     uint64 `json:"input_tokens"`
	Output    uint64 `json:"output_tokens"`
	Cached    uint64 `json:"cached_tokens"`
	Reasoning uint64 `json:"reasoning_tokens"`
	Total     uint64 `json:"total_tokens"`
}

// UsagePeriod is the token usage of the requests routed to a tier over one
// period. Coverage is the share of those Requests whose usage the upstream
// Reported.
type UsagePeriod struct {
	Requests uint64                 `json:"requests"`
	Reported uint64                 `json:"reported"`
	Coverage float64                `json:"coverage"`
	Tokens   TokenCounts            `json:"tokens"`
	Models   map[string]TokenCounts `json:"models"`
	Tiers    map[string]TokenCounts `json:"tiers"`
	// Sessions maps canonical session ID to token burn. Sessions share one
	// upstream IP bucket on the anonymous lane, so this shows which
	// conversation is draining the daily budget.
	Sessions map[string]TokenCounts `json:"sessions"`
	// ServerKeys maps the local server key fingerprint (or "playground") to
	// its share of Requests and Tokens, so the entries sum to the totals.
	ServerKeys map[string]ServerKeyUsage `json:"server_keys"`
}

// ServerKeyUsage is the request count and token usage of one server key.
type ServerKeyUsage struct {
	Requests uint64 `json:"requests"`
	TokenCounts
}

// UsageSnapshot is the token usage since start and over the last hour.
type UsageSnapshot struct {
	Lifetime UsagePeriod `json:"lifetime"`
	Window   UsagePeriod `json:"last_hour"`
}

// AttemptCounts counts upstream attempts by result.
type AttemptCounts struct {
	Total   uint64 `json:"total"`
	Success uint64 `json:"success"`
	Failed  uint64 `json:"failed"`
}

// AttemptAggregate counts upstream attempts in total and per tier, channel and
// upstream key.
type AttemptAggregate struct {
	AttemptCounts
	SuccessRate float64                  `json:"success_rate"`
	Tiers       map[string]AttemptCounts `json:"tiers"`
	Channels    map[string]AttemptCounts `json:"channels"`
	Keys        map[string]AttemptCounts `json:"keys"`
}

// UpstreamAttempt is one upstream call made for a client request: one key or
// anonymous credential through one proxy.
type UpstreamAttempt struct {
	Time       time.Time `json:"time"`
	RequestID  string    `json:"request_id"`
	Model      string    `json:"model"`
	Tier       string    `json:"tier"`
	Attempt    int       `json:"attempt"`
	KeyID      string    `json:"key_id"`
	Channel    string    `json:"channel"`
	Anonymous  bool      `json:"anonymous"`
	Proxy      string    `json:"proxy_node"`
	Status     int       `json:"status,omitempty"`
	DurationMS int64     `json:"duration_ms"`
	Success    bool      `json:"success"`
	Outcome    string    `json:"outcome"`
}

// UpstreamRequest is the credential route that was actually used for one
// client inference request. It is kept separately from UpstreamAttempt,
// because a single request can try several keys before it succeeds. A request
// that never reached an upstream is recorded too, with Channel "not_routed".
type UpstreamRequest struct {
	Time        time.Time `json:"time"`
	RequestID   string    `json:"request_id"`
	Session     string    `json:"session,omitempty"`
	ServerKeyID string    `json:"server_key_id,omitempty"`
	Model       string    `json:"model"`
	Tier        string    `json:"tier,omitempty"`
	KeyID       string    `json:"key_id,omitempty"`
	Channel     string    `json:"channel"`
	Anonymous   bool      `json:"anonymous"`
	Proxy       string    `json:"proxy_node,omitempty"`
	Attempts    int       `json:"attempts"`
	Status      int       `json:"status"`
	DurationMS  int64     `json:"duration_ms"`
	Success     bool      `json:"success"`
	Outcome     string    `json:"outcome"`
}

// attemptBucket holds the upstream attempts of one minute, like metricBucket.
type attemptBucket struct {
	minute    int64
	aggregate AttemptAggregate
}

// UpstreamSnapshot is the upstream attempt view of the monitor: aggregates
// since start and over the last hour, plus the newest requests and attempts of
// the last hour, oldest first.
type UpstreamSnapshot struct {
	Lifetime AttemptAggregate  `json:"lifetime"`
	Window   AttemptAggregate  `json:"last_hour"`
	Requests []UpstreamRequest `json:"requests"`
	Recent   []UpstreamAttempt `json:"recent"`
}

// The recent request and attempt buffers hold at most an hour of entries up to
// these capacities; a snapshot returns the newest monitorRecentOutputLimit.
const (
	monitorRecentRequestCapacity = 10000
	monitorRecentAttemptCapacity = 20000
	monitorRecentOutputLimit     = 500
)

// upstreamRequestBuffer is a fixed-capacity ring of recent requests in the
// order they were added. Add overwrites the oldest entry when it is full, and
// PruneBefore drops entries from the oldest end until one is not older than
// cutoff.
type upstreamRequestBuffer struct {
	items []UpstreamRequest
	start int
	count int
}

func upstreamRequestRing() upstreamRequestBuffer {
	return upstreamRequestBuffer{items: make([]UpstreamRequest, monitorRecentRequestCapacity)}
}

// Add appends value, overwriting the oldest entry once the ring is full.
func (ring *upstreamRequestBuffer) Add(value UpstreamRequest) {
	if len(ring.items) == 0 {
		ring.items = make([]UpstreamRequest, monitorRecentRequestCapacity)
	}
	index := (ring.start + ring.count) % len(ring.items)
	if ring.count == len(ring.items) {
		ring.items[index] = value
		ring.start = (ring.start + 1) % len(ring.items)
		return
	}
	ring.items[index] = value
	ring.count++
}

// PruneBefore drops the oldest entries that are older than cutoff.
func (ring *upstreamRequestBuffer) PruneBefore(cutoff time.Time) {
	for ring.count > 0 && ring.items[ring.start].Time.Before(cutoff) {
		ring.items[ring.start] = UpstreamRequest{}
		ring.start = (ring.start + 1) % len(ring.items)
		ring.count--
	}
}

// Snapshot copies the newest limit entries that are not older than cutoff,
// oldest first.
func (ring *upstreamRequestBuffer) Snapshot(cutoff time.Time, limit int) []UpstreamRequest {
	if limit < 1 || ring.count == 0 {
		return nil
	}
	result := make([]UpstreamRequest, 0, min(ring.count, limit))
	for offset := 0; offset < ring.count; offset++ {
		value := ring.items[(ring.start+offset)%len(ring.items)]
		if !value.Time.Before(cutoff) {
			result = append(result, value)
		}
	}
	if len(result) > limit {
		result = result[len(result)-limit:]
	}
	return result
}

// upstreamAttemptBuffer is the upstreamRequestBuffer counterpart for attempts.
type upstreamAttemptBuffer struct {
	items []UpstreamAttempt
	start int
	count int
}

func upstreamAttemptRing() upstreamAttemptBuffer {
	return upstreamAttemptBuffer{items: make([]UpstreamAttempt, monitorRecentAttemptCapacity)}
}

// Add appends value, overwriting the oldest entry once the ring is full.
func (ring *upstreamAttemptBuffer) Add(value UpstreamAttempt) {
	if len(ring.items) == 0 {
		ring.items = make([]UpstreamAttempt, monitorRecentAttemptCapacity)
	}
	index := (ring.start + ring.count) % len(ring.items)
	if ring.count == len(ring.items) {
		ring.items[index] = value
		ring.start = (ring.start + 1) % len(ring.items)
		return
	}
	ring.items[index] = value
	ring.count++
}

// PruneBefore drops the oldest entries that are older than cutoff.
func (ring *upstreamAttemptBuffer) PruneBefore(cutoff time.Time) {
	for ring.count > 0 && ring.items[ring.start].Time.Before(cutoff) {
		ring.items[ring.start] = UpstreamAttempt{}
		ring.start = (ring.start + 1) % len(ring.items)
		ring.count--
	}
}

// Snapshot copies the newest limit entries that are not older than cutoff,
// oldest first.
func (ring *upstreamAttemptBuffer) Snapshot(cutoff time.Time, limit int) []UpstreamAttempt {
	if limit < 1 || ring.count == 0 {
		return nil
	}
	result := make([]UpstreamAttempt, 0, min(ring.count, limit))
	for offset := 0; offset < ring.count; offset++ {
		value := ring.items[(ring.start+offset)%len(ring.items)]
		if !value.Time.Before(cutoff) {
			result = append(result, value)
		}
	}
	if len(result) > limit {
		result = result[len(result)-limit:]
	}
	return result
}

// Metrics aggregates request, token usage and upstream attempt statistics for
// the WebUI monitor: totals since start plus a per-minute view of the last
// hour. The request counters are atomic; mu guards everything else.
type Metrics struct {
	started         time.Time
	active          atomic.Int64
	activeStreams   atomic.Int64
	total           atomic.Uint64
	success         atomic.Uint64
	errors          atomic.Uint64
	mu              sync.Mutex
	buckets         [60]metricBucket
	lifetimeUsage   UsagePeriod
	sessionSeen     map[string]int64
	attemptLifetime AttemptAggregate
	attemptBuckets  [60]attemptBucket
	recentRequests  upstreamRequestBuffer
	recentAttempts  upstreamAttemptBuffer
}

// Monitor creates an empty Metrics whose uptime starts now.
func Monitor() *Metrics {
	return &Metrics{
		started:         time.Now().UTC(),
		lifetimeUsage:   usagePeriod(),
		sessionSeen:     make(map[string]int64),
		attemptLifetime: attemptAggregate(),
		recentRequests:  upstreamRequestRing(),
		recentAttempts:  upstreamAttemptRing(),
	}
}

// BeginStream records a stream entering its response body phase.
func (m *Metrics) BeginStream() { m.activeStreams.Add(1) }

// EndStream releases a stream recorded by BeginStream.
func (m *Metrics) EndStream() { m.activeStreams.Add(-1) }

// latencyBounds are the upper bounds, in milliseconds, of the latency
// histogram buckets. The histogram's last bucket counts slower requests.
var latencyBounds = [...]uint64{50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000}

// Record accounts one finished HTTP request. Every request counts toward the
// totals, latency, endpoints and statuses. Token usage is counted once the
// request was routed to a tier, and a request that carries a request ID and a
// model joins the recent requests.
func (m *Metrics) Record(endpoint string, status int, duration time.Duration, meta *RequestMeta) {
	success := requestSucceeded(status, meta)
	m.total.Add(1)
	if success {
		m.success.Add(1)
	} else {
		m.errors.Add(1)
	}
	minute := time.Now().Unix() / 60
	m.mu.Lock()
	bucket := &m.buckets[minute%60]
	if bucket.minute != minute {
		bucket.reset(minute)
	}
	bucket.total++
	if success {
		bucket.success++
	} else {
		bucket.errors++
	}
	milliseconds := uint64(max(duration.Milliseconds(), 0))
	bucket.duration += milliseconds
	index := len(latencyBounds)
	for i, bound := range latencyBounds {
		if milliseconds <= bound {
			index = i
			break
		}
	}
	bucket.histogram[index]++
	bucket.endpoints[endpoint]++
	bucket.statuses[fmt.Sprint(status)]++
	if meta != nil {
		if meta.Model != "" {
			bucket.models[meta.Model]++
		}
		if meta.Tier != "" {
			bucket.tiers[meta.Tier]++
			bucket.usageRequests++
			m.lifetimeUsage.Requests++
			if meta.UsageReported {
				bucket.usageReported++
				m.lifetimeUsage.Reported++
			}
			tokens := tokenCounts(meta.Usage)
			addTokenCounts(&bucket.tokens, tokens)
			addTokenCounts(&m.lifetimeUsage.Tokens, tokens)
			addTokenMap(bucket.usageModels, meta.Model, tokens)
			addTokenMap(bucket.usageTiers, meta.Tier, tokens)
			addTokenMap(m.lifetimeUsage.Models, meta.Model, tokens)
			addTokenMap(m.lifetimeUsage.Tiers, meta.Tier, tokens)
			addTokenMap(bucket.usageSessions, meta.Session, tokens)
			m.addLifetimeSessionLocked(meta.Session, tokens)
			addServerKeyUsage(bucket.usageKeys, meta.ServerKeyID, ServerKeyUsage{Requests: 1, TokenCounts: tokens})
			addServerKeyUsage(m.lifetimeUsage.ServerKeys, meta.ServerKeyID, ServerKeyUsage{Requests: 1, TokenCounts: tokens})
		}
		if meta.Request != "" && meta.Model != "" {
			request := UpstreamRequest{
				Time: time.Now().UTC(), RequestID: meta.Request, Session: meta.Session, ServerKeyID: meta.ServerKeyID, Model: meta.Model, Tier: meta.Tier,
				KeyID: meta.KeyID, Channel: meta.Channel, Anonymous: meta.Anonymous, Proxy: meta.Proxy,
				Attempts: meta.Attempts, Status: status, DurationMS: max(duration.Milliseconds(), 0),
				Success: success,
			}
			if request.Channel == "" {
				request.Channel = "not_routed"
			}
			if request.Anonymous {
				request.KeyID = "anonymous"
			}
			request.Outcome = requestOutcome(status, request.Channel)
			if meta.Outcome != "" {
				request.Outcome = meta.Outcome
			}
			m.recentRequests.PruneBefore(request.Time.Add(-time.Hour))
			m.recentRequests.Add(request)
		}
	}
	m.mu.Unlock()
}

// requestSucceeded reports whether a request counts as a success: a 2xx or 3xx
// status and no terminal failure in meta.Outcome. A stream can fail after HTTP
// headers have been sent. Preserve its HTTP status while using the terminal
// result for request success and error counts. Without metadata, terminal
// outcome is unknown and the request is conservatively counted as a failure.
func requestSucceeded(status int, meta *RequestMeta) bool {
	return status >= 200 && status < 400 && meta != nil && meta.Outcome == ""
}

// requestOutcome classifies a request that carries no explicit outcome. A
// request without a channel, or with the "not_routed" default, never reached an
// upstream.
func requestOutcome(status int, channel string) string {
	if channel == "" || channel == "not_routed" {
		return "not_routed"
	}
	if status >= 200 && status < 400 {
		return "success"
	}
	if status >= 400 && status < 500 {
		return "client_error"
	}
	if status >= 500 {
		return "server_error"
	}
	return "unknown"
}

// RecordAttempt accounts one upstream attempt; a zero Time means now.
func (m *Metrics) RecordAttempt(attempt UpstreamAttempt) {
	if attempt.Time.IsZero() {
		attempt.Time = time.Now().UTC()
	} else {
		attempt.Time = attempt.Time.UTC()
	}
	minute := attempt.Time.Unix() / 60
	m.mu.Lock()
	recordAttemptAggregate(&m.attemptLifetime, attempt)
	bucket := &m.attemptBuckets[minute%60]
	if bucket.minute != minute {
		*bucket = attemptBucket{minute: minute, aggregate: attemptAggregate()}
	}
	recordAttemptAggregate(&bucket.aggregate, attempt)
	m.recentAttempts.PruneBefore(attempt.Time.Add(-time.Hour))
	m.recentAttempts.Add(attempt)
	m.mu.Unlock()
}

// MonitorSnapshot is the WebUI monitor view of Metrics. Lifetime covers the
// time since start and Window the last hour, which Series breaks down per
// minute; the per-endpoint, model, tier and status counts cover the last hour.
type MonitorSnapshot struct {
	StartedAt     time.Time         `json:"started_at"`
	UptimeSeconds int64             `json:"uptime_seconds"`
	Active        int64             `json:"active_requests"`
	ActiveStreams int64             `json:"active_streams"`
	Lifetime      MetricSummary     `json:"lifetime"`
	Window        MetricSummary     `json:"last_hour"`
	Series        []MetricSeries    `json:"series"`
	Endpoints     map[string]uint64 `json:"endpoints"`
	Models        map[string]uint64 `json:"models"`
	Tiers         map[string]uint64 `json:"tiers"`
	Statuses      map[string]uint64 `json:"statuses"`
	Usage         UsageSnapshot     `json:"usage"`
	Upstream      UpstreamSnapshot  `json:"upstream"`
}

// MetricSummary counts requests by result. The latency fields are filled only
// for the last hour, and each percentile is the upper bound of the histogram
// bucket that holds it.
type MetricSummary struct {
	Total       uint64  `json:"total"`
	Success     uint64  `json:"success"`
	Errors      uint64  `json:"errors"`
	SuccessRate float64 `json:"success_rate"`
	AverageMS   float64 `json:"average_ms,omitempty"`
	P50MS       uint64  `json:"p50_ms,omitempty"`
	P95MS       uint64  `json:"p95_ms,omitempty"`
	P99MS       uint64  `json:"p99_ms,omitempty"`
}

// MetricSeries is one minute of the last-hour series.
type MetricSeries struct {
	Minute          time.Time `json:"minute"`
	Total           uint64    `json:"total"`
	Success         uint64    `json:"success"`
	Errors          uint64    `json:"errors"`
	InputTokens     uint64    `json:"input_tokens"`
	OutputTokens    uint64    `json:"output_tokens"`
	CachedTokens    uint64    `json:"cached_tokens"`
	ReasoningTokens uint64    `json:"reasoning_tokens"`
	TotalTokens     uint64    `json:"total_tokens"`
	UsageReported   uint64    `json:"usage_reported"`
}

// ServerKeyUsage returns per-server-key usage for the lifetime and the last
// hour. The API key list polls it, so it skips building a full Snapshot.
func (m *Metrics) ServerKeyUsage() (lifetime, window map[string]ServerKeyUsage) {
	nowMinute := time.Now().Unix() / 60
	lifetime, window = make(map[string]ServerKeyUsage), make(map[string]ServerKeyUsage)
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, value := range m.lifetimeUsage.ServerKeys {
		lifetime[key] = value
	}
	for offset := int64(0); offset < 60; offset++ {
		minute := nowMinute - offset
		if bucket := &m.buckets[minute%60]; bucket.minute == minute {
			for key, value := range bucket.usageKeys {
				addServerKeyUsage(window, key, value)
			}
		}
	}
	return lifetime, window
}

// Snapshot builds the monitor view. It also prunes recent requests and
// attempts older than an hour.
func (m *Metrics) Snapshot() MonitorSnapshot {
	nowMinute := time.Now().Unix() / 60
	window := MetricSummary{}
	var histogram [11]uint64
	endpoints, models, tiers, statuses := map[string]uint64{}, map[string]uint64{}, map[string]uint64{}, map[string]uint64{}
	series := make([]MetricSeries, 0, 60)
	usageWindow := usagePeriod()
	upstreamWindow := attemptAggregate()
	var recentRequests []UpstreamRequest
	var recentAttempts []UpstreamAttempt
	m.mu.Lock()
	for offset := int64(59); offset >= 0; offset-- {
		minute := nowMinute - offset
		bucket := &m.buckets[minute%60]
		entry := MetricSeries{Minute: time.Unix(minute*60, 0).UTC()}
		if bucket.minute == minute {
			entry.Total, entry.Success, entry.Errors = bucket.total, bucket.success, bucket.errors
			entry.InputTokens, entry.OutputTokens = bucket.tokens.Input, bucket.tokens.Output
			entry.CachedTokens, entry.ReasoningTokens, entry.TotalTokens = bucket.tokens.Cached, bucket.tokens.Reasoning, bucket.tokens.Total
			entry.UsageReported = bucket.usageReported
			window.Total += bucket.total
			window.Success += bucket.success
			window.Errors += bucket.errors
			window.AverageMS += float64(bucket.duration)
			for i := range histogram {
				histogram[i] += bucket.histogram[i]
			}
			mergeCounts(endpoints, bucket.endpoints)
			mergeCounts(models, bucket.models)
			mergeCounts(tiers, bucket.tiers)
			mergeCounts(statuses, bucket.statuses)
			usageWindow.Requests += bucket.usageRequests
			usageWindow.Reported += bucket.usageReported
			addTokenCounts(&usageWindow.Tokens, bucket.tokens)
			mergeTokenMaps(usageWindow.Models, bucket.usageModels)
			mergeTokenMaps(usageWindow.Tiers, bucket.usageTiers)
			mergeTokenMaps(usageWindow.Sessions, bucket.usageSessions)
			for key, value := range bucket.usageKeys {
				addServerKeyUsage(usageWindow.ServerKeys, key, value)
			}
		}
		attemptBucket := &m.attemptBuckets[minute%60]
		if attemptBucket.minute == minute {
			mergeAttemptAggregate(&upstreamWindow, attemptBucket.aggregate)
		}
		series = append(series, entry)
	}
	usageLifetime := cloneUsagePeriod(m.lifetimeUsage)
	upstreamLifetime := cloneAttemptAggregate(m.attemptLifetime)
	cutoff := time.Now().Add(-time.Hour)
	m.recentRequests.PruneBefore(cutoff)
	m.recentAttempts.PruneBefore(cutoff)
	recentRequests = m.recentRequests.Snapshot(cutoff, monitorRecentOutputLimit)
	recentAttempts = m.recentAttempts.Snapshot(cutoff, monitorRecentOutputLimit)
	m.mu.Unlock()
	finalizeUsagePeriod(&usageLifetime)
	finalizeUsagePeriod(&usageWindow)
	finalizeAttemptAggregate(&upstreamLifetime)
	finalizeAttemptAggregate(&upstreamWindow)
	if window.Total > 0 {
		window.SuccessRate = float64(window.Success) / float64(window.Total)
		window.AverageMS /= float64(window.Total)
		window.P50MS = histogramPercentile(histogram, window.Total, 0.50)
		window.P95MS = histogramPercentile(histogram, window.Total, 0.95)
		window.P99MS = histogramPercentile(histogram, window.Total, 0.99)
	}
	lifetime := MetricSummary{Total: m.total.Load(), Success: m.success.Load(), Errors: m.errors.Load()}
	if lifetime.Total > 0 {
		lifetime.SuccessRate = float64(lifetime.Success) / float64(lifetime.Total)
	}
	return MonitorSnapshot{
		StartedAt: m.started, UptimeSeconds: int64(time.Since(m.started).Seconds()), Active: m.active.Load(),
		ActiveStreams: m.activeStreams.Load(), Lifetime: lifetime, Window: window, Series: series,
		Endpoints: endpoints, Models: models, Tiers: tiers, Statuses: statuses,
		Usage:    UsageSnapshot{Lifetime: usageLifetime, Window: usageWindow},
		Upstream: UpstreamSnapshot{Lifetime: upstreamLifetime, Window: upstreamWindow, Requests: recentRequests, Recent: recentAttempts},
	}
}

func mergeCounts(target, source map[string]uint64) {
	for key, value := range source {
		target[key] += value
	}
}

// usagePeriod returns an empty UsagePeriod with its maps allocated.
func usagePeriod() UsagePeriod {
	return UsagePeriod{Models: make(map[string]TokenCounts), Tiers: make(map[string]TokenCounts), Sessions: make(map[string]TokenCounts), ServerKeys: make(map[string]ServerKeyUsage)}
}

func tokenCounts(usage protocol.Usage) TokenCounts {
	return TokenCounts{
		Input: uint64(max(usage.Input, 0)), Output: uint64(max(usage.Output, 0)),
		Cached: uint64(max(usage.Cached, 0)), Reasoning: uint64(max(usage.Reasoning, 0)), Total: uint64(max(usage.Total, 0)),
	}
}

func addTokenCounts(target *TokenCounts, source TokenCounts) {
	target.Input += source.Input
	target.Output += source.Output
	target.Cached += source.Cached
	target.Reasoning += source.Reasoning
	target.Total += source.Total
}

func addTokenMap(target map[string]TokenCounts, key string, value TokenCounts) {
	if key == "" {
		return
	}
	current := target[key]
	addTokenCounts(&current, value)
	target[key] = current
}

func addServerKeyUsage(target map[string]ServerKeyUsage, key string, value ServerKeyUsage) {
	if key == "" {
		return
	}
	current := target[key]
	current.Requests += value.Requests
	addTokenCounts(&current.TokenCounts, value.TokenCounts)
	target[key] = current
}

func mergeTokenMaps(target, source map[string]TokenCounts) {
	for key, value := range source {
		addTokenMap(target, key, value)
	}
}

// maxLifetimeSessions bounds the lifetime per-session burn map. Session IDs
// are high-cardinality (one per conversation), so an unbounded map grows
// forever. When full, the least recently seen session is evicted: evicting the
// smallest total would drop the newest conversation first, so every new
// session would push out the previous one. Buckets and snapshots are
// short-lived and stay uncapped.
const maxLifetimeSessions = 500

// addLifetimeSessionLocked adds tokens to the lifetime total of session and
// marks it as just seen, evicting the least recently seen session first when
// the map is full. The caller holds m.mu.
func (m *Metrics) addLifetimeSessionLocked(session string, tokens TokenCounts) {
	if session == "" {
		return
	}
	if _, ok := m.lifetimeUsage.Sessions[session]; !ok && len(m.lifetimeUsage.Sessions) >= maxLifetimeSessions {
		victim, oldest := "", int64(0)
		for key, seen := range m.sessionSeen {
			if victim == "" || seen < oldest {
				victim, oldest = key, seen
			}
		}
		delete(m.lifetimeUsage.Sessions, victim)
		delete(m.sessionSeen, victim)
	}
	m.sessionSeen[session] = time.Now().UnixNano()
	addTokenMap(m.lifetimeUsage.Sessions, session, tokens)
}

func cloneUsagePeriod(source UsagePeriod) UsagePeriod {
	result := usagePeriod()
	result.Requests, result.Reported, result.Tokens = source.Requests, source.Reported, source.Tokens
	mergeTokenMaps(result.Models, source.Models)
	mergeTokenMaps(result.Tiers, source.Tiers)
	mergeTokenMaps(result.Sessions, source.Sessions)
	for key, value := range source.ServerKeys {
		addServerKeyUsage(result.ServerKeys, key, value)
	}
	return result
}

func finalizeUsagePeriod(period *UsagePeriod) {
	if period.Requests > 0 {
		period.Coverage = float64(period.Reported) / float64(period.Requests)
	}
}

// attemptAggregate returns an empty AttemptAggregate with its maps allocated.
func attemptAggregate() AttemptAggregate {
	return AttemptAggregate{
		Tiers: make(map[string]AttemptCounts), Channels: make(map[string]AttemptCounts), Keys: make(map[string]AttemptCounts),
	}
}

func recordAttemptAggregate(target *AttemptAggregate, attempt UpstreamAttempt) {
	recordAttemptCounts(&target.AttemptCounts, attempt.Success)
	addAttemptMap(target.Tiers, attempt.Tier, attempt.Success)
	addAttemptMap(target.Channels, attempt.Channel, attempt.Success)
	addAttemptMap(target.Keys, attempt.KeyID, attempt.Success)
}

func recordAttemptCounts(target *AttemptCounts, success bool) {
	target.Total++
	if success {
		target.Success++
	} else {
		target.Failed++
	}
}

func addAttemptMap(target map[string]AttemptCounts, key string, success bool) {
	if key == "" {
		return
	}
	current := target[key]
	recordAttemptCounts(&current, success)
	target[key] = current
}

func mergeAttemptAggregate(target *AttemptAggregate, source AttemptAggregate) {
	target.Total += source.Total
	target.Success += source.Success
	target.Failed += source.Failed
	mergeAttemptMaps(target.Tiers, source.Tiers)
	mergeAttemptMaps(target.Channels, source.Channels)
	mergeAttemptMaps(target.Keys, source.Keys)
}

func mergeAttemptMaps(target, source map[string]AttemptCounts) {
	for key, value := range source {
		current := target[key]
		current.Total += value.Total
		current.Success += value.Success
		current.Failed += value.Failed
		target[key] = current
	}
}

func cloneAttemptAggregate(source AttemptAggregate) AttemptAggregate {
	result := attemptAggregate()
	mergeAttemptAggregate(&result, source)
	return result
}

func finalizeAttemptAggregate(aggregate *AttemptAggregate) {
	if aggregate.Total > 0 {
		aggregate.SuccessRate = float64(aggregate.Success) / float64(aggregate.Total)
	}
}

// histogramPercentile returns the upper bound of the bucket holding the given
// percentile of total, or one past the last bound for the overflow bucket.
func histogramPercentile(histogram [11]uint64, total uint64, percentile float64) uint64 {
	target := uint64(float64(total)*percentile + 0.999)
	var count uint64
	for i, value := range histogram {
		count += value
		if count >= target {
			if i < len(latencyBounds) {
				return latencyBounds[i]
			}
			return latencyBounds[len(latencyBounds)-1] + 1
		}
	}
	return 0
}
