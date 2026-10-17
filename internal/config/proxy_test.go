package config

import "testing"

func TestParseProxyEntry(t *testing.T) {
	cases := []struct {
		raw  string
		want string
		pool bool
	}{
		{"direct", "direct", false},
		{"", "", false},
		{"  http://127.0.0.1:7890  ", "http://127.0.0.1:7890", false},
		{"http://127.0.0.1:7890", "http://127.0.0.1:7890", false},
		{"http://127.0.0.1:7890#pool", "http://127.0.0.1:7890", true},
		{"http://127.0.0.1:7890#POOL", "http://127.0.0.1:7890", true},
		{"http://user:pass@127.0.0.1:7890#pool", "http://user:pass@127.0.0.1:7890", true},
		{"socks5://127.0.0.1:1080#pool", "socks5://127.0.0.1:1080", true},
		{"socks5h://127.0.0.1:1080#pool", "socks5h://127.0.0.1:1080", true},
		{"pool+http://127.0.0.1:7890", "http://127.0.0.1:7890", true},
		{"pool+socks5://127.0.0.1:1080", "socks5://127.0.0.1:1080", true},
		// A fragment that is not the pool marker belongs to the proxy URL.
		{"http://127.0.0.1:7890#other", "http://127.0.0.1:7890#other", false},
	}
	for _, tc := range cases {
		clean, pool := ParseProxyEntry(tc.raw)
		if clean != tc.want || pool != tc.pool {
			t.Errorf("ParseProxyEntry(%q) = (%q, %v), want (%q, %v)", tc.raw, clean, pool, tc.want, tc.pool)
		}
	}
}

func TestRedactURLKeepsPoolMarker(t *testing.T) {
	// url.User("***") percent-escapes the asterisks; that is the existing
	// RedactURL shape for every proxy, so the expectation mirrors it.
	const masked = "http://%2A%2A%2A@127.0.0.1:7890#pool"
	if got := RedactURL("http://user:pass@127.0.0.1:7890#pool"); got != masked {
		t.Errorf("pool URL redacted as %q, want %q", got, masked)
	}
	if got := RedactURL("pool+http://user:pass@127.0.0.1:7890"); got != masked {
		t.Errorf("pool+ URL redacted as %q, want %q", got, masked)
	}
	if got := RedactURL("http://127.0.0.1:7890"); got != "http://127.0.0.1:7890" {
		t.Errorf("static URL redacted as %q", got)
	}
}

// The same endpoint listed statically and as a pool is two distinct routes and
// must survive deduplication; an exact repeat must not.
func TestResolveProxyFilesKeepsPoolVariantOfSameEndpoint(t *testing.T) {
	cfg := Config{Proxies: []string{
		"http://127.0.0.1:7890",
		"http://127.0.0.1:7890#pool",
		"http://127.0.0.1:7890",
	}}
	if err := resolveProxyFiles("config.json", &cfg); err != nil {
		t.Fatalf("resolveProxyFiles failed: %v", err)
	}
	got := cfg.RuntimeProxies()
	want := []string{"http://127.0.0.1:7890", "http://127.0.0.1:7890#pool"}
	if len(got) != len(want) {
		t.Fatalf("resolved proxies = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resolved proxies = %q, want %q", got, want)
		}
	}
}

// A pool entry must validate against the clean URL, and its credentials must
// stay redactable after the marker is stripped.
func TestNormalizeAcceptsPoolProxy(t *testing.T) {
	cfg := Config{
		Listen:      "127.0.0.1:8080",
		ServerKeys:  []string{"local-key"},
		Anonymous:   true,
		Proxies:     []string{"http://user:secret@127.0.0.1:7890#pool", "direct"},
		Upstream:    UpstreamConfig{Zen: "https://opencode.ai/zen", Go: "https://opencode.ai/zen/go", Anonymous: "https://opencode.ai/inference"},
		Retry:       RetryConfig{MaxAttempts: 3, TimeoutSeconds: 300},
		Models:      ModelsConfig{RefreshSeconds: 300, Protocols: map[string]string{}},
		Performance: PerformanceConfig{MaxIdleConns: 10, MaxIdleConnsPerHost: 10, IdleConnTimeoutSeconds: 30, ConnectTimeoutSeconds: 5, FailureCooldownSeconds: 15},
		Logging:     LoggingConfig{Level: "info", RingSize: 100},
		Prefer:      TierGo,
	}
	normalized, err := Normalize("config.json", cfg)
	if err != nil {
		t.Fatalf("Normalize rejected a pool proxy: %v", err)
	}
	redactor := NewSecretRedactor()
	redactor.Replace(normalized)
	if got := redactor.String("dial http://user:secret@127.0.0.1:7890 failed"); got != "dial *** failed" {
		t.Errorf("pool proxy credentials survived redaction: %q", got)
	}
}
