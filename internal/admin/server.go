// Package admin serves configuration, authentication, diagnostics, and the WebUI.
package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"opencode2api/internal/buildinfo"
	"opencode2api/internal/config"
	"opencode2api/internal/gateway"
	"opencode2api/internal/httpx"
	"opencode2api/internal/telemetry"
	"opencode2api/webui"
)

// Server serves the management API and the embedded WebUI. All mutable state
// here is guarded by mu; quotaCache has its own quotaMu because a quota
// refresh holds it across network calls.
type Server struct {
	manager       *gateway.RuntimeController
	monitor       *telemetry.Metrics
	logs          *telemetry.LogBroker
	logger        *slog.Logger
	mu            sync.Mutex
	sessions      map[string]adminSession
	attempts      map[string]loginWindow
	debugAttempts map[string]loginWindow
	lastInference *DebugInferenceResult
	quotaMu       sync.Mutex
	quotaCache    map[string]quotaCacheEntry
}

// New returns a Server for manager, reporting to monitor and logs.
func New(manager *gateway.RuntimeController, monitor *telemetry.Metrics, logs *telemetry.LogBroker, logger *slog.Logger) *Server {
	return &Server{
		manager: manager, monitor: monitor, logs: logs, logger: logger, sessions: make(map[string]adminSession),
		attempts: make(map[string]loginWindow), debugAttempts: make(map[string]loginWindow),
	}
}

// Handler returns the management routes: the API under /api/ and the embedded
// WebUI for anything else. Every API route except login requires a session,
// and every mutating one also requires a CSRF token. Unknown /api/ paths
// answer 404 as JSON, and a path that is not an embedded asset falls back to
// index.html so the single-page app can route it.
func (a *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/login", a.handleLogin)
	mux.Handle("GET /api/auth/session", a.authenticate(http.HandlerFunc(a.handleSession)))
	mux.Handle("POST /api/auth/logout", a.authenticate(a.csrf(http.HandlerFunc(a.handleLogout))))
	mux.Handle("GET /api/config", a.authenticate(http.HandlerFunc(a.handleGetConfig)))
	mux.Handle("PUT /api/config", a.authenticate(a.csrf(http.HandlerFunc(a.handlePutConfig))))
	mux.Handle("POST /api/config/reload", a.authenticate(a.csrf(http.HandlerFunc(a.handleReload))))
	mux.Handle("POST /api/config/reveal", a.authenticate(a.csrf(http.HandlerFunc(a.handleReveal))))
	mux.Handle("PUT /api/account", a.authenticate(a.csrf(http.HandlerFunc(a.handleAccount))))
	mux.Handle("GET /api/monitor", a.authenticate(http.HandlerFunc(a.handleMonitor)))
	mux.Handle("GET /api/debug/models", a.authenticate(http.HandlerFunc(a.handleDebugModels)))
	mux.Handle("GET /api/models/availability", a.authenticate(http.HandlerFunc(a.handleAvailability)))
	mux.Handle("POST /api/models/restore", a.authenticate(a.csrf(http.HandlerFunc(a.handleRestoreModel))))
	mux.Handle("POST /api/models/manual", a.authenticate(a.csrf(http.HandlerFunc(a.handleManualModel))))
	mux.Handle("POST /api/models/probe", a.authenticate(a.csrf(http.HandlerFunc(a.handleProbeModels))))
	mux.Handle("GET /api/rotation", a.authenticate(http.HandlerFunc(a.handleRotation)))
	mux.Handle("PUT /api/rotation", a.authenticate(a.csrf(http.HandlerFunc(a.handleSaveRotation))))
	mux.Handle("POST /api/rotation/current", a.authenticate(a.csrf(http.HandlerFunc(a.handleSelectRotation))))
	mux.Handle("GET /api/model-quotas", a.authenticate(http.HandlerFunc(a.handleModelQuotas)))
	mux.Handle("GET /api/api-keys", a.authenticate(http.HandlerFunc(a.handleListAPIKeys)))
	mux.Handle("POST /api/api-keys", a.authenticate(a.csrf(http.HandlerFunc(a.handleCreateAPIKey))))
	mux.Handle("PATCH /api/api-keys/{id}", a.authenticate(a.csrf(http.HandlerFunc(a.handleUpdateAPIKey))))
	mux.Handle("DELETE /api/api-keys/{id}", a.authenticate(a.csrf(http.HandlerFunc(a.handleDeleteAPIKey))))
	mux.Handle("POST /api/debug/inference", a.authenticate(a.csrf(http.HandlerFunc(a.handleDebugInference))))
	mux.Handle("GET /api/logs", a.authenticate(http.HandlerFunc(a.handleLogs)))
	mux.Handle("GET /api/logs/stream", a.authenticate(http.HandlerFunc(a.handleLogStream)))
	mux.Handle("/", a.staticHandler())
	return a.securityHeaders(telemetry.Recover(a.logger, mux))
}

