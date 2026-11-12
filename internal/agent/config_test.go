package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPersistentConfigSaveFileOption(t *testing.T) {
	tmpDir := t.TempDir()
	configPathOverride = filepath.Join(tmpDir, "mcp-remoteaccess", "user-client.json")
	defer func() { configPathOverride = "" }()

	// 1. Initially empty: loading config must NOT create any file or folder on disk
	if HasConfigFile() {
		t.Fatalf("expected HasConfigFile() to be false initially")
	}
	cfg := LoadPersistentConfig()
	if cfg.UserKey != "" {
		t.Fatalf("expected empty user_key initially, got %q", cfg.UserKey)
	}
	if HasConfigFile() {
		t.Fatalf("expected HasConfigFile() to remain false after LoadPersistentConfig()")
	}
	if _, err := os.Stat(configPathOverride); !os.IsNotExist(err) {
		t.Fatalf("expected config file NOT to exist after LoadPersistentConfig(), but it exists: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(configPathOverride)); !os.IsNotExist(err) {
		t.Fatalf("expected mcp-remoteaccess folder NOT to exist after LoadPersistentConfig(), but it exists")
	}

	// 2. User chooses to SAVE file
	cfg.UserKey = "my-secret-key"
	if err := SavePersistentConfig(cfg); err != nil {
		t.Fatalf("failed to save config: %v", err)
	}
	if !HasConfigFile() {
		t.Fatalf("expected HasConfigFile() to be true after SavePersistentConfig()")
	}

	loaded := LoadPersistentConfig()
	if loaded.UserKey != "my-secret-key" {
		t.Fatalf("expected saved key 'my-secret-key', got %q", loaded.UserKey)
	}

	// 3. User chooses NOT to save file (cancels / unchecks): both file and folder are removed
	if err := RemoveConfigFile(); err != nil {
		t.Fatalf("failed to remove config: %v", err)
	}
	if HasConfigFile() {
		t.Fatalf("expected HasConfigFile() to be false after removing config file")
	}

	if _, err := os.Stat(configPathOverride); !os.IsNotExist(err) {
		t.Fatalf("expected config file to be removed from disk, but it still exists")
	}
	if _, err := os.Stat(filepath.Dir(configPathOverride)); !os.IsNotExist(err) {
		t.Fatalf("expected mcp-remoteaccess folder to be removed from disk, but it still exists")
	}

	reloaded := LoadPersistentConfig()
	if reloaded.UserKey != "" {
		t.Fatalf("expected empty user_key after removal, got %q", reloaded.UserKey)
	}
	if _, err := os.Stat(configPathOverride); !os.IsNotExist(err) {
		t.Fatalf("reloaded LoadPersistentConfig() should not create file on disk")
	}
	if _, err := os.Stat(filepath.Dir(configPathOverride)); !os.IsNotExist(err) {
		t.Fatalf("reloaded LoadPersistentConfig() should not create folder on disk")
	}
}
