package config

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ParseProxyEntry splits an optional dynamic-pool marker ("pool+" scheme prefix
// or "#pool" URL fragment) from a configured proxy string and returns the clean
// dial URL alongside whether the entry is a rotating proxy pool (such as Resin).
func ParseProxyEntry(raw string) (clean string, pool bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "direct" {
		return trimmed, false
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "pool+") {
		trimmed = strings.TrimSpace(trimmed[len("pool+"):])
		pool = true
	}
	if u, err := url.Parse(trimmed); err == nil && u.Host != "" {
		if strings.EqualFold(strings.TrimSpace(u.Fragment), "pool") {
			pool = true
			u.Fragment = ""
			trimmed = u.String()
		}
	} else if idx := strings.LastIndex(trimmed, "#"); idx >= 0 && strings.EqualFold(strings.TrimSpace(trimmed[idx+1:]), "pool") {
		pool = true
		trimmed = strings.TrimSpace(trimmed[:idx])
	}
	return trimmed, pool
}

func resolveProxyFiles(configPath string, cfg *Config) error {
	trimList(&cfg.Proxies)
	effective := append([]string(nil), cfg.Proxies...)
	if cfg.ProxyFile != "" {
		resolved := cfg.ProxyFile
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(filepath.Dir(configPath), resolved)
		}
		proxies, err := readProxyFile(resolved)
		if err != nil {
			return fmt.Errorf("load proxy file %s: %w", resolved, err)
		}
		effective = append(effective, proxies...)
	}

	effective = uniqueProxyEntries(effective)
	if len(effective) == 0 {
		effective = []string{"direct"}
	}
	cfg.effectiveProxies = effective
	return nil
}

