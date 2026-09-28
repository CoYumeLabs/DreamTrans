package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func resetConfigForTest() {
	mu.Lock()
	current = Config{}
	path = ""
	sharedDB = nil
	sharedRevision++
	sharedRefreshing = false
	sharedWriting = false
	sharedRefreshAfter = time.Time{}
	mu.Unlock()
}

func TestLoadCreatesPrivateDefaultConfig(t *testing.T) {
	resetConfigForTest()
	configPath := filepath.Join(t.TempDir(), "nested", "dreamtrans.json")
	t.Setenv("DREAMTRANS_CONFIG_PATH", configPath)

	if err := Load(); err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat created config: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}
	if cfg := Get(); cfg.Models.Chat == "" || cfg.Prompts.Translate == "" {
		t.Fatalf("defaults were not populated: %#v", cfg)
	}
}

func TestUpdatePersistsAllSummaryThresholds(t *testing.T) {
	resetConfigForTest()
	configPath := filepath.Join(t.TempDir(), "dreamtrans.json")
	t.Setenv("DREAMTRANS_CONFIG_PATH", configPath)
	if err := Load(); err != nil {
		t.Fatalf("load defaults: %v", err)
	}

	partial := &Config{}
	partial.Summary.MaxLines = 42
	partial.Summary.ParMinChars = 321
	if err := Update(partial); err != nil {
		t.Fatalf("update: %v", err)
	}

	payload, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var persisted Config
	if err := json.Unmarshal(payload, &persisted); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if persisted.Summary.MaxLines != 42 || persisted.Summary.ParMinChars != 321 {
		t.Fatalf("summary thresholds were not persisted: %#v", persisted.Summary)
	}
}
