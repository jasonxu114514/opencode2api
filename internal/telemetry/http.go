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

// RequestMeta is the request-local trace that Middleware attaches to a request
// and the gateway fills in while it routes, retries and streams it. Metrics and
// the request log read it once the handler returns.
type RequestMeta struct {
	ModelAlias string
	Model      string
	Tier       string
	Protocol   protocol.Protocol
	Request    string
	// Session is the canonical upstream session (ses_...). All turns of one
	// conversation share it, so per-session token burn is visible in logs
	// and monitoring: every session draws from the same upstream IP bucket
	// on the anonymous lane.
	Session   string
	KeyID     string
	Channel   string
	Anonymous bool
	Proxy     string
	Attempts  int
	Legs      []string
	Stop      string
	Tail      string
	Stream    bool
	// ServerKeyID is the fingerprint of the local server key the caller
	// authenticated with, so usage can be split per caller. KeyID is the
	// upstream account that served the request.
	ServerKeyID string
	// Shaped marks a key-tier free-model request whose wire body was
	// normalized to agent shape (stream + required tools) like the anonymous
	// lane. Non-streaming responses for shaped requests arrive as SSE
	// and must be collapsed like anonymous ones.
	Shaped bool
	// ToolAlias holds the client's original read-related tool name (e.g.
	// "reAd" or "read_file") when the v1 lane's shaper renamed it to "read"
	// on the outbound wire. When non-empty, "read" tool calls in the
	// upstream response are rewritten back to ToolAlias before returning.
	ToolAlias      string
	Usage          protocol.Usage
	UsageReported  bool
	AttemptOutcome string
	Outcome        string
}

// PlaygroundServerKey buckets admin Playground traffic, which authenticates
// with whichever server key is enabled first and is not that caller's usage.
const PlaygroundServerKey = "playground"

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

// MetaFromRequest returns the request-local trace of r, or nil when it is
// absent.
func MetaFromRequest(r *http.Request) *RequestMeta {
	return MetaFromContext(r.Context())
}

// statusWriter records the status and body size of a response. Only the first
// WriteHeader reaches the underlying writer, and a Write without one implies
// 200.
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

// Middleware counts active requests and attaches a RequestMeta to every
// request, reusing one that is already present. When the handler returns it
// records the request in monitor and logs it: at info level when it was routed
// to an upstream channel, and always at debug level. /healthz bypasses all of
// it.
func Middleware(monitor *Metrics, logger *slog.Logger, next http.Handler) http.Handler {
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
					"model", meta.Model, "model_alias", meta.ModelAlias, "session", meta.Session, "server_key_id", meta.ServerKeyID, "tier", meta.Tier, "key_id", meta.KeyID,
					"channel", meta.Channel, "anonymous", meta.Anonymous, "attempts", meta.Attempts, "legs", strings.Join(meta.Legs, " "), "stream", meta.Stream, "outcome", outcome,
					"stop", meta.Stop, "in", meta.Usage.Input, "out", meta.Usage.Output, "reasoning_tokens", meta.Usage.Reasoning, "cached", meta.Usage.Cached, "tail", meta.Tail)
			}
			logger.Debug("request completed", "component", "http", "event", "request_complete", "method", r.Method,
				"path", r.URL.Path, "status", status, "duration_ms", duration.Milliseconds(), "bytes", writer.bytes,
				"request_id", meta.Request, "model", meta.Model, "model_alias", meta.ModelAlias, "tier", meta.Tier, "key_id", meta.KeyID,
				"channel", meta.Channel, "anonymous", meta.Anonymous, "attempts", meta.Attempts, "stream", meta.Stream, "outcome", outcome)
		}()
		next.ServeHTTP(writer, r)
	})
}
