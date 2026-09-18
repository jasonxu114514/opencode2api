// Package telemetry records request outcomes, metrics, and structured logs.
package telemetry

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"opencode2api/internal/protocol"
)

type RequestMeta struct {
	Model     string
	Tier      string
	Protocol  protocol.Protocol
	Request   string
	KeyID     string
	Channel   string
	Anonymous bool
	Proxy     string
	Attempts  int
	Legs      []string
	Stream    bool
	Stop      string
	Tail      string
	// Shaped marks a key-tier free-model request whose wire body was
	// normalized to agent shape (stream + core tools) like the anonymous
	// lane. Non-streaming responses for shaped requests arrive as SSE
	// and must be collapsed like anonymous ones.
	Shaped         bool
	Usage          protocol.Usage
	UsageReported  bool
	AttemptOutcome string
	Outcome        string
}

type requestMetaKey struct{}

// WithRequestMeta shares request-local outcomes across HTTP and gateway layers.
func WithRequestMeta(ctx context.Context, meta *RequestMeta) context.Context {
	return context.WithValue(ctx, requestMetaKey{}, meta)
}

// MetaFromContext returns the request-local trace, or nil when it is absent.
func MetaFromContext(ctx context.Context) *RequestMeta {
	meta, _ := ctx.Value(requestMetaKey{}).(*RequestMeta)
	return meta
}

func MetaFromRequest(r *http.Request) *RequestMeta {
	return MetaFromContext(r.Context())
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

func Middleware(monitor *Monitor, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		meta := MetaFromRequest(r)
		if meta == nil {
			meta = &RequestMeta{}
			r = r.WithContext(WithRequestMeta(r.Context(), meta))
		}
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
			outcome := meta.Outcome
			if outcome == "" {
				outcome = requestOutcome(status, meta.Channel)
			}
			if meta.Channel != "" {
				logger.Info("request routed", "component", "http", "event", "request_routed", "method", r.Method,
					"path", r.URL.Path, "status", status, "duration_ms", duration.Milliseconds(), "request_id", meta.Request,
					"model", meta.Model, "tier", meta.Tier, "key_id", meta.KeyID, "channel", meta.Channel,
					"anonymous", meta.Anonymous, "attempts", meta.Attempts, "legs", strings.Join(meta.Legs, " "), "stream", meta.Stream, "outcome", outcome,
					"stop", meta.Stop, "in", meta.Usage.Input, "out", meta.Usage.Output, "reasoning_tokens", meta.Usage.Reasoning, "cached", meta.Usage.Cached, "tail", meta.Tail)
			}
			logger.Debug("request completed", "component", "http", "event", "request_complete", "method", r.Method,
				"path", r.URL.Path, "status", status, "duration_ms", duration.Milliseconds(), "bytes", writer.bytes,
				"request_id", meta.Request, "model", meta.Model, "tier", meta.Tier, "key_id", meta.KeyID,
				"channel", meta.Channel, "anonymous", meta.Anonymous, "attempts", meta.Attempts, "legs", strings.Join(meta.Legs, " "), "stream", meta.Stream, "outcome", outcome,
				"stop", meta.Stop, "in", meta.Usage.Input, "out", meta.Usage.Output, "reasoning_tokens", meta.Usage.Reasoning, "cached", meta.Usage.Cached, "tail", meta.Tail)
		}()
		next.ServeHTTP(writer, r)
	})
}