// staticHandler serves the embedded WebUI assets, with the index fallback
// described on Handler.
func (a *Server) staticHandler() http.Handler {
	assets := webui.Assets
	files := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeAdminError(w, http.StatusNotFound, "not_found", "management endpoint not found")
			return
		}
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			if _, err := fs.Stat(assets, strings.TrimPrefix(r.URL.Path, "/")); err != nil {
				r.URL.Path = "/"
			}
		}
		// The UI ships inside the binary and only changes when the binary does,
		// so a browser that cached app.js/styles.css under a heuristic freshness
		// lifetime keeps running the previous build's frontend against the new
		// backend — the UI then shows stale pages (or breaks outright) until the
		// cache happens to expire or the operator hard-refreshes. Force
		// revalidation instead.
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}

// securityHeaders sets the CSP and the other hardening headers the WebUI
// relies on: no inline or third-party scripts or styles, no referrer, no
// framing and no embedding.
func (a *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; connect-src 'self'; img-src 'self' data:")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// SecretView is a configured secret as the config page shows it: the
// fingerprint to send back when the value is unchanged, and the masked display
// text.
type SecretView struct {
	ID      string `json:"id"`
	Display string `json:"display"`
}

// ConfigView is the configuration as GET /api/config returns it: every secret
// masked, plus the listeners that are actually in effect and the fields whose
// change needs a restart.
type ConfigView struct {
	Listen      string                   `json:"listen"`
	ServerKeys  []SecretView             `json:"server_keys"`
	ZenKeys     []SecretView             `json:"zen_keys"`
	GoKeys      []SecretView             `json:"go_keys"`
	Anonymous   bool                     `json:"anonymous"`
	Warp        config.WarpConfig        `json:"warp"`
	Proxies     []SecretView             `json:"proxies"`
	ProxyFile   string                   `json:"proxyfile"`
	Upstream    config.UpstreamConfig    `json:"upstream"`
	Retry       config.RetryConfig       `json:"retry"`
	Models      config.ModelsConfig      `json:"models"`
	Performance config.PerformanceConfig `json:"performance"`
	Logging     config.LoggingConfig     `json:"logging"`
	Prefer      config.Tier              `json:"prefer"`
	Reasoning   config.ReasoningConfig   `json:"reasoning"`
	WebUI       WebUIView                `json:"webui"`
	Effective   EffectiveView            `json:"effective"`
	Restart     []string                 `json:"restart_required_fields,omitempty"`
}

// EffectiveView is the set of listeners the running process actually bound,
// which may differ from the configured ones until a restart.
type EffectiveView struct {
	Listen       string `json:"listen"`
	WebUIListen  string `json:"webui_listen"`
	WebUIEnabled bool   `json:"webui_enabled"`
}

// WebUIView is the writable part of the WebUI configuration; the password and
// its hash are never exposed.
type WebUIView struct {
	Enabled           bool   `json:"enabled"`
	Listen            string `json:"listen"`
	Username          string `json:"username"`
	SessionTTLMinutes int    `json:"session_ttl_minutes"`
}

// SecretInput is one secret in a configuration update: a new Value, or the ID
// of an existing secret to keep unchanged.
type SecretInput struct {
	ID    string `json:"id,omitempty"`
	Value string `json:"value,omitempty"`
}

// ConfigUpdate is the body of PUT /api/config. Secrets carry either a new
// value or the ID of the existing one. The WebUI username and password are
// not changed here; they go through /api/account.
type ConfigUpdate struct {
	Listen      string                   `json:"listen"`
	ServerKeys  []SecretInput            `json:"server_keys"`
	ZenKeys     []SecretInput            `json:"zen_keys"`
	GoKeys      []SecretInput            `json:"go_keys"`
	Anonymous   bool                     `json:"anonymous"`
	Warp        config.WarpConfig        `json:"warp"`
	Proxies     []SecretInput            `json:"proxies"`
	ProxyFile   string                   `json:"proxyfile"`
	Upstream    config.UpstreamConfig    `json:"upstream"`
	Retry       config.RetryConfig       `json:"retry"`
	Models      config.ModelsConfig      `json:"models"`
	Performance config.PerformanceConfig `json:"performance"`
	Logging     config.LoggingConfig     `json:"logging"`
	Prefer      config.Tier              `json:"prefer"`
	// A pointer distinguishes an omitted field (legacy clients should keep the
	// current reasoning configuration) from an explicit empty object (clear it).
	Reasoning *config.ReasoningConfig `json:"reasoning,omitempty"`
	WebUI     WebUIView               `json:"webui"`
}

// handleGetConfig answers with the current configuration, secrets masked.
func (a *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, a.configView())
}

