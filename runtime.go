package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type gatewayRuntime struct {
	config  Config
	gateway *Gateway
	handler http.Handler
	cancel  context.CancelFunc
}

type ApplyResult struct {
	Applied         bool     `json:"applied"`
	RestartRequired bool     `json:"restart_required"`
	RestartFields   []string `json:"restart_fields,omitempty"`
}

type RuntimeManager struct {
	configPath string
	root       context.Context
	logger     *slog.Logger
	monitor    *Monitor
	hub        *LogHub
	redactor   *SecretRedactor
	level      *slog.LevelVar
	current    atomic.Pointer[gatewayRuntime]
	updateMu   sync.Mutex
	effective  effectiveListeners
	metadata   *modelMetadataStore
}

type effectiveListeners struct {
	API          string
	WebUI        string
	WebUIEnabled bool
}

func NewRuntimeManager(root context.Context, configPath string, cfg Config, logger *slog.Logger, monitor *Monitor, hub *LogHub, redactor *SecretRedactor, level *slog.LevelVar) (*RuntimeManager, error) {
	manager := &RuntimeManager{
		configPath: configPath, root: root, logger: logger, monitor: monitor, hub: hub, redactor: redactor, level: level,
		effective: effectiveListeners{API: cfg.Listen, WebUI: cfg.WebUI.Listen, WebUIEnabled: cfg.WebUI.Enabled},
	}
	manager.metadata = newModelMetadataStore(configPath, logger)
	// models.dev refreshes ride the active runtime's healthy proxy transports
	// and fall back to the store's direct client when none are available.
	manager.metadata.SetClientProvider(func() []*http.Client {
		runtime := manager.current.Load()
		if runtime == nil || runtime.gateway == nil || runtime.gateway.transports == nil {
			return nil
		}
		clients := make([]*http.Client, 0, len(runtime.gateway.transports.items))
		for _, proxy := range runtime.gateway.transports.items {
			if proxy != nil && proxy.healthy.Load() {
				clients = append(clients, proxy.client)
			}
		}
		return clients
	})
	if cfg.WebUI.Password != "" {
		hash, err := hashPassword(cfg.WebUI.Password)
		if err != nil {
			return nil, fmt.Errorf("hash webui password: %w", err)
		}
		cfg.WebUI.PasswordHash = hash
		cfg.WebUI.Password = ""
		if err := SaveConfigAtomic(configPath, cfg); err != nil {
			return nil, fmt.Errorf("persist webui password migration: %w", err)
		}
		// The automatically-created backup contains the one-time plaintext
		// bootstrap password and must not be retained.
		_ = os.Remove(configPath + ".bak")
	}
	runtime, err := manager.build(cfg)
	if err != nil {
		return nil, err
	}
	manager.current.Store(runtime)
	manager.redactor.Replace(cfg)
	setLogLevel(manager.level, cfg.Logging.Level)
	manager.start(runtime)
	manager.metadata.Start(root)
	return manager, nil
}

func (m *RuntimeManager) build(cfg Config) (*gatewayRuntime, error) {
	gateway, err := NewGateway(cfg, m.logger, m.monitor)
	if err != nil {
		return nil, err
	}
	gateway.catalog.metadata = m.metadata
	return &gatewayRuntime{config: cfg, gateway: gateway, handler: gateway.Handler(), cancel: func() {}}, nil
}

func (m *RuntimeManager) start(runtime *gatewayRuntime) {
	// Store the context through a lightweight wrapper so cancel stops catalog
	// and proxy checks while in-flight HTTP requests continue on the old pools.
	runtimeCtx, cancel := context.WithCancel(m.root)
	runtime.cancel = cancel
	runtime.gateway.StartProxyHealthChecks(runtimeCtx)
	runtime.gateway.StartModelRefresh(runtimeCtx)
}

func (m *RuntimeManager) Handler() http.Handler {
	dynamic := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runtime := m.current.Load()
		if runtime == nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		runtime.handler.ServeHTTP(w, r)
	})
	return monitorMiddleware(m.monitor, m.logger, dynamic)
}

func (m *RuntimeManager) Config() Config {
	runtime := m.current.Load()
	if runtime == nil {
		return Config{}
	}
	return cloneConfig(runtime.config)
}

