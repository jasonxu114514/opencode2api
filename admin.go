package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const adminCookieName = "opencode2api_session"

//go:embed webui/*
var webAssets embed.FS

type adminSession struct {
	Username    string
	AuthVersion string
	CSRF        string
	Expires     time.Time
}

type loginWindow struct {
	Started time.Time
	Count   int
}

type AdminServer struct {
	manager       *RuntimeManager
	monitor       *Monitor
	logs          *LogHub
	logger        *slog.Logger
	mu            sync.Mutex
	sessions      map[string]adminSession
	attempts      map[string]loginWindow
	debugAttempts map[string]loginWindow
	lastInference *DebugInferenceResult
}

func NewAdminServer(manager *RuntimeManager, monitor *Monitor, logs *LogHub, logger *slog.Logger) *AdminServer {
	return &AdminServer{
		manager: manager, monitor: monitor, logs: logs, logger: logger, sessions: make(map[string]adminSession),
		attempts: make(map[string]loginWindow), debugAttempts: make(map[string]loginWindow),
	}
}

func (a *AdminServer) Handler() http.Handler {
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
	mux.Handle("POST /api/debug/inference", a.authenticate(a.csrf(http.HandlerFunc(a.handleDebugInference))))
	mux.Handle("GET /api/logs", a.authenticate(http.HandlerFunc(a.handleLogs)))
	mux.Handle("GET /api/logs/stream", a.authenticate(http.HandlerFunc(a.handleLogStream)))
	mux.Handle("/", a.staticHandler())
	return a.securityHeaders(recoveryMiddleware(a.logger, mux))
}

func (a *AdminServer) staticHandler() http.Handler {
	assets, err := fs.Sub(webAssets, "webui")
	if err != nil {
		panic(err)
	}
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
		files.ServeHTTP(w, r)
	})
}

func (a *AdminServer) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func (a *AdminServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	client := clientIP(r)
	if !a.allowLogin(client) {
		writeAdminError(w, http.StatusTooManyRequests, "rate_limited", "too many login attempts; try again later")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	cfg := a.manager.Config()
	usernameMatch := len(input.Username) == len(cfg.WebUI.Username) && subtle.ConstantTimeCompare([]byte(input.Username), []byte(cfg.WebUI.Username)) == 1
	passwordMatch := verifyPassword(cfg.WebUI.PasswordHash, input.Password)
	if !usernameMatch || !passwordMatch {
		a.recordLoginFailure(client)
		a.logger.Warn("admin login failed", "component", "auth", "event", "login_failed", "client_ip", client)
		writeAdminError(w, http.StatusUnauthorized, "invalid_credentials", "invalid username or password")
		return
	}
	token, err := randomToken(32)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "could not create session")
		return
	}
	csrf, err := randomToken(24)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "could not create session")
		return
	}
	expires := time.Now().Add(time.Duration(cfg.WebUI.SessionTTLMinutes) * time.Minute)
	a.mu.Lock()
	delete(a.attempts, client)
	a.cleanupSessionsLocked(time.Now())
	if len(a.sessions) >= 2048 {
		a.removeEarliestSessionLocked()
	}
	a.sessions[tokenDigest(token)] = adminSession{Username: cfg.WebUI.Username, AuthVersion: secretFingerprint(cfg.WebUI.PasswordHash), CSRF: csrf, Expires: expires}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: adminCookieName, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: int(time.Until(expires).Seconds()), Secure: requestIsSecure(r)})
	w.Header().Set("Cache-Control", "no-store")
	a.logger.Info("admin login succeeded", "component", "auth", "event", "login_succeeded", "client_ip", client)
	writeJSON(w, http.StatusOK, map[string]any{"username": cfg.WebUI.Username, "csrf_token": csrf, "expires_at": expires.UTC()})
}

func (a *AdminServer) handleSession(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "username": session.Username, "csrf_token": session.CSRF, "expires_at": session.Expires.UTC()})
}

func (a *AdminServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, _ := r.Cookie(adminCookieName)
	if cookie != nil {
		a.mu.Lock()
		delete(a.sessions, tokenDigest(cookie.Value))
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: adminCookieName, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1, Secure: requestIsSecure(r)})
	w.WriteHeader(http.StatusNoContent)
}

type sessionContextKey struct{}