// handlePutConfig resolves the submitted secrets against the current ones,
// keeps what the update omits (rotation, server key metadata, the WebUI
// username and password hash, and reasoning when the field is absent), and
// hands the result to the runtime, which validates, applies and persists it.
func (a *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var update ConfigUpdate
	if err := decodeAdminJSON(w, r, &update); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	current := a.manager.Config()
	serverKeys, err := resolveSecrets(update.ServerKeys, current.ServerKeys)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_server_keys", err.Error())
		return
	}
	zenKeys, err := resolveSecrets(update.ZenKeys, current.ZenKeys)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_zen_keys", err.Error())
		return
	}
	goKeys, err := resolveSecrets(update.GoKeys, current.GoKeys)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_go_keys", err.Error())
		return
	}
	proxies, err := resolveSecrets(update.Proxies, current.Proxies)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_proxies", err.Error())
		return
	}
	reasoning := current.Reasoning
	if update.Reasoning != nil {
		reasoning = *update.Reasoning
	}
	candidate := config.Config{
		ServerKeyMetadata: current.ServerKeyMetadata,
		Rotation:          current.Rotation,
		Listen:            update.Listen, ServerKeys: serverKeys, ZenKeys: zenKeys, GoKeys: goKeys, Anonymous: update.Anonymous, Warp: update.Warp, Proxies: proxies, ProxyFile: update.ProxyFile,
		Upstream: update.Upstream, Retry: update.Retry, Models: update.Models, Performance: update.Performance, Logging: update.Logging, Prefer: update.Prefer,
		Reasoning: reasoning,
		WebUI:     config.WebUIConfig{Enabled: update.WebUI.Enabled, Listen: update.WebUI.Listen, Username: current.WebUI.Username, PasswordHash: current.WebUI.PasswordHash, SessionTTLMinutes: update.WebUI.SessionTTLMinutes},
	}
	result, err := a.manager.Apply(candidate, true)
	if err != nil {
		a.logger.Warn("configuration update rejected", "component", "config", "event", "config_rejected", "error", err)
		writeAdminError(w, http.StatusBadRequest, "configuration_rejected", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"result": result, "config": a.configView()})
}

