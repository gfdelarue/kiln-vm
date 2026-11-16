package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// CLIConfig holds CLI configuration.
type CLIConfig struct {
	APIURL            string `json:"api_url"`
	Token             string `json:"token"`
	AllowInsecureHTTP bool   `json:"allow_insecure_http,omitempty"`
}

func configDir() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "kiln-vm")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "kiln-vm")
}

func configPath() string {
	return filepath.Join(configDir(), "config.json")
}

// loadSavedConfig loads only configuration persisted by kiln-vm login. Keeping this
// separate from LoadConfig prevents environment overrides from being written to
// disk when login updates one field.
func loadSavedConfig() (*CLIConfig, error) {
	cfg := &CLIConfig{}

	data, err := os.ReadFile(configPath())
	if err == nil {
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", configPath(), err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read config %s: %w", configPath(), err)
	}

	return cfg, nil
}

// LoadConfig loads persisted configuration and applies environment overrides.
func LoadConfig() (*CLIConfig, error) {
	cfg, err := loadSavedConfig()
	if err != nil {
		return nil, err
	}

	// Environment values override persisted values for this process only. An
	// environment-provided API URL does not inherit consent persisted for a
	// different URL; it needs its own explicit environment opt-in.
	if v, ok := os.LookupEnv("KILN_VM_API_URL"); ok {
		cfg.APIURL = v
		cfg.AllowInsecureHTTP = false
	}
	if v, ok := os.LookupEnv("KILN_VM_TOKEN"); ok {
		cfg.Token = v
	}
	if value, ok := os.LookupEnv("KILN_VM_ALLOW_INSECURE_HTTP"); ok {
		switch value {
		case "true":
			cfg.AllowInsecureHTTP = true
		case "false":
			cfg.AllowInsecureHTTP = false
		default:
			return nil, fmt.Errorf("KILN_VM_ALLOW_INSECURE_HTTP must be exactly true or false, got %q", value)
		}
	}

	return cfg, nil
}

// SaveConfig writes CLI configuration to disk.
func SaveConfig(cfg *CLIConfig) error {
	dir := configDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(configPath(), data, 0600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}
