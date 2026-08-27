package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type LogEvent struct {
	Sequence  uint64         `json:"sequence"`
	Time      time.Time      `json:"time"`
	Level     string         `json:"level"`
	Message   string         `json:"message"`
	Component string         `json:"component,omitempty"`
	Fields    map[string]any `json:"fields,omitempty"`
}

type logSubscriber struct {
	ch chan LogEvent
}

type LogHub struct {
	mu          sync.RWMutex
	buffer      []LogEvent
	start       int
	count       int
	next        uint64
	subscribers map[uint64]*logSubscriber
	nextSub     uint64
}

func NewLogHub(capacity int) *LogHub {
	if capacity < 100 {
		capacity = 100
	}
	return &LogHub{buffer: make([]LogEvent, capacity), subscribers: make(map[uint64]*logSubscriber)}
}

func (h *LogHub) Resize(capacity int) {
	if capacity < 100 {
		capacity = 100
	}
	h.mu.Lock()
	if capacity == len(h.buffer) {
		h.mu.Unlock()
		return
	}
	keep := min(h.count, capacity)
	next := make([]LogEvent, capacity)
	for i := 0; i < keep; i++ {
		source := (h.start + h.count - keep + i) % len(h.buffer)
		next[i] = h.buffer[source]
	}
	h.buffer, h.start, h.count = next, 0, keep
	h.mu.Unlock()
}

func (h *LogHub) Publish(event LogEvent) {
	h.mu.Lock()
	h.next++
	event.Sequence = h.next
	if h.count < len(h.buffer) {
		index := (h.start + h.count) % len(h.buffer)
		h.buffer[index] = event
		h.count++
	} else {
		h.buffer[h.start] = event
		h.start = (h.start + 1) % len(h.buffer)
	}
	for _, sub := range h.subscribers {
		select {
		case sub.ch <- event:
		default:
			// A slow browser must never block request processing. Its reconnect
			// cursor will make the gap visible on the next subscription.
		}
	}
	h.mu.Unlock()
}

func (h *LogHub) Recent(after uint64, limit int) ([]LogEvent, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if limit < 1 || limit > len(h.buffer) {
		limit = len(h.buffer)
	}
	oldest := uint64(0)
	if h.count > 0 {
		oldest = h.buffer[h.start].Sequence
	}
	gap := after > 0 && oldest > 0 && after+1 < oldest
	out := make([]LogEvent, 0, min(limit, h.count))
	for i := 0; i < h.count; i++ {
		event := h.buffer[(h.start+i)%len(h.buffer)]
		if event.Sequence > after {
			out = append(out, event)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, gap
}

func (h *LogHub) Subscribe() (<-chan LogEvent, func()) {
	h.mu.Lock()
	h.nextSub++
	id := h.nextSub
	sub := &logSubscriber{ch: make(chan LogEvent, 128)}
	h.subscribers[id] = sub
	h.mu.Unlock()
	return sub.ch, func() {
		h.mu.Lock()
		delete(h.subscribers, id)
		h.mu.Unlock()
	}
}

type SecretRedactor struct {
	values atomic.Value
}

func NewSecretRedactor() *SecretRedactor {
	r := &SecretRedactor{}
	r.values.Store([]string(nil))
	return r
}

func (r *SecretRedactor) Replace(cfg Config) {
	values := make([]string, 0, len(cfg.ServerKeys)+len(cfg.ZenKeys)+len(cfg.GoKeys)+2)
	values = append(values, cfg.ServerKeys...)
	values = append(values, cfg.ZenKeys...)
	values = append(values, cfg.GoKeys...)
	if cfg.WebUI.Password != "" {
		values = append(values, cfg.WebUI.Password)
	}
	for _, value := range []string{cfg.Upstream.Zen, cfg.Upstream.Go} {
		if parsed, err := url.Parse(value); err == nil && parsed.User != nil {
			values = append(values, value, parsed.User.Username())
			if password, ok := parsed.User.Password(); ok {
				values = append(values, password)
			}
		}
	}
	for _, value := range cfg.RuntimeProxies() {
		if value != "direct" {
			values = append(values, value)
			if parsed, err := url.Parse(value); err == nil && parsed.User != nil {
				values = append(values, parsed.User.Username())
				if password, ok := parsed.User.Password(); ok {
					values = append(values, password)
				}
			}
		}
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	r.values.Store(values)
}

func (r *SecretRedactor) String(value string) string {
	for _, secret := range r.values.Load().([]string) {
		if len(secret) >= 4 {
			value = strings.ReplaceAll(value, secret, "***")
		}
	}
	return value
}

type hubHandler struct {
	base     slog.Handler
	hub      *LogHub
	redactor *SecretRedactor
	attrs    []slog.Attr
	groups   []string
}

func NewStructuredLogger(level *slog.LevelVar, hub *LogHub, redactor *SecretRedactor) *slog.Logger {
	return newStructuredLogger(os.Stdout, level, hub, redactor)
}

func newStructuredLogger(output io.Writer, level *slog.LevelVar, hub *LogHub, redactor *SecretRedactor) *slog.Logger {
	base := slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level, ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
		return sanitizeLogAttr(redactor, attr)
	}})
	return slog.New(&hubHandler{base: base, hub: hub, redactor: redactor})
}