func readProxyFile(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var proxies []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		value := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		value = strings.TrimSpace(stripProxyLineComment(value))
		if value != "" {
			proxies = append(proxies, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return proxies, nil
}

func stripProxyLineComment(line string) string {
	for i := 0; i < len(line); i++ {
		if i > 0 && line[i-1] != ' ' && line[i-1] != '\t' {
			continue
		}
		if line[i] == '#' || line[i] == ';' || (line[i] == '/' && i+1 < len(line) && line[i+1] == '/') {
			return line[:i]
		}
	}
	return line
}

// uniqueProxyEntries drops exact repeats while keeping a #pool entry next to a
// static proxy that dials the same endpoint: they are different routes (fresh
// connection, no cooldown, no health eviction), not two spellings of one.
func uniqueProxyEntries(items []string) []string {
	out := items[:0]
	seen := make(map[string]struct{}, len(items))
	pools := make(map[string]struct{}, len(items))
	for _, item := range items {
		key, pool := ParseProxyEntry(item)
		if key == "" {
			continue
		}
		if pool {
			if _, exists := pools[key]; exists {
				continue
			}
			pools[key] = struct{}{}
			out = append(out, item)
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	return out
}

func trimList(items *[]string) {
	out := (*items)[:0]
	for _, item := range *items {
		if value := strings.TrimSpace(item); value != "" {
			out = append(out, value)
		}
	}
	*items = out
}

// normalizeProxyByModel validates proxy_by_model: keys are model IDs (or "*"
// wildcard), values are non-empty pools that must be subsets of the resolved
// RuntimeProxies. Empty arrays are treated as absent. Values may be raw proxy
// URLs or their fingerprints (so the WebUI can reference credentialed
// proxies without pasting passwords); fingerprints resolve to raw URLs.
func normalizeProxyByModel(cfg *Config) error {
	if len(cfg.ProxyByModel) == 0 {
		cfg.ProxyByModel = nil
		return nil
	}
	allowed := make(map[string]struct{}, len(cfg.RuntimeProxies()))
	byFingerprint := make(map[string]string, len(cfg.RuntimeProxies()))
	for _, raw := range cfg.RuntimeProxies() {
		allowed[raw] = struct{}{}
		byFingerprint[Fingerprint(raw)] = raw
	}
	normalized := make(map[string][]string, len(cfg.ProxyByModel))
	for model, pool := range cfg.ProxyByModel {
		key := strings.TrimSpace(model)
		if key == "" {
			return fmt.Errorf("proxy_by_model must not contain an empty model ID")
		}
		cleaned := make([]string, 0, len(pool))
		seen := make(map[string]struct{}, len(pool))
		for _, raw := range pool {
			value := strings.TrimSpace(raw)
			if value == "" {
				continue
			}
			// Accept fingerprints for credentialed proxies.
			if resolved, ok := byFingerprint[value]; ok {
				value = resolved
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			if _, ok := allowed[value]; !ok {
				return fmt.Errorf("proxy_by_model[%q] references unknown proxy %q; it must be one of the configured proxies", key, RedactURL(value))
			}
			cleaned = append(cleaned, value)
		}
		if len(cleaned) == 0 {
			continue
		}
		normalized[key] = cleaned
	}
	if len(normalized) == 0 {
		cfg.ProxyByModel = nil
		return nil
	}
	cfg.ProxyByModel = normalized
	return nil
}

func normalizeProxyRetry(cfg *Config) error {
	if cfg.ProxyRetry.MaxAttempts < 0 || cfg.ProxyRetry.MaxAttempts > 10 {
		return fmt.Errorf("proxy_retry.max_attempts must be between 0 and 10 (0 uses retry.max_attempts)")
	}
	if cfg.ProxyRetry.IntervalSeconds < 0 || cfg.ProxyRetry.IntervalSeconds > 60 {
		return fmt.Errorf("proxy_retry.interval_seconds must be between 0 and 60")
	}
	return nil
}

func normalizeModelFallback(cfg *Config) error {
	if len(cfg.ModelFallback) == 0 {
		cfg.ModelFallback = nil
		return nil
	}
	normalized := make(map[string]string, len(cfg.ModelFallback))
	for model, fallback := range cfg.ModelFallback {
		key := strings.TrimSpace(model)
		target := strings.TrimSpace(fallback)
		if key == "" {
			return fmt.Errorf("model_fallback must not contain an empty model ID")
		}
		if target == "" {
			return fmt.Errorf("model_fallback[%q] must not be empty", key)
		}
		if target == key {
			return fmt.Errorf("model_fallback[%q] must not point to itself", key)
		}
		normalized[key] = target
	}
	// Detect 2-cycles eagerly; longer cycles are stopped at runtime by the
	// single-hop rule, but an explicit A<->B is almost always a typo.
	for model, target := range normalized {
		if back, ok := normalized[target]; ok && back == model {
			return fmt.Errorf("model_fallback contains a cycle between %q and %q", model, target)
		}
	}
	// Detect longer cycles via DFS.
	visited := make(map[string]int, len(normalized))
	var visit func(node string, stack map[string]bool) error
	visit = func(node string, stack map[string]bool) error {
		if visited[node] == 2 {
			return nil
		}
		if stack[node] {
			return fmt.Errorf("model_fallback contains a cycle involving %q", node)
		}
		if visited[node] == 1 {
			return nil
		}
		visited[node] = 1
		stack[node] = true
		if next, ok := normalized[node]; ok {
			if _, isKey := normalized[next]; isKey {
				if err := visit(next, stack); err != nil {
					return err
				}
			}
		}
		delete(stack, node)
		visited[node] = 2
		return nil
	}
	for model := range normalized {
		if err := visit(model, map[string]bool{}); err != nil {
			return err
		}
	}
	cfg.ModelFallback = normalized
	return nil
}

// ProxyAllowlistFor returns the allowed proxy pool for a model: exact match,
// then "*" wildcard, else the global RuntimeProxies. The result is a copy.
func (cfg Config) ProxyAllowlistFor(model string) []string {
	if pool, ok := cfg.ProxyByModel[model]; ok && len(pool) > 0 {
		return append([]string(nil), pool...)
	}
	if pool, ok := cfg.ProxyByModel["*"]; ok && len(pool) > 0 {
		return append([]string(nil), pool...)
	}
	return append([]string(nil), cfg.RuntimeProxies()...)
}

// HasCustomProxyPool reports whether a model has an explicit pool (exact or
// wildcard) that differs from the global list. Used to select the decoupled
// (key, proxy) path versus the legacy sticky-binding path.
func (cfg Config) HasCustomProxyPool(model string) bool {
	if pool, ok := cfg.ProxyByModel[model]; ok && len(pool) > 0 {
		return true
	}
	if pool, ok := cfg.ProxyByModel["*"]; ok && len(pool) > 0 {
		return true
	}
	return false
}

// EffectiveProxyAttempts resolves proxy_retry.max_attempts (0 = retry.max_attempts).
func (cfg Config) EffectiveProxyAttempts() int {
	if cfg.ProxyRetry.MaxAttempts > 0 {
		return cfg.ProxyRetry.MaxAttempts
	}
	if cfg.Retry.MaxAttempts > 0 {
		return cfg.Retry.MaxAttempts
	}
	return 3
}

// FallbackFor returns the configured fallback model for model, or "".
func (cfg Config) FallbackFor(model string) string {
	if cfg.ModelFallback == nil {
		return ""
	}
	return cfg.ModelFallback[model]
}
