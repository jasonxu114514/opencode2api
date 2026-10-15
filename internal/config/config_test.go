package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaultsStreamContinuationTimeoutForExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{"server_keys":["local-test"],"anonymous":true,"performance":{"stream_continuation":true}}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load legacy config: %v", err)
	}
	if got := cfg.Performance.StreamContinuationTimeoutSeconds; got != 60 {
		t.Fatalf("stream_continuation_timeout_seconds = %d, want 60", got)
	}
	if got := cfg.Retry.TimeoutSeconds; got != 300 {
		t.Fatalf("retry.timeout_seconds = %d, want 300", got)
	}
	if got := cfg.Performance.StreamContinuationMax; got != 1 {
		t.Fatalf("stream_continuation_max = %d, want 1", got)
	}
}

func TestLoadStreamContinuationTimeoutOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{"server_keys":["local-test"],"anonymous":true,"performance":{"stream_continuation_timeout_seconds":90}}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if got := cfg.Performance.StreamContinuationTimeoutSeconds; got != 90 {
		t.Fatalf("stream_continuation_timeout_seconds = %d, want 90", got)
	}
}
