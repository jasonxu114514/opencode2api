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

type hubHandler struct {
	base     slog.Handler
	hub      *LogHub
	redactor *config.SecretRedactor
	attrs    []slog.Attr
	groups   []string
}

// multiSink fans every log record out to stdout and, when enabled, the
// on-disk mirror. File failures are swallowed by LogFile.Write so the
// service keeps logging to the console.
type multiSink struct {
	stdout io.Writer
	file   *LogFile
}

func (m *multiSink) Write(p []byte) (int, error) {
	n, err := m.stdout.Write(p)
	if m.file != nil {
		_, _ = m.file.Write(p)
	}
	return n, err
}

func NewStructuredLogger(level *slog.LevelVar, hub *LogHub, redactor *config.SecretRedactor, file *LogFile) *slog.Logger {
	return newStructuredLogger(&multiSink{stdout: os.Stdout, file: file}, level, hub, redactor)
}

func newStructuredLogger(output io.Writer, level *slog.LevelVar, hub *LogHub, redactor *config.SecretRedactor) *slog.Logger {
	base := slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level, ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
		return sanitizeLogAttr(redactor, attr)
	}})
	return slog.New(&hubHandler{base: base, hub: hub, redactor: redactor})
}

func sanitizeLogAttr(redactor *config.SecretRedactor, attr slog.Attr) slog.Attr {
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