func sanitizeLogAttr(redactor *SecretRedactor, attr slog.Attr) slog.Attr {
	attr.Value = attr.Value.Resolve()
	lower := strings.ToLower(attr.Key)
	if strings.Contains(lower, "password") || strings.Contains(lower, "authorization") || strings.Contains(lower, "cookie") || strings.Contains(lower, "secret") {
		return slog.String(attr.Key, "***")
	}
	switch attr.Value.Kind() {
	case slog.KindString:
		attr.Value = slog.StringValue(redactor.String(attr.Value.String()))
	case slog.KindAny:
		if err, ok := attr.Value.Any().(error); ok {
			attr.Value = slog.StringValue(redactor.String(err.Error()))
		}
	}
	return attr
}

func (h *hubHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level)
}

func (h *hubHandler) Handle(ctx context.Context, record slog.Record) error {
	if err := h.base.Handle(ctx, record); err != nil {
		return err
	}
	fields := make(map[string]any, record.NumAttrs()+len(h.attrs))
	for _, attr := range h.attrs {
		h.addAttr(fields, attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		h.addAttr(fields, attr)
		return true
	})
	component, _ := fields["component"].(string)
	delete(fields, "component")
	h.hub.Publish(LogEvent{
		Time:      record.Time.UTC(),
		Level:     strings.ToLower(record.Level.String()),
		Message:   h.redactor.String(record.Message),
		Component: component,
		Fields:    fields,
	})
	return nil
}

func (h *hubHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.base = h.base.WithAttrs(attrs)
	clone.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &clone
}

func (h *hubHandler) WithGroup(name string) slog.Handler {
	clone := *h
	clone.base = h.base.WithGroup(name)
	clone.groups = append(append([]string(nil), h.groups...), name)
	return &clone
}

func (h *hubHandler) addAttr(fields map[string]any, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	key := attr.Key
	if len(h.groups) > 0 {
		key = strings.Join(append(append([]string(nil), h.groups...), key), ".")
	}
	lower := strings.ToLower(key)
	if strings.Contains(lower, "password") || strings.Contains(lower, "authorization") || strings.Contains(lower, "cookie") || strings.Contains(lower, "secret") {
		fields[key] = "***"
		return
	}
	var value any
	switch attr.Value.Kind() {
	case slog.KindString:
		value = h.redactor.String(attr.Value.String())
	case slog.KindInt64:
		value = attr.Value.Int64()
	case slog.KindUint64:
		value = attr.Value.Uint64()
	case slog.KindFloat64:
		value = attr.Value.Float64()
	case slog.KindBool:
		value = attr.Value.Bool()
	case slog.KindDuration:
		value = attr.Value.Duration().String()
	case slog.KindTime:
		value = attr.Value.Time().UTC()
	default:
		value = h.redactor.String(fmt.Sprint(attr.Value.Any()))
	}
	fields[key] = value
}

type requestMeta struct {
	Model         string
	Tier          string
	Request       string
	KeyID         string
	Channel       string
	Anonymous     bool
	Proxy         string
	Attempts      int
	Stream        bool
	Usage         bridgeUsage
	UsageReported bool
}

type requestMetaKey struct{}

func metaFromRequest(r *http.Request) *requestMeta {
	meta, _ := r.Context().Value(requestMetaKey{}).(*requestMeta)
	return meta
}

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
}

func (b *metricBucket) reset(minute int64) {
	*b = metricBucket{
		minute: minute, endpoints: make(map[string]uint64), models: make(map[string]uint64),
		tiers: make(map[string]uint64), statuses: make(map[string]uint64),
		usageModels: make(map[string]TokenCounts), usageTiers: make(map[string]TokenCounts),
	}
}

type TokenCounts struct {
	Input     uint64 `json:"input_tokens"`
	Output    uint64 `json:"output_tokens"`
	Cached    uint64 `json:"cached_tokens"`
	Reasoning uint64 `json:"reasoning_tokens"`
	Total     uint64 `json:"total_tokens"`
}