func (a *AdminServer) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(adminCookieName)
		if err != nil || cookie.Value == "" {
			writeAdminError(w, http.StatusUnauthorized, "authentication_required", "login required")
			return
		}
		now := time.Now()
		a.mu.Lock()
		session, ok := a.sessions[tokenDigest(cookie.Value)]
		if ok && now.After(session.Expires) {
			delete(a.sessions, tokenDigest(cookie.Value))
			ok = false
		}
		a.mu.Unlock()
		if !ok {
			writeAdminError(w, http.StatusUnauthorized, "authentication_required", "session expired or invalid")
			return
		}
		cfg := a.manager.Config()
		if session.Username != cfg.WebUI.Username || session.AuthVersion != secretFingerprint(cfg.WebUI.PasswordHash) {
			writeAdminError(w, http.StatusUnauthorized, "authentication_required", "session is no longer valid")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), sessionContextKey{}, session))
		next.ServeHTTP(w, r)
	})
}

func sessionFromContext(r *http.Request) (adminSession, bool) {
	session, ok := r.Context().Value(sessionContextKey{}).(adminSession)
	return session, ok
}

func (a *AdminServer) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, ok := sessionFromContext(r)
		if !ok || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(session.CSRF)) != 1 {
			writeAdminError(w, http.StatusForbidden, "csrf_failed", "invalid CSRF token")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			parsed, err := url.Parse(origin)
			expectedScheme := "http"
			if requestIsSecure(r) {
				expectedScheme = "https"
			}
			if err != nil || !strings.EqualFold(parsed.Host, r.Host) || !strings.EqualFold(parsed.Scheme, expectedScheme) {
				writeAdminError(w, http.StatusForbidden, "origin_failed", "request origin does not match this server")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type SecretView struct {
	ID      string `json:"id"`
	Display string `json:"display"`
}

type ConfigView struct {
	Listen      string            `json:"listen"`
	ServerKeys  []SecretView      `json:"server_keys"`
	ZenKeys     []SecretView      `json:"zen_keys"`
	GoKeys      []SecretView      `json:"go_keys"`
	Anonymous   bool              `json:"anonymous"`
	Proxies     []SecretView      `json:"proxies"`
	ProxyFile   string            `json:"proxyfile"`
	Upstream    UpstreamConfig    `json:"upstream"`
	Retry       RetryConfig       `json:"retry"`
	Models      ModelsConfig      `json:"models"`
	Performance PerformanceConfig `json:"performance"`
	Logging     LoggingConfig     `json:"logging"`
	Prefer      Tier              `json:"prefer"`
	WebUI       WebUIView         `json:"webui"`
	Effective   EffectiveView     `json:"effective"`
	Restart     []string          `json:"restart_required_fields,omitempty"`
}

type EffectiveView struct {
	Listen       string `json:"listen"`
	WebUIListen  string `json:"webui_listen"`
	WebUIEnabled bool   `json:"webui_enabled"`
}

type WebUIView struct {
	Enabled           bool   `json:"enabled"`
	Listen            string `json:"listen"`
	Username          string `json:"username"`
	SessionTTLMinutes int    `json:"session_ttl_minutes"`
}

type SecretInput struct {
	ID    string `json:"id,omitempty"`
	Value string `json:"value,omitempty"`
}

type ConfigUpdate struct {
	Listen      string            `json:"listen"`
	ServerKeys  []SecretInput     `json:"server_keys"`
	ZenKeys     []SecretInput     `json:"zen_keys"`
	GoKeys      []SecretInput     `json:"go_keys"`
	Anonymous   bool              `json:"anonymous"`
	Proxies     []SecretInput     `json:"proxies"`
	ProxyFile   string            `json:"proxyfile"`
	Upstream    UpstreamConfig    `json:"upstream"`
	Retry       RetryConfig       `json:"retry"`
	Models      ModelsConfig      `json:"models"`
	Performance PerformanceConfig `json:"performance"`
	Logging     LoggingConfig     `json:"logging"`
	Prefer      Tier              `json:"prefer"`
	WebUI       WebUIView         `json:"webui"`
}

func (a *AdminServer) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, a.configView())
}

