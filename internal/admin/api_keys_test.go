package admin

import (
	"encoding/json"
	"strings"
	"testing"

	"opencode2api/internal/config"
)

func TestManagedKeyLifecycle(t *testing.T) {
	base := "sk-existing-1234567890"
	cfg := config.Config{ServerKeys: []string{base}}
	key := "sk-client-12345678901234"
	if err := addAPIKey(&cfg, "笔记助手", key); err != nil {
		t.Fatal(err)
	}
	if err := addAPIKey(&cfg, "重复", key); err == nil {
		t.Fatal("duplicate accepted")
	}
	views, _ := json.Marshal(apiKeyViews(cfg))
	if strings.Contains(string(views), key) || strings.Contains(string(views), base) {
		t.Fatal("list leaked secret")
	}
	id := config.Fingerprint(key)
	enabled := false
	candidate := config.Clone(cfg)
	if err := changeAPIKey(&candidate, id, nil, &enabled, false); err != nil {
		t.Fatal(err)
	}
	if candidate.ServerKeyEnabled(key) || !cfg.ServerKeyEnabled(key) {
		t.Fatal("clone metadata aliases source")
	}
	cfg = candidate
	name := "新的名称"
	if err := changeAPIKey(&cfg, id, &name, nil, false); err != nil {
		t.Fatal(err)
	}
	if cfg.ServerKeyMetadata[id].Name != name {
		t.Fatal("rename not applied")
	}
	if err := changeAPIKey(&cfg, config.Fingerprint(base), nil, &enabled, false); err == nil {
		t.Fatal("last active key disabled")
	}
	// Failed mutations are applied only to a clone by RuntimeManager.Update.
	cfg = config.Config{ServerKeys: []string{base, key}}
	if err := changeAPIKey(&cfg, id, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if len(cfg.ServerKeys) != 1 || cfg.ServerKeys[0] != base {
		t.Fatal("wrong key deleted")
	}
	if err := changeAPIKey(&cfg, config.Fingerprint(base), nil, nil, true); err == nil {
		t.Fatal("last key deleted")
	}
}

func TestCustomKeyValidation(t *testing.T) {
	for _, key := range []string{"short", "long-key with-space-123", "含中文的密钥12345678901234567890", "secret\n12345678901234567890"} {
		cfg := config.Config{}
		if addAPIKey(&cfg, "test", key) == nil {
			t.Errorf("invalid key accepted")
		}
	}
}
