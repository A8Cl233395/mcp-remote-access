package agent

import (
	"path/filepath"
	"testing"
)

func TestPersistentConfigSaveKeyOption(t *testing.T) {
	tmpDir := t.TempDir()
	configPathOverride = filepath.Join(tmpDir, "user-client.json")
	defer func() { configPathOverride = "" }()

	// 1. Initially empty
	cfg := LoadPersistentConfig()
	if cfg.UserKey != "" {
		t.Fatalf("expected empty user_key initially, got %q", cfg.UserKey)
	}

	// 2. User connects and chooses to SAVE key
	cfg.UserKey = "my-secret-key"
	if err := SavePersistentConfig(cfg); err != nil {
		t.Fatalf("failed to save config: %v", err)
	}

	loaded := LoadPersistentConfig()
	if loaded.UserKey != "my-secret-key" {
		t.Fatalf("expected saved key 'my-secret-key', got %q", loaded.UserKey)
	}

	// 3. User connects and chooses NOT to save key
	loaded.UserKey = ""
	if err := SavePersistentConfig(loaded); err != nil {
		t.Fatalf("failed to save config: %v", err)
	}

	reloaded := LoadPersistentConfig()
	if reloaded.UserKey != "" {
		t.Fatalf("expected user_key to be cleared when not saved, got %q", reloaded.UserKey)
	}
}
