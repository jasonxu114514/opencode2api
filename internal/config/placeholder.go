package config

import "strings"

// placeholderKeys are the example values the shipped documentation uses. They
// are not secrets and no upstream accepts them: a configuration that still
// carries one has simply not been filled in yet.
var placeholderKeys = []string{
	"sk-your-zen-key",
	"change-this-local-key",
}

// IsPlaceholderKey reports whether key is a documented placeholder rather than
// a real credential. Comparison is case-insensitive and ignores surrounding
// whitespace.
func IsPlaceholderKey(key string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(key))
	for _, placeholder := range placeholderKeys {
		if trimmed == placeholder {
			return true
		}
	}
	return false
}

// DropPlaceholderKeys removes placeholder credentials from the key pools. A
// placeholder can never authenticate, so keeping it only turns every fallback
// that reaches the keyed tiers into a 401 — which then hides the real error
// (the cause of the failover) from the client. Callers still see the configured
// list through the management API; the pools simply behave as if the tier was
// left empty.
func DropPlaceholderKeys(cfg Config) Config {
	drop := func(keys []string) []string {
		if len(keys) == 0 {
			return keys
		}
		kept := keys[:0]
		for _, key := range keys {
			if IsPlaceholderKey(key) {
				continue
			}
			kept = append(kept, key)
		}
		return kept
	}
	cfg.ZenKeys = drop(cfg.ZenKeys)
	cfg.GoKeys = drop(cfg.GoKeys)
	return cfg
}