type UsagePeriod struct {
	Requests uint64                 `json:"requests"`
	Reported uint64                 `json:"reported"`
	Coverage float64                `json:"coverage"`
	Tokens   TokenCounts            `json:"tokens"`
	Models   map[string]TokenCounts `json:"models"`
	Tiers    map[string]TokenCounts `json:"tiers"`
}

type UsageSnapshot struct {
	Lifetime UsagePeriod `json:"lifetime"`
	Window   UsagePeriod `json:"last_hour"`
}

type AttemptCounts struct {
	Total   uint64 `json:"total"`
	Success uint64 `json:"success"`
	Failed  uint64 `json:"failed"`
}

type AttemptAggregate struct {
	AttemptCounts
	SuccessRate float64                  `json:"success_rate"`
	Tiers       map[string]AttemptCounts `json:"tiers"`
	Channels    map[string]AttemptCounts `json:"channels"`
	Keys        map[string]AttemptCounts `json:"keys"`
}

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
// because a single request can try several keys before it succeeds.
type UpstreamRequest struct {
	Time       time.Time `json:"time"`
	RequestID  string    `json:"request_id"`
	Model      string    `json:"model"`
	Tier       string    `json:"tier,omitempty"`
	KeyID      string    `json:"key_id,omitempty"`
	Channel    string    `json:"channel"`
	Anonymous  bool      `json:"anonymous"`
	Proxy      string    `json:"proxy_node,omitempty"`
	Attempts   int       `json:"attempts"`
	Status     int       `json:"status"`
	DurationMS int64     `json:"duration_ms"`
	Success    bool      `json:"success"`
	Outcome    string    `json:"outcome"`
}

type attemptBucket struct {
	minute    int64
	aggregate AttemptAggregate
}

type UpstreamSnapshot struct {
	Lifetime AttemptAggregate  `json:"lifetime"`
	Window   AttemptAggregate  `json:"last_hour"`
	Requests []UpstreamRequest `json:"requests"`
	Recent   []UpstreamAttempt `json:"recent"`
}

type Monitor struct {
	started         time.Time
	active          atomic.Int64
	activeStreams   atomic.Int64
	total           atomic.Uint64
	success         atomic.Uint64
	errors          atomic.Uint64
	mu              sync.Mutex
	buckets         [60]metricBucket
	lifetimeUsage   UsagePeriod
	attemptLifetime AttemptAggregate
	attemptBuckets  [60]attemptBucket
	recentRequests  []UpstreamRequest
	recentAttempts  []UpstreamAttempt
}

func NewMonitor() *Monitor {
	return &Monitor{
		started:         time.Now().UTC(),
		lifetimeUsage:   newUsagePeriod(),
		attemptLifetime: newAttemptAggregate(),
	}
}

var latencyBounds = [...]uint64{50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000}

func (m *Monitor) Record(endpoint string, status int, duration time.Duration, meta *requestMeta) {
	m.total.Add(1)
	if status >= 200 && status < 400 {
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
	if status >= 200 && status < 400 {
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
		}
		if meta.Request != "" && meta.Model != "" {
			request := UpstreamRequest{
				Time: time.Now().UTC(), RequestID: meta.Request, Model: meta.Model, Tier: meta.Tier,
				KeyID: meta.KeyID, Channel: meta.Channel, Anonymous: meta.Anonymous, Proxy: meta.Proxy,
				Attempts: meta.Attempts, Status: status, DurationMS: max(duration.Milliseconds(), 0),
				Success: status >= 200 && status < 400,
			}
			if request.Channel == "" {
				request.Channel = "not_routed"
			}
			if request.Anonymous {
				request.KeyID = "anonymous"
			}
			request.Outcome = requestOutcome(status, request.Channel)
			m.recentRequests = append(m.recentRequests, request)
			cutoff := request.Time.Add(-time.Hour)
			first := 0
			for first < len(m.recentRequests) && m.recentRequests[first].Time.Before(cutoff) {
				first++
			}
			if first > 0 {
				copy(m.recentRequests, m.recentRequests[first:])
				m.recentRequests = m.recentRequests[:len(m.recentRequests)-first]
			}
		}
	}
	m.mu.Unlock()
}

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

func (m *Monitor) RecordAttempt(attempt UpstreamAttempt) {
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
		*bucket = attemptBucket{minute: minute, aggregate: newAttemptAggregate()}
	}
	recordAttemptAggregate(&bucket.aggregate, attempt)
	cutoff := attempt.Time.Add(-time.Hour)
	first := 0
	for first < len(m.recentAttempts) && m.recentAttempts[first].Time.Before(cutoff) {
		first++
	}
	if first > 0 {
		copy(m.recentAttempts, m.recentAttempts[first:])
		m.recentAttempts = m.recentAttempts[:len(m.recentAttempts)-first]
	}
	m.recentAttempts = append(m.recentAttempts, attempt)
	m.mu.Unlock()
}

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

