package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"mcp-remoteaccess/internal/protocol"
)

type PersistentConfig struct {
	ServerAddr string `json:"server_addr"`
	UserKey    string `json:"user_key"`
	MachineID  string `json:"machine_id"`
	Shell      string `json:"shell,omitempty"`
	Workdir    string `json:"workdir,omitempty"`
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

func LoadPersistentConfig() PersistentConfig {
	var cfg PersistentConfig
	data, err := os.ReadFile(ConfigFilePath())
	if err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	if cfg.ServerAddr == "" {
		cfg.ServerAddr = protocol.DefaultServerAddr
	}
	if cfg.MachineID == "" {
		host, _ := os.Hostname()
		if host == "" {
			host = "node"
		}
		b := make([]byte, 3)
		_, _ = rand.Read(b)
		cfg.MachineID = host + "-" + hex.EncodeToString(b)
		_ = SavePersistentConfig(cfg)
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
