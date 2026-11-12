package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPersistentConfigSaveFileOption(t *testing.T) {
	tmpDir := t.TempDir()
	configPathOverride = filepath.Join(tmpDir, "user-client.json")
	defer func() { configPathOverride = "" }()

	// 1. Initially empty: loading config must NOT create any file on disk
	cfg := LoadPersistentConfig()
	if cfg.UserKey != "" {
		t.Fatalf("expected empty user_key initially, got %q", cfg.UserKey)
	}
	if _, err := os.Stat(configPathOverride); !os.IsNotExist(err) {
		t.Fatalf("expected config file NOT to exist after LoadPersistentConfig(), but it exists: %v", err)
	}

	// 2. User chooses to SAVE file
	cfg.UserKey = "my-secret-key"
	if err := SavePersistentConfig(cfg); err != nil {
		t.Fatalf("failed to save config: %v", err)
	}

	loaded := LoadPersistentConfig()
	if loaded.UserKey != "my-secret-key" {
		t.Fatalf("expected saved key 'my-secret-key', got %q", loaded.UserKey)
	}

	// 3. User chooses NOT to save file (cancels / unchecks): file is removed
	if err := os.Remove(configPathOverride); err != nil {
		t.Fatalf("failed to remove config file: %v", err)
	}

	if _, err := os.Stat(configPathOverride); !os.IsNotExist(err) {
		t.Fatalf("expected config file to be removed from disk, but it still exists")
	}

	reloaded := LoadPersistentConfig()
	if reloaded.UserKey != "" {
		t.Fatalf("expected empty user_key after removal, got %q", reloaded.UserKey)
	}
	if _, err := os.Stat(configPathOverride); !os.IsNotExist(err) {
		t.Fatalf("reloaded LoadPersistentConfig() should not create file on disk")
	}
}
