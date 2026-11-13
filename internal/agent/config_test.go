package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestRestartStateHandoff(t *testing.T) {
	origEnv := os.Getenv(handoffStateEnv)
	defer func() {
		if origEnv == "" {
			_ = os.Unsetenv(handoffStateEnv)
		} else {
			_ = os.Setenv(handoffStateEnv, origEnv)
		}
	}()

	state := RestartState{
		ServerAddr: "example.com:20055",
		UserKey:    "transient-secret-key",
		MachineID:  "node-99",
		Shell:      "pwsh",
		Workdir:    `C:\Work`,
		Insecure:   true,
	}
	env := buildHandoffEnv([]string{"PATH=/usr/bin", handoffStateEnv + "=stale"}, state)

	var found string
	count := 0
	for _, entry := range env {
		if strings.HasPrefix(entry, handoffStateEnv+"=") {
			count++
			found = strings.TrimPrefix(entry, handoffStateEnv+"=")
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 %s entry, got %d", handoffStateEnv, count)
	}

	_ = os.Setenv(handoffStateEnv, found)
	got := ConsumeRestartState()
	if got == nil {
		t.Fatalf("expected non-nil RestartState")
	}
	if *got != state {
		t.Fatalf("RestartState mismatch: got %+v, want %+v", *got, state)
	}

	// Must be cleared immediately so child shells never inherit credentials
	if rem := os.Getenv(handoffStateEnv); rem != "" {
		t.Fatalf("expected %s to be unset after ConsumeRestartState, got %q", handoffStateEnv, rem)
	}
	if second := ConsumeRestartState(); second != nil {
		t.Fatalf("expected nil on second ConsumeRestartState call, got %+v", second)
	}
}

func TestPersistentConfigOnlyPersistsKeyAndMachineID(t *testing.T) {
	tmpDir := t.TempDir()
	configPathOverride = filepath.Join(tmpDir, "mcp-remoteaccess", "user-client.json")
	defer func() { configPathOverride = "" }()

	// Legacy fields on disk should be ignored and stripped on save
	if err := os.MkdirAll(filepath.Dir(configPathOverride), 0755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"server_addr":"old.example.com:20055","user_key":"k1","machine_id":"m1","shell":"bash","workdir":"/tmp"}`
	if err := os.WriteFile(configPathOverride, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}

	loaded := LoadPersistentConfig()
	if loaded.UserKey != "k1" || loaded.MachineID != "m1" {
		t.Fatalf("unexpected loaded config: %+v", loaded)
	}

	if err := SavePersistentConfig(loaded); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(configPathOverride)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 || m["user_key"] != "k1" || m["machine_id"] != "m1" {
		t.Fatalf("expected persisted JSON to contain only user_key and machine_id, got %s", string(raw))
	}
}