func (m *RuntimeManager) RestartStatus() (effectiveListeners, []string) {
	cfg := m.Config()
	fields := make([]string, 0, 3)
	if cfg.Listen != m.effective.API {
		fields = append(fields, "listen")
	}
	if cfg.WebUI.Listen != m.effective.WebUI {
		fields = append(fields, "webui.listen")
	}
	if cfg.WebUI.Enabled != m.effective.WebUIEnabled {
		fields = append(fields, "webui.enabled")
	}
	return m.effective, fields
}

func cloneConfig(cfg Config) Config {
	cfg.ServerKeys = append([]string(nil), cfg.ServerKeys...)
	cfg.ZenKeys = append([]string(nil), cfg.ZenKeys...)
	cfg.GoKeys = append([]string(nil), cfg.GoKeys...)
	cfg.Proxies = append([]string(nil), cfg.Proxies...)
	cfg.effectiveProxies = append([]string(nil), cfg.effectiveProxies...)
	if cfg.Models.Protocols != nil {
		protocols := make(map[string]string, len(cfg.Models.Protocols))
		for key, value := range cfg.Models.Protocols {
			protocols[key] = value
		}
		cfg.Models.Protocols = protocols
	}
	return cfg
}

func (m *RuntimeManager) Apply(candidate Config, persist bool) (ApplyResult, error) {
	m.updateMu.Lock()
	defer m.updateMu.Unlock()

	current := m.current.Load()
	hadPlaintextPassword := candidate.WebUI.Password != ""
	if hadPlaintextPassword {
		hash, err := hashPassword(candidate.WebUI.Password)
		if err != nil {
			return ApplyResult{}, err
		}
		candidate.WebUI.PasswordHash = hash
		candidate.WebUI.Password = ""
	}
	normalized, err := NormalizeConfig(m.configPath, candidate)
	if err != nil {
		return ApplyResult{}, err
	}
	next, err := m.build(normalized)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("initialize runtime: %w", err)
	}
	if current != nil {
		next.gateway.catalog.CopyState(current.gateway.catalog)
	}
	if persist || hadPlaintextPassword {
		if err := SaveConfigAtomic(m.configPath, normalized); err != nil {
			next.cancel()
			return ApplyResult{}, err
		}
	}

	result := ApplyResult{Applied: true}
	if normalized.Listen != m.effective.API {
		result.RestartFields = append(result.RestartFields, "listen")
	}
	if normalized.WebUI.Listen != m.effective.WebUI {
		result.RestartFields = append(result.RestartFields, "webui.listen")
	}
	if normalized.WebUI.Enabled != m.effective.WebUIEnabled {
		result.RestartFields = append(result.RestartFields, "webui.enabled")
	}
	result.RestartRequired = len(result.RestartFields) > 0
	m.redactor.Replace(normalized)
	setLogLevel(m.level, normalized.Logging.Level)
	if current == nil || normalized.Logging.RingSize != current.config.Logging.RingSize {
		m.hub.Resize(normalized.Logging.RingSize)
	}
	m.start(next)
	previous := m.current.Swap(next)
	if previous != nil {
		previous.cancel()
	}
	m.logger.Info("configuration applied", "component", "config", "event", "config_applied", "restart_required", result.RestartRequired, "restart_fields", result.RestartFields)
	return result, nil
}

func (m *RuntimeManager) Reload() (ApplyResult, error) {
	cfg, err := LoadConfig(m.configPath)
	if err != nil {
		return ApplyResult{}, err
	}
	hadPlaintextPassword := cfg.WebUI.Password != ""
	result, err := m.Apply(cfg, false)
	if err == nil && hadPlaintextPassword {
		_ = os.Remove(m.configPath + ".bak")
	}
	return result, err
}

func (m *RuntimeManager) Shutdown() {
	if current := m.current.Load(); current != nil {
		current.cancel()
	}
}

type ResourceSnapshot struct {
	Models    modelCatalogSnapshot `json:"models"`
	Keys      []KeyStatus          `json:"keys"`
	Proxies   []ProxyStatus        `json:"proxies"`
	Anonymous bool                 `json:"anonymous"`
	Metadata  MetadataSnapshot     `json:"metadata"`
}

type KeyStatus struct {
	ID            string     `json:"id"`
	Tier          string     `json:"tier"`
	Index         int        `json:"index"`
	ProxyIndex    int        `json:"proxy_index"`
	Failures      uint32     `json:"failures"`
	CooldownUntil *time.Time `json:"cooldown_until,omitempty"`
}

