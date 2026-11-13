package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
)

type PersistentConfig struct {
	UserKey   string `json:"user_key"`
	MachineID string `json:"machine_id"`
}

var configPathOverride string

func ConfigFilePath() string {
	if configPathOverride != "" {
		return configPathOverride
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "mcp-remoteaccess", "user-client.json")
}

// HasConfigFile returns true if a persistent configuration file already exists on disk.
func HasConfigFile() bool {
	_, err := os.Stat(ConfigFilePath())
	return err == nil
}

// RemoveConfigFile deletes the configuration file and its parent mcp-remoteaccess folder if empty.
func RemoveConfigFile() error {
	p := ConfigFilePath()
	_ = os.Remove(p)
	dir := filepath.Dir(p)
	if filepath.Base(dir) == "mcp-remoteaccess" {
		_ = os.Remove(dir)
	}
	return nil
}

func LoadPersistentConfig() PersistentConfig {
	var cfg PersistentConfig
	data, err := os.ReadFile(ConfigFilePath())
	if err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	if cfg.MachineID == "" {
		host, _ := os.Hostname()
		if host == "" {
			host = "node"
		}
		b := make([]byte, 3)
		_, _ = rand.Read(b)
		cfg.MachineID = host + "-" + hex.EncodeToString(b)
	}
	return cfg
}

func SavePersistentConfig(cfg PersistentConfig) error {
	p := ConfigFilePath()
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0600)
}
