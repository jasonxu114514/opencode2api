package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func sampleConfig() Config {
	return Config{
		Listen:     "127.0.0.1:8080",
		ServerKeys: []string{"sk-local-test-key-1"},
		ZenKeys:    []string{"zen-cred-alpha-12345", "zen-cred-beta-67890"},
		GoKeys:     []string{"go-cred-gamma-11111"},
		Proxies:    []string{"http://127.0.0.1:18090", "http://127.0.0.1:18091", "direct"},
		Upstream: UpstreamConfig{
			Zen:       "https://opencode.ai/zen",
			Go:        "https://opencode.ai/go",
			Anonymous: "https://opencode.ai/inference",
		},
		Retry: RetryConfig{
			MaxAttempts:    3,
			TimeoutSeconds: 30,
		},
		Models: ModelsConfig{
			RefreshSeconds: 300,
		},
		Performance: PerformanceConfig{
			MaxIdleConns:           10,
			MaxIdleConnsPerHost:    5,
			IdleConnTimeoutSeconds: 30,
			ConnectTimeoutSeconds:  5,
			FailureCooldownSeconds: 15,
		},
		Logging: LoggingConfig{
			Level:    "info",
			RingSize: 1000,
		},
		Prefer: TierGo,
	}
}

func TestNormalizeProxyBindings_Valid(t *testing.T) {
	cfg := sampleConfig()
	zenAlphaFP := Fingerprint(cfg.ZenKeys[0])
	cfg.ProxyBindings = map[string]string{
		zenAlphaFP: "http://127.0.0.1:18090",
		// Key by raw value, should be canonicalized to its fingerprint
		"zen-cred-beta-67890": "http://127.0.0.1:18091",
		// Go key bound to direct
		"go-cred-gamma-11111": "direct",
	}

	norm, err := Normalize("", cfg)
	if err != nil {
		t.Fatalf("Normalize failed: %v", err)
	}

	zenBetaFP := Fingerprint("zen-cred-beta-67890")
	goGammaFP := Fingerprint("go-cred-gamma-11111")

	if got := norm.ProxyBindings[zenAlphaFP]; got != "http://127.0.0.1:18090" {
		t.Errorf("zenAlpha proxy: got %q, want %q", got, "http://127.0.0.1:18090")
	}
	if got := norm.ProxyBindings[zenBetaFP]; got != "http://127.0.0.1:18091" {
		t.Errorf("zenBeta proxy: got %q, want %q", got, "http://127.0.0.1:18091")
	}
	if got := norm.ProxyBindings[goGammaFP]; got != "direct" {
		t.Errorf("goGamma proxy: got %q, want %q", got, "direct")
	}
	// Raw key string must not be kept in the map
	if _, exists := norm.ProxyBindings["zen-cred-beta-67890"]; exists {
		t.Errorf("raw secret key was kept in ProxyBindings map")
	}
}

func TestNormalizeProxyBindings_UnknownKeyRejected(t *testing.T) {
	cfg := sampleConfig()
	cfg.ProxyBindings = map[string]string{
		"unknown-secret-key": "http://127.0.0.1:18090",
	}

	_, err := Normalize("", cfg)
	if err == nil {
		t.Fatalf("expected error for unknown key, got nil")
	}
	if !strings.Contains(err.Error(), "proxy_bindings contains reference to unknown key") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestNormalizeProxyBindings_UnknownProxyRejected(t *testing.T) {
	cfg := sampleConfig()
	cfg.ProxyBindings = map[string]string{
		"zen-cred-alpha-12345": "http://192.0.2.1:9999",
	}

	_, err := Normalize("", cfg)
	if err == nil {
		t.Fatalf("expected error for unknown proxy, got nil")
	}
	if !strings.Contains(err.Error(), "proxy_bindings references unknown proxy") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestNormalizeProxyBindings_SecretNotExposedInJSON(t *testing.T) {
	cfg := sampleConfig()
	cfg.ProxyBindings = map[string]string{
		"zen-cred-alpha-12345": "http://127.0.0.1:18090",
	}
	norm, err := Normalize("", cfg)
	if err != nil {
		t.Fatalf("Normalize failed: %v", err)
	}

	data, err := json.Marshal(norm)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	jsonStr := string(data)
	if strings.Contains(jsonStr, "zen-cred-alpha-12345") {
		// Only zen_keys array should contain the secret, proxy_bindings must not!
		type configCheck struct {
			ProxyBindings map[string]string `json:"proxy_bindings"`
		}
		var check configCheck
		if err := json.Unmarshal(data, &check); err != nil {
			t.Fatalf("Unmarshal check failed: %v", err)
		}
		for k := range check.ProxyBindings {
			if strings.Contains(k, "zen-cred") {
				t.Errorf("raw secret leaked into serialized proxy_bindings key: %q", k)
			}
		}
	}
}

func TestNormalizeProxyBindings_ReorderingKeysPreservesBindings(t *testing.T) {
	cfg := sampleConfig()
	cfg.ProxyBindings = map[string]string{
		"zen-cred-alpha-12345": "http://127.0.0.1:18090",
		"zen-cred-beta-67890":  "http://127.0.0.1:18091",
	}

	norm1, err := Normalize("", cfg)
	if err != nil {
		t.Fatalf("Normalize 1 failed: %v", err)
	}

	// Reverse zen keys order
	cfgReordered := sampleConfig()
	cfgReordered.ZenKeys = []string{"zen-cred-beta-67890", "zen-cred-alpha-12345"}
	cfgReordered.ProxyBindings = norm1.ProxyBindings

	norm2, err := Normalize("", cfgReordered)
	if err != nil {
		t.Fatalf("Normalize 2 failed: %v", err)
	}

	fpAlpha := Fingerprint("zen-cred-alpha-12345")
	fpBeta := Fingerprint("zen-cred-beta-67890")

	if norm2.ProxyBindings[fpAlpha] != norm1.ProxyBindings[fpAlpha] {
		t.Errorf("alpha binding shifted after key reordering")
	}
	if norm2.ProxyBindings[fpBeta] != norm1.ProxyBindings[fpBeta] {
		t.Errorf("beta binding shifted after key reordering")
	}
}

func TestClone_PreservesProxyBindings(t *testing.T) {
	cfg := sampleConfig()
	cfg.ProxyBindings = map[string]string{
		"fp-1": "http://127.0.0.1:18090",
	}
	cloned := Clone(cfg)
	if cloned.ProxyBindings["fp-1"] != "http://127.0.0.1:18090" {
		t.Errorf("Clone lost proxy bindings")
	}
	cloned.ProxyBindings["fp-2"] = "direct"
	if _, exists := cfg.ProxyBindings["fp-2"]; exists {
		t.Errorf("mutation of cloned map leaked to original")
	}
}