type ProxyStatus struct {
	Index     int    `json:"index"`
	Address   string `json:"address"`
	Healthy   bool   `json:"healthy"`
	Checking  bool   `json:"checking"`
	ZenKeys   int    `json:"zen_keys"`
	GoKeys    int    `json:"go_keys"`
	Anonymous bool   `json:"anonymous"`
}

func (m *RuntimeManager) Resources() ResourceSnapshot {
	runtime := m.current.Load()
	if runtime == nil {
		return ResourceSnapshot{}
	}
	gateway := runtime.gateway
	result := ResourceSnapshot{Models: gateway.catalog.Snapshot(), Anonymous: gateway.cfg.Anonymous}
	if gateway.catalog.metadata != nil {
		result.Metadata = gateway.catalog.metadata.Snapshot()
	}
	result.Keys = append(result.Keys, keyStatuses("zen", gateway.zenNodes)...)
	result.Keys = append(result.Keys, keyStatuses("go", gateway.goNodes)...)
	gateway.zenNodes.bindingsMu.Lock()
	zenBindings := append([]int(nil), gateway.zenNodes.bindingCount...)
	gateway.zenNodes.bindingsMu.Unlock()
	gateway.goNodes.bindingsMu.Lock()
	goBindings := append([]int(nil), gateway.goNodes.bindingCount...)
	gateway.goNodes.bindingsMu.Unlock()
	for _, proxy := range gateway.transports.items {
		status := ProxyStatus{Index: proxy.index, Address: redactURL(proxy.name), Healthy: proxy.healthy.Load(), Checking: proxy.checking.Load(), Anonymous: gateway.cfg.Anonymous}
		if proxy.index < len(zenBindings) {
			status.ZenKeys = zenBindings[proxy.index]
		}
		if proxy.index < len(goBindings) {
			status.GoKeys = goBindings[proxy.index]
		}
		result.Proxies = append(result.Proxies, status)
	}
	return result
}

func (m *RuntimeManager) DebugModels() ([]ModelRouteDiagnostic, MetadataSnapshot) {
	runtime := m.current.Load()
	if runtime == nil {
		return nil, MetadataSnapshot{}
	}
	gateway := runtime.gateway
	models := gateway.catalog.List()
	result := make([]ModelRouteDiagnostic, 0, len(models))
	for _, model := range models {
		result = append(result, gateway.catalog.Diagnostic(model, "", len(gateway.cfg.ZenKeys) > 0, len(gateway.cfg.GoKeys) > 0, gateway.cfg.Anonymous))
	}
	metadata := MetadataSnapshot{}
	if gateway.catalog.metadata != nil {
		metadata = gateway.catalog.metadata.Snapshot()
	}
	return result, metadata
}

func (m *RuntimeManager) DebugRoute(model string, requested Protocol) ModelRouteDiagnostic {
	runtime := m.current.Load()
	if runtime == nil {
		return ModelRouteDiagnostic{Model: model, RequestedProtocol: requested, RouteError: "gateway runtime is unavailable"}
	}
	gateway := runtime.gateway
	return gateway.catalog.Diagnostic(model, requested, len(gateway.cfg.ZenKeys) > 0, len(gateway.cfg.GoKeys) > 0, gateway.cfg.Anonymous)
}

func keyStatuses(tier string, pool *nodePool) []KeyStatus {
	result := make([]KeyStatus, 0, len(pool.nodes))
	for _, node := range pool.nodes {
		status := KeyStatus{ID: keyDisplayID(node.key), Tier: tier, Index: node.index, ProxyIndex: int(node.proxyIndex.Load()), Failures: node.failures.Load()}
		if until := node.cooldownUntil.Load(); until > time.Now().UnixNano() {
			value := time.Unix(0, until).UTC()
			status.CooldownUntil = &value
		}
		result = append(result, status)
	}
	return result
}

func secretFingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:10]
}

// keyDisplayID is intentionally separate from secretFingerprint. The latter
// is an internal stable identifier used by session/config bookkeeping; this
// value is safe for logs and the operator UI and shows only the key suffix.
func keyDisplayID(value string) string {
	runes := []rune(value)
	if len(runes) <= 5 {
		return string(runes)
	}
	return string(runes[len(runes)-5:])
}
