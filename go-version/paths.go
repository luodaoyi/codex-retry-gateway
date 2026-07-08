package main

import (
	"os"
	"path/filepath"
)

const (
	defaultListenHost = "127.0.0.1"
	defaultListenPort = 4610
	defaultHealthPath = "/__codex_retry_gateway/health"
	defaultUIPath     = "/__codex_retry_gateway/ui"
)

type gatewayPaths struct {
	StateRoot     string `json:"state_root"`
	ConfigDir     string `json:"config_dir"`
	ConfigPath    string `json:"config_path"`
	LogDir        string `json:"log_dir"`
	LogPath       string `json:"log_path"`
	BackupDir     string `json:"backup_dir"`
	AnalyticsRoot string `json:"analytics_root"`
	StatePath     string `json:"state_path"`
	PIDPath       string `json:"pid_path"`
}

func defaultCodexConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "config.toml")
}

func defaultAuthPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "auth.json")
}

func defaultStateRoot() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex-retry-gateway")
}

func buildGatewayPaths(stateRoot string) gatewayPaths {
	return gatewayPaths{
		StateRoot:     stateRoot,
		ConfigDir:     filepath.Join(stateRoot, "config"),
		ConfigPath:    filepath.Join(stateRoot, "config", "config.json"),
		LogDir:        filepath.Join(stateRoot, "logs"),
		LogPath:       filepath.Join(stateRoot, "logs", "gateway.log"),
		BackupDir:     filepath.Join(stateRoot, "backups"),
		AnalyticsRoot: filepath.Join(stateRoot, "analytics"),
		StatePath:     filepath.Join(stateRoot, "state.json"),
		PIDPath:       filepath.Join(stateRoot, "gateway.pid"),
	}
}