func (a *AdminServer) handlePutConfig(w http.ResponseWriter, r *http.Request) {
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
	candidate := Config{
		Listen: update.Listen, ServerKeys: serverKeys, ZenKeys: zenKeys, GoKeys: goKeys, Anonymous: update.Anonymous, Proxies: proxies, ProxyFile: update.ProxyFile,
		Upstream: update.Upstream, Retry: update.Retry, Models: update.Models, Performance: update.Performance, Logging: update.Logging, Prefer: update.Prefer,
		WebUI: WebUIConfig{Enabled: update.WebUI.Enabled, Listen: update.WebUI.Listen, Username: current.WebUI.Username, PasswordHash: current.WebUI.PasswordHash, SessionTTLMinutes: update.WebUI.SessionTTLMinutes},
	}
	result, err := a.manager.Apply(candidate, true)
	if err != nil {
		a.logger.Warn("configuration update rejected", "component", "config", "event", "config_rejected", "error", err)
		writeAdminError(w, http.StatusBadRequest, "configuration_rejected", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result, "config": a.configView()})
}

func (a *AdminServer) handleReload(w http.ResponseWriter, _ *http.Request) {
	result, err := a.manager.Reload()
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "reload_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *AdminServer) handleReveal(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Password string `json:"password"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	cfg := a.manager.Config()
	if !verifyPassword(cfg.WebUI.PasswordHash, input.Password) {
		writeAdminError(w, http.StatusForbidden, "verification_failed", "password verification failed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	a.logger.Info("sensitive configuration revealed", "component", "auth", "event", "secrets_revealed", "client_ip", clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"server_keys": cfg.ServerKeys, "zen_keys": cfg.ZenKeys, "go_keys": cfg.GoKeys, "proxies": cfg.Proxies})
}

func (a *AdminServer) handleAccount(w http.ResponseWriter, r *http.Request) {
	var input struct {
		CurrentPassword string `json:"current_password"`
		Username        string `json:"username"`
		NewPassword     string `json:"new_password"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	cfg := a.manager.Config()
	if !verifyPassword(cfg.WebUI.PasswordHash, input.CurrentPassword) {
		writeAdminError(w, http.StatusForbidden, "verification_failed", "current password is incorrect")
		return
	}
	if username := strings.TrimSpace(input.Username); username != "" {
		cfg.WebUI.Username = username
	}
	if input.NewPassword != "" {
		cfg.WebUI.Password = input.NewPassword
	}
	if _, err := a.manager.Apply(cfg, true); err != nil {
		writeAdminError(w, http.StatusBadRequest, "account_update_failed", err.Error())
		return
	}
	a.mu.Lock()
	a.sessions = make(map[string]adminSession)
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: adminCookieName, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1, Secure: requestIsSecure(r)})
	a.logger.Info("admin account updated", "component", "auth", "event", "account_updated", "client_ip", clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"updated": true, "reauthenticate": true})
}

func (a *AdminServer) handleMonitor(w http.ResponseWriter, _ *http.Request) {
	metrics := a.monitor.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"version": version, "metrics": metrics, "usage": metrics.Usage, "upstream": metrics.Upstream, "resources": a.manager.Resources(),
	})
}

type DebugInferenceRequest struct {
	Protocol Protocol       `json:"protocol"`
	Request  map[string]any `json:"request"`
}

type DebugInferenceResult struct {
	OK         bool                 `json:"ok"`
	HTTPStatus int                  `json:"http_status"`
	DurationMS int64                `json:"duration_ms"`
	RequestID  string               `json:"request_id,omitempty"`
	Route      ModelRouteDiagnostic `json:"route"`
	Response   any                  `json:"response"`
}

func (a *AdminServer) handleDebugModels(w http.ResponseWriter, _ *http.Request) {
	models, metadata := a.manager.DebugModels()
	a.mu.Lock()
	last := a.lastInference
	a.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"models": models, "metadata": metadata, "last_inference": last})
}

func (a *AdminServer) handleDebugInference(w http.ResponseWriter, r *http.Request) {
	if !a.allowDebug(clientIP(r)) {
		writeAdminError(w, http.StatusTooManyRequests, "debug_rate_limited", "too many Playground requests; retry in one minute")
		return
	}
	var input DebugInferenceRequest
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !validProtocol(input.Protocol) {
		writeAdminError(w, http.StatusBadRequest, "invalid_protocol", "protocol must be chat, responses, or anthropic")
		return
	}
	if input.Request == nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "request must be a JSON object")
		return
	}
	payload := cloneMap(input.Request)
	payload["stream"] = false
	model := stringAt(payload, "model")
	if model == "" {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "request.model is required")
		return
	}
	route := a.manager.DebugRoute(model, input.Protocol)
	encoded, err := json.Marshal(payload)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "request contains unsupported JSON values")
		return
	}
	path := protocolPath(input.Protocol)
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://gateway.local"+path, bytes.NewReader(encoded))
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "debug_request_failed", "could not construct Gateway request")
		return
	}
	cfg := a.manager.Config()
	if len(cfg.ServerKeys) == 0 {
		writeAdminError(w, http.StatusServiceUnavailable, "debug_unavailable", "no local server key is configured")
		return
	}
	request.Header.Set("Authorization", "Bearer "+cfg.ServerKeys[0])
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	recorder := newDebugResponseRecorder()
	started := time.Now()
	a.manager.Handler().ServeHTTP(recorder, request)
	duration := time.Since(started)
	requestID := recorder.Header().Get("x-request-id")
	if requestID != "" {
		snapshot := a.monitor.Snapshot().Upstream
		for index := len(snapshot.Requests) - 1; index >= 0; index-- {
			if snapshot.Requests[index].RequestID == requestID {
				requestRoute := snapshot.Requests[index]
				route.Anonymous = requestRoute.Anonymous
				route.Tier = Tier(requestRoute.Tier)
				route.KeyID = requestRoute.KeyID
				route.Channel = requestRoute.Channel
				route.Attempts = requestRoute.Attempts
				break
			}
		}
		if route.KeyID == "" {
			for index := len(snapshot.Recent) - 1; index >= 0; index-- {
				if snapshot.Recent[index].RequestID == requestID {
					attempt := snapshot.Recent[index]
					route.Anonymous = attempt.Anonymous
					route.Tier = Tier(attempt.Tier)
					route.KeyID = attempt.KeyID
					route.Channel = attempt.Channel
					break
				}
			}
		}
	}
	var raw any
	if json.Unmarshal(recorder.body.Bytes(), &raw) != nil {
		raw = recorder.body.String()
	}
	raw = sanitizeDebugValue(raw, a.manager.redactor)
	result := DebugInferenceResult{
		OK: recorder.status >= 200 && recorder.status < 300, HTTPStatus: recorder.status,
		DurationMS: max(duration.Milliseconds(), 0), RequestID: requestID, Route: route, Response: raw,
	}
	a.mu.Lock()
	a.lastInference = &result
	a.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}

type debugResponseRecorder struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newDebugResponseRecorder() *debugResponseRecorder {
	return &debugResponseRecorder{header: make(http.Header), status: http.StatusOK}
}

func (recorder *debugResponseRecorder) Header() http.Header { return recorder.header }

func (recorder *debugResponseRecorder) WriteHeader(status int) {
	if recorder.status != http.StatusOK || status == http.StatusOK {
		return
	}
	recorder.status = status
}

func (recorder *debugResponseRecorder) Write(data []byte) (int, error) {
	return recorder.body.Write(data)
}

func sanitizeDebugValue(value any, redactor *SecretRedactor) any {
	switch current := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(current))
		for key, item := range current {
			lower := strings.ToLower(key)
			if sensitiveDebugKey(lower) {
				result[key] = "***"
				continue
			}
			result[key] = sanitizeDebugValue(item, redactor)
		}
		return result
	case []any:
		result := make([]any, len(current))
		for index, item := range current {
			result[index] = sanitizeDebugValue(item, redactor)
		}
		return result
	case string:
		return redactor.String(current)
	default:
		return value
	}
}

func (a *AdminServer) handleLogs(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, gap := a.logs.Recent(after, limit)
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "gap": gap})
}

func (a *AdminServer) handleLogStream(w http.ResponseWriter, r *http.Request) {
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
		_ = encodeSSE(w, "gap", 0, map[string]any{"message": "older log events have expired"})
	}
	for _, event := range recent {
		if err := encodeSSE(w, "log", event.Sequence, event); err != nil {
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
				_ = encodeSSE(w, "gap", 0, map[string]any{"message": "slow client missed log events", "after": after, "next": event.Sequence})
			}
			if err := encodeSSE(w, "log", event.Sequence, event); err != nil {
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

func (a *AdminServer) sessionTokenValid(token string) bool {
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
	return session.Username == cfg.WebUI.Username && session.AuthVersion == secretFingerprint(cfg.WebUI.PasswordHash)
}

func (a *AdminServer) configView() ConfigView {
	cfg := a.manager.Config()
	effective, restart := a.manager.RestartStatus()
	return ConfigView{
		Listen: cfg.Listen, ServerKeys: maskSecrets(cfg.ServerKeys, false), ZenKeys: maskSecrets(cfg.ZenKeys, false), GoKeys: maskSecrets(cfg.GoKeys, false), Anonymous: cfg.Anonymous,
		Proxies: maskSecrets(cfg.Proxies, true), ProxyFile: cfg.ProxyFile, Upstream: cfg.Upstream, Retry: cfg.Retry, Models: cfg.Models,
		Performance: cfg.Performance, Logging: cfg.Logging, Prefer: cfg.Prefer,
		WebUI:     WebUIView{Enabled: cfg.WebUI.Enabled, Listen: cfg.WebUI.Listen, Username: cfg.WebUI.Username, SessionTTLMinutes: cfg.WebUI.SessionTTLMinutes},
		Effective: EffectiveView{Listen: effective.API, WebUIListen: effective.WebUI, WebUIEnabled: effective.WebUIEnabled},
		Restart:   restart,
	}
}

func maskSecrets(values []string, proxy bool) []SecretView {
	result := make([]SecretView, 0, len(values))
	for _, value := range values {
		display := maskValue(value)
		if proxy {
			display = redactURL(value)
		}
		result = append(result, SecretView{ID: secretFingerprint(value), Display: display})
	}
	return result
}

func maskValue(value string) string {
	runes := []rune(value)
	if len(runes) <= 5 {
		return "••••"
	}
	return "••••" + string(runes[len(runes)-5:])
}

func resolveSecrets(inputs []SecretInput, existing []string) ([]string, error) {
	known := make(map[string]string, len(existing))
	for _, value := range existing {
		known[secretFingerprint(value)] = value
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

func (a *AdminServer) allowLogin(client string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if len(a.attempts) > 1024 {
		for address, candidate := range a.attempts {
			if now.Sub(candidate.Started) > 5*time.Minute {
				delete(a.attempts, address)
			}
		}
	}
	if _, exists := a.attempts[client]; !exists && len(a.attempts) >= 4096 {
		return false
	}
	window := a.attempts[client]
	if window.Started.IsZero() || now.Sub(window.Started) > 5*time.Minute {
		return true
	}
	return window.Count < 5
}

func (a *AdminServer) recordLoginFailure(client string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	window := a.attempts[client]
	if window.Started.IsZero() || now.Sub(window.Started) > 5*time.Minute {
		window = loginWindow{Started: now}
	}
	window.Count++
	a.attempts[client] = window
}

func (a *AdminServer) allowDebug(client string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	window := a.debugAttempts[client]
	if window.Started.IsZero() || now.Sub(window.Started) >= time.Minute {
		window = loginWindow{Started: now}
	}
	if window.Count >= 12 {
		return false
	}
	window.Count++
	a.debugAttempts[client] = window
	if len(a.debugAttempts) > 4096 {
		for key, candidate := range a.debugAttempts {
			if now.Sub(candidate.Started) >= time.Minute {
				delete(a.debugAttempts, key)
			}
		}
	}
	return true
}

func (a *AdminServer) cleanupSessionsLocked(now time.Time) {
	for token, session := range a.sessions {
		if now.After(session.Expires) {
			delete(a.sessions, token)
		}
	}
}

func (a *AdminServer) removeEarliestSessionLocked() {
	var earliestToken string
	var earliest time.Time
	for token, session := range a.sessions {
		if earliestToken == "" || session.Expires.Before(earliest) {
			earliestToken, earliest = token, session.Expires
		}
	}
	if earliestToken != "" {
		delete(a.sessions, earliestToken)
	}
}

func randomToken(length int) (string, error) {
	data := make([]byte, length)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func tokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func requestIsSecure(r *http.Request) bool {
	if r == nil {
		return false
	}
	return r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

func sensitiveDebugKey(key string) bool {
	for _, hint := range []string{
		"authorization", "cookie", "password", "secret", "api_key", "api-key", "x-api-key",
		"access_token", "refresh_token", "set-cookie", "credential",
	} {
		if strings.Contains(key, hint) {
			return true
		}
	}
	return false
}

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

func writeAdminError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
