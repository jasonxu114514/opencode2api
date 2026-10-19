package telemetry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
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
	attrs    []slog.Attr
	groups   []string
}

// StructuredLogger returns a logger that writes JSON to stdout and mirrors
// every record into hub. Both outputs pass through redactor, and attributes
// whose keys mention a password, authorization, cookie or secret are masked.
func StructuredLogger(level *slog.LevelVar, hub *LogBroker, redactor *config.Redactor) *slog.Logger {
	return structuredLogger(os.Stdout, level, hub, redactor)
}

// structuredLogger is StructuredLogger with an explicit JSON output.
func structuredLogger(output io.Writer, level *slog.LevelVar, hub *LogBroker, redactor *config.Redactor) *slog.Logger {
	base := slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level, ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
		return sanitizeLogAttr(redactor, attr)
	}})
	return slog.New(&hubHandler{base: base, hub: hub, redactor: redactor})
}

// sanitizeLogAttr prepares an attribute for the JSON output: a sensitive key
// masks the value, and string and error values pass through redactor.
func sanitizeLogAttr(redactor *config.Redactor, attr slog.Attr) slog.Attr {
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

// addAttr stores attr in fields under its dotted group path. A sensitive key
// masks the value; strings and values of non-scalar kinds are redacted as text.
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
