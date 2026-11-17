package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"opencode2api/internal/config"
)

// LogEvent is one structured log record as the LogBroker keeps and streams it.
// Sequence grows by one per published event, so a subscriber can detect gaps.
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

// LogBroker keeps the most recent log events in a ring buffer for the WebUI and
// fans every new event out to live subscribers.
type LogBroker struct {
	mu          sync.RWMutex
	buffer      []LogEvent
	start       int
	count       int
	next        uint64
	subscribers map[uint64]*logSubscriber
	nextSub     uint64
}

// LogHub creates a LogBroker that keeps the last capacity events, at least 100.
func LogHub(capacity int) *LogBroker {
	if capacity < 100 {
		capacity = 100
	}
	return &LogBroker{buffer: make([]LogEvent, capacity), subscribers: make(map[uint64]*logSubscriber)}
}

// Resize changes the buffer capacity (at least 100), keeping the newest events
// that still fit.
func (h *LogBroker) Resize(capacity int) {
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

// Publish assigns event the next sequence number, stores it, overwriting the
// oldest event when the buffer is full, and offers it to every subscriber.
func (h *LogBroker) Publish(event LogEvent) {
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
			// A slow browser must never block request processing. The dropped
			// event leaves a hole in the sequence numbers, which the subscriber
			// can report as a gap.
		}
	}
	h.mu.Unlock()
}

// Recent returns the buffered events with a sequence number above after, at
// most limit of the newest (the whole buffer when limit is out of range). The
// second result reports a gap: events following after have already been
// overwritten.
func (h *LogBroker) Recent(after uint64, limit int) ([]LogEvent, bool) {
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

// Subscribe returns a channel of newly published events and a function that
// ends the subscription. The channel buffers 128 events; Publish drops events
// that do not fit instead of blocking. The channel is never closed.
func (h *LogBroker) Subscribe() (<-chan LogEvent, func()) {
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

// hubHandler is the slog.Handler behind StructuredLogger. It passes each
// record to base and publishes a redacted copy to hub.
type hubHandler struct {
	base     slog.Handler
	hub      *LogBroker
	redactor *config.Redactor
	attrs    []scopedLogAttr
	groups   []string
}

// scopedLogAttr keeps the group active when WithAttrs was called; later
// WithGroup calls must not move previously bound attributes into that group.
type scopedLogAttr struct {
	attr   slog.Attr
	groups []string
}

// StructuredLogger returns a logger that writes JSON to stdout and mirrors
// every record into hub. Both outputs pass through redactor, and attributes
// whose keys mention a password, authorization, cookie or secret are masked.
func StructuredLogger(level *slog.LevelVar, hub *LogBroker, redactor *config.Redactor) *slog.Logger {
	return structuredLogger(os.Stdout, level, hub, redactor)
}

// structuredLogger is StructuredLogger with an explicit JSON output.
func structuredLogger(output io.Writer, level *slog.LevelVar, hub *LogBroker, redactor *config.Redactor) *slog.Logger {
	base := slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level})
	return slog.New(&hubHandler{base: base, hub: hub, redactor: redactor})
}

// sanitizeLogAttr applies the same policy to scalars, groups and structured
// values. Redacting only strings leaves secrets inside slog.Any untouched.
func sanitizeLogAttr(redactor *config.Redactor, attr slog.Attr) slog.Attr {
	attr.Value = attr.Value.Resolve()
	if sensitiveLogKey(attr.Key) {
		return slog.String(redactor.String(attr.Key), "***")
	}
	attr.Key = redactor.String(attr.Key)
	switch attr.Value.Kind() {
	case slog.KindString:
		attr.Value = slog.StringValue(redactor.String(attr.Value.String()))
	case slog.KindGroup:
		children := attr.Value.Group()
		sanitized := make([]slog.Attr, 0, len(children))
		for _, child := range children {
			sanitized = append(sanitized, sanitizeLogAttr(redactor, child))
		}
		attr.Value = slog.GroupValue(sanitized...)
	case slog.KindAny:
		value := attr.Value.Any()
		if value == nil {
			break
		}
		reflected := reflect.ValueOf(value)
		if reflected.Kind() == reflect.Pointer && reflected.IsNil() {
			attr.Value = slog.AnyValue(nil)
			break
		}
		if err, ok := value.(error); ok {
			attr.Value = slog.StringValue(redactor.String(err.Error()))
			break
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			attr.Value = slog.StringValue("<unsupported log value>")
			break
		}
		var decoded any
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.UseNumber()
		if err := decoder.Decode(&decoded); err != nil {
			attr.Value = slog.StringValue("<unsupported log value>")
			break
		}
		attr.Value = slog.AnyValue(sanitizeLogValue(redactor, decoded))
	}
	return attr
}