func (m *Monitor) Snapshot() MonitorSnapshot {
	nowMinute := time.Now().Unix() / 60
	window := MetricSummary{}
	var histogram [11]uint64
	endpoints, models, tiers, statuses := map[string]uint64{}, map[string]uint64{}, map[string]uint64{}, map[string]uint64{}
	series := make([]MetricSeries, 0, 60)
	usageWindow := newUsagePeriod()
	upstreamWindow := newAttemptAggregate()
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
	for _, request := range m.recentRequests {
		if !request.Time.Before(cutoff) {
			recentRequests = append(recentRequests, request)
		}
	}
	if len(recentRequests) > 500 {
		recentRequests = recentRequests[len(recentRequests)-500:]
	}
	for _, attempt := range m.recentAttempts {
		if !attempt.Time.Before(cutoff) {
			recentAttempts = append(recentAttempts, attempt)
		}
	}
	if len(recentAttempts) > 500 {
		recentAttempts = recentAttempts[len(recentAttempts)-500:]
	}
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

func newUsagePeriod() UsagePeriod {
	return UsagePeriod{Models: make(map[string]TokenCounts), Tiers: make(map[string]TokenCounts)}
}

func tokenCounts(usage bridgeUsage) TokenCounts {
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

func mergeTokenMaps(target, source map[string]TokenCounts) {
	for key, value := range source {
		addTokenMap(target, key, value)
	}
}

func cloneUsagePeriod(source UsagePeriod) UsagePeriod {
	result := newUsagePeriod()
	result.Requests, result.Reported, result.Tokens = source.Requests, source.Reported, source.Tokens
	mergeTokenMaps(result.Models, source.Models)
	mergeTokenMaps(result.Tiers, source.Tiers)
	return result
}

func finalizeUsagePeriod(period *UsagePeriod) {
	if period.Requests > 0 {
		period.Coverage = float64(period.Reported) / float64(period.Requests)
	}
}

func newAttemptAggregate() AttemptAggregate {
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
	result := newAttemptAggregate()
	mergeAttemptAggregate(&result, source)
	return result
}

func finalizeAttemptAggregate(aggregate *AttemptAggregate) {
	if aggregate.Total > 0 {
		aggregate.SuccessRate = float64(aggregate.Success) / float64(aggregate.Total)
	}
}

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

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func monitorMiddleware(monitor *Monitor, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		meta := &requestMeta{}
		r = r.WithContext(context.WithValue(r.Context(), requestMetaKey{}, meta))
		writer := &statusWriter{ResponseWriter: w}
		monitor.active.Add(1)
		defer func() {
			monitor.active.Add(-1)
			status := writer.status
			if status == 0 {
				status = http.StatusOK
			}
			duration := time.Since(started)
			monitor.Record(r.URL.Path, status, duration, meta)
			if meta.Channel != "" {
				logger.Info("request routed", "component", "http", "event", "request_routed", "method", r.Method,
					"path", r.URL.Path, "status", status, "duration_ms", duration.Milliseconds(), "request_id", meta.Request,
					"model", meta.Model, "tier", meta.Tier, "key_id", meta.KeyID, "channel", meta.Channel,
					"anonymous", meta.Anonymous, "attempts", meta.Attempts, "stream", meta.Stream)
			}
			logger.Debug("request completed", "component", "http", "event", "request_complete", "method", r.Method,
				"path", r.URL.Path, "status", status, "duration_ms", duration.Milliseconds(), "bytes", writer.bytes,
				"request_id", meta.Request, "model", meta.Model, "tier", meta.Tier, "key_id", meta.KeyID,
				"channel", meta.Channel, "anonymous", meta.Anonymous, "attempts", meta.Attempts, "stream", meta.Stream)
		}()
		next.ServeHTTP(writer, r)
	})
}

func setLogLevel(level *slog.LevelVar, value string) {
	switch value {
	case "debug":
		level.Set(slog.LevelDebug)
	case "warn":
		level.Set(slog.LevelWarn)
	case "error":
		level.Set(slog.LevelError)
	default:
		level.Set(slog.LevelInfo)
	}
}

func encodeSSE(w http.ResponseWriter, event string, id uint64, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if id > 0 {
		_, _ = fmt.Fprintf(w, "id: %d\n", id)
	}
	if event != "" {
		_, _ = fmt.Fprintf(w, "event: %s\n", event)
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}
