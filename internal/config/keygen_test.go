package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestGenerateAPIKey(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		key, err := GenerateAPIKey()
		if err != nil {
			t.Fatalf("GenerateAPIKey() error = %v", err)
		}
		const prefix = "sk-local-"
		if !strings.HasPrefix(key, prefix) {
			t.Fatalf("key %q does not use the expected prefix", key)
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(key, prefix))
		if err != nil {
			t.Fatalf("generated key is not valid raw base64url: %v", err)
		}
		if len(raw) != apiKeyRandomBytes {
			t.Fatalf("random payload length = %d, want %d", len(raw), apiKeyRandomBytes)
		}
		if _, ok := seen[key]; ok {
			t.Fatalf("duplicate key generated: %q", key)
		}
		seen[key] = struct{}{}
	}
}