func sensitiveLogKey(key string) bool {
	key = strings.Map(func(character rune) rune {
		if character == '_' || character == '-' || character == '.' {
			return -1
		}
		return character
	}, strings.ToLower(key))
	for _, marker := range []string{"password", "authorization", "cookie", "secret", "credential", "apikey", "privatekey", "accesstoken", "refreshtoken", "authtoken", "sessiontoken", "idtoken"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return key == "token" || key == "bearer" || key == "jwt"
}

func sanitizeLogValue(redactor *config.Redactor, value any) any {
	switch current := value.(type) {
	case string:
		return redactor.String(current)
	case map[string]any:
		sanitized := make(map[string]any, len(current))
		for key, child := range current {
			if sensitiveLogKey(key) {
				sanitized[redactor.String(key)] = "***"
			} else {
				sanitized[redactor.String(key)] = sanitizeLogValue(redactor, child)
			}
		}
		return sanitized
	case []any:
		for index, child := range current {
			current[index] = sanitizeLogValue(redactor, child)
		}
	}
	return value
}

func (h *hubHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level)
}

func (h *hubHandler) Handle(ctx context.Context, record slog.Record) error {
	// Sanitize bound attributes at emission time. JSONHandler.WithAttrs would
	// cache their old masking policy across configuration and token refreshes.
	output := slog.NewRecord(record.Time, record.Level, h.redactor.String(record.Message), record.PC)
	handler := h.base
	groupDepth := 0
	fields := make(map[string]any, record.NumAttrs()+len(h.attrs))
	for _, attr := range h.attrs {
		for groupDepth < len(attr.groups) {
			handler = handler.WithGroup(h.redactor.String(attr.groups[groupDepth]))
			groupDepth++
		}
		sanitized := h.scopedAttr(attr.attr, attr.groups)
		handler = handler.WithAttrs([]slog.Attr{sanitized})
		h.addAttr(fields, sanitized, h.redactedGroups(attr.groups))
	}
	for groupDepth < len(h.groups) {
		handler = handler.WithGroup(h.redactor.String(h.groups[groupDepth]))
		groupDepth++
	}
	record.Attrs(func(attr slog.Attr) bool {
		sanitized := h.scopedAttr(attr, h.groups)
		output.AddAttrs(sanitized)
		h.addAttr(fields, sanitized, h.redactedGroups(h.groups))
		return true
	})
	if err := handler.Handle(ctx, output); err != nil {
		return err
	}
	component, _ := fields["component"].(string)
	delete(fields, "component")
	h.hub.Publish(LogEvent{
		Time:      record.Time.UTC(),
		Level:     strings.ToLower(record.Level.String()),
		Message:   output.Message,
		Component: component,
		Fields:    fields,
	})
	return nil
}

func (h *hubHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append([]scopedLogAttr(nil), h.attrs...)
	for _, attr := range attrs {
		clone.attrs = append(clone.attrs, scopedLogAttr{attr: attr, groups: append([]string(nil), h.groups...)})
	}
	return &clone
}

func (h *hubHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	clone.groups = append(append([]string(nil), h.groups...), name)
	return &clone
}

func (h *hubHandler) scopedAttr(attr slog.Attr, groups []string) slog.Attr {
	if attr.Equal(slog.Attr{}) {
		return attr
	}
	for _, group := range groups {
		if sensitiveLogKey(group) {
			return slog.String(h.redactor.String(attr.Key), "***")
		}
	}
	return sanitizeLogAttr(h.redactor, attr)
}

func (h *hubHandler) redactedGroups(groups []string) []string {
	result := make([]string, len(groups))
	for index, group := range groups {
		result[index] = h.redactor.String(group)
	}
	return result
}

// addAttr stores attr in fields under its dotted group path. A sensitive key
// masks the value. Groups are traversed so both outputs scrub nested secrets.
func (h *hubHandler) addAttr(fields map[string]any, attr slog.Attr, groups []string) {
	if attr.Equal(slog.Attr{}) {
		return
	}
	key := attr.Key
	if len(groups) > 0 {
		key = strings.Join(append(append([]string(nil), groups...), key), ".")
	}
	var value any
	switch attr.Value.Kind() {
	case slog.KindGroup:
		if attr.Key != "" {
			groups = append(append([]string(nil), groups...), attr.Key)
		}
		for _, child := range attr.Value.Group() {
			h.addAttr(fields, child, groups)
		}
		return
	case slog.KindString:
		value = attr.Value.String()
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
		value = attr.Value.Any()
	}
	fields[key] = value
}

// SetLogLevel applies a configured level name to level. Anything other than
// "debug", "warn" or "error" selects info.
func SetLogLevel(level *slog.LevelVar, value string) {
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