// handleReload re-reads config.json and applies it, so an edit made on disk
// takes effect without a restart.
func (a *Server) handleReload(w http.ResponseWriter, _ *http.Request) {
	result, err := a.manager.Reload()
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "reload_failed", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

// handleReveal returns the plaintext server, Zen, Go and proxy secrets after
// verifying the current WebUI password, and logs that it happened.
func (a *Server) handleReveal(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Password string `json:"password"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	cfg := a.manager.Config()
	if !config.VerifyPassword(cfg.WebUI.PasswordHash, input.Password) {
		writeAdminError(w, http.StatusForbidden, "verification_failed", "password verification failed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	a.logger.Info("sensitive configuration revealed", "component", "auth", "event", "secrets_revealed", "client_ip", clientIP(r))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"server_keys": cfg.ServerKeys, "zen_keys": cfg.ZenKeys, "go_keys": cfg.GoKeys, "proxies": cfg.Proxies})
}

// handleMonitor answers with the build version, the metrics snapshot and the
// current resource counts.
func (a *Server) handleMonitor(w http.ResponseWriter, _ *http.Request) {
	metrics := a.monitor.Snapshot()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"version": buildinfo.Version, "metrics": metrics, "usage": metrics.Usage, "upstream": metrics.Upstream, "resources": a.manager.Resources(),
	})
}

// handleLogs returns the buffered log events after the "after" sequence,
// newest "limit" of them, and whether older events have already expired.
func (a *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, gap := a.logs.Recent(after, limit)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"events": events, "gap": gap})
}

// handleLogStream streams log events as server-sent events, starting after the
// Last-Event-ID header or the "after" query parameter. A gap event marks a
// cursor that has already expired, and a keepalive is sent every 20 seconds;
// a keepalive that finds the session no longer valid ends the stream, since
// nothing re-authenticates it mid-stream.
func (a *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAdminError(w, http.StatusInternalServerError, "stream_unsupported", "streaming is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	after, _ := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64)
	if queryAfter, err := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64); err == nil && queryAfter > after {
		after = queryAfter
	}
	recent, gap := a.logs.Recent(after, 2000)
	if gap {
		_ = httpx.WriteSSE(w, "gap", 0, map[string]any{"message": "older log events have expired"})
	}
	for _, event := range recent {
		if err := httpx.WriteSSE(w, "log", event.Sequence, event); err != nil {
			return
		}
		after = event.Sequence
	}
	flusher.Flush()
	stream, unsubscribe := a.logs.Subscribe()
	defer unsubscribe()
	cookie, _ := r.Cookie(adminCookieName)
	keepAlive := time.NewTicker(20 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-stream:
			if after > 0 && event.Sequence > after+1 {
				_ = httpx.WriteSSE(w, "gap", 0, map[string]any{"message": "slow client missed log events", "after": after, "next": event.Sequence})
			}
			if err := httpx.WriteSSE(w, "log", event.Sequence, event); err != nil {
				return
			}
			after = event.Sequence
			flusher.Flush()
		case <-keepAlive.C:
			if cookie == nil || !a.sessionTokenValid(cookie.Value) {
				return
			}
			_, _ = io.WriteString(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// sessionTokenValid reports whether token still names a live session that
// belongs to the current username and password hash.
func (a *Server) sessionTokenValid(token string) bool {
	a.mu.Lock()
	session, ok := a.sessions[tokenDigest(token)]
	if ok && time.Now().After(session.Expires) {
		delete(a.sessions, tokenDigest(token))
		ok = false
	}
	a.mu.Unlock()
	if !ok {
		return false
	}
	cfg := a.manager.Config()
	return session.Username == cfg.WebUI.Username && session.AuthVersion == config.Fingerprint(cfg.WebUI.PasswordHash)
}

// configView builds the masked view of the current configuration.
func (a *Server) configView() ConfigView {
	cfg := a.manager.Config()
	effective, restart := a.manager.RestartStatus()
	return ConfigView{
		Listen: cfg.Listen, ServerKeys: maskSecrets(cfg.ServerKeys, false), ZenKeys: maskSecrets(cfg.ZenKeys, false), GoKeys: maskSecrets(cfg.GoKeys, false), Anonymous: cfg.Anonymous, Warp: cfg.Warp,
		Proxies: maskSecrets(cfg.Proxies, true), ProxyFile: cfg.ProxyFile, Upstream: cfg.Upstream, Retry: cfg.Retry, Models: cfg.Models,
		Performance: cfg.Performance, Logging: cfg.Logging, Prefer: cfg.Prefer, Reasoning: cfg.Reasoning,
		WebUI:     WebUIView{Enabled: cfg.WebUI.Enabled, Listen: cfg.WebUI.Listen, Username: cfg.WebUI.Username, SessionTTLMinutes: cfg.WebUI.SessionTTLMinutes},
		Effective: EffectiveView{Listen: effective.API, WebUIListen: effective.WebUI, WebUIEnabled: effective.WebUIEnabled},
		Restart:   restart,
	}
}

// maskSecrets turns secrets into views carrying their fingerprint and either
// the masked value or, for proxies, the URL with its credentials redacted.
func maskSecrets(values []string, proxy bool) []SecretView {
	result := make([]SecretView, 0, len(values))
	for _, value := range values {
		display := config.MaskValue(value)
		if proxy {
			display = config.RedactURL(value)
		}
		result = append(result, SecretView{ID: config.Fingerprint(value), Display: display})
	}
	return result
}

// resolveSecrets rebuilds a secret list from the update: an item with a value
// supplies it, an item with only an ID keeps the existing secret it names. An
// unknown ID is an error, which is how a stale page is detected.
func resolveSecrets(inputs []SecretInput, existing []string) ([]string, error) {
	known := make(map[string]string, len(existing))
	for _, value := range existing {
		known[config.Fingerprint(value)] = value
	}
	result := make([]string, 0, len(inputs))
	for _, input := range inputs {
		switch {
		case strings.TrimSpace(input.Value) != "":
			result = append(result, strings.TrimSpace(input.Value))
		case input.ID != "":
			value, ok := known[input.ID]
			if !ok {
				return nil, fmt.Errorf("unknown or stale secret id %q", input.ID)
			}
			result = append(result, value)
		default:
			return nil, errors.New("each item must contain id or value")
		}
	}
	return result, nil
}

// decodeAdminJSON decodes one JSON value of at most 2 MiB into target,
// rejecting unknown fields and any trailing value.
func decodeAdminJSON(w http.ResponseWriter, r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

// writeAdminError answers with {"error":{"code":...,"message":...}}.
func writeAdminError(w http.ResponseWriter, status int, code, message string) {
	httpx.WriteJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
