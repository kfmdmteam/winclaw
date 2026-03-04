package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	appName    = "WinClaw"
	configFile = "config.json"
)

// Config holds all application configuration. API keys are never stored here;
// they live in the Windows Credential Manager.
type Config struct {
	// Model is the Anthropic model identifier to use.
	Model string `json:"model"`

	// MaxTokens is the maximum number of tokens per response.
	MaxTokens int `json:"max_tokens"`

	// DataDir is the root directory for all WinClaw data (DB, sessions, etc.).
	DataDir string `json:"data_dir"`

	// LogLevel controls verbosity: "debug", "info", "warning", "error".
	LogLevel string `json:"log_level"`

	// MaxConcurrentAgents limits how many agent goroutines run simultaneously.
	MaxConcurrentAgents int `json:"max_concurrent_agents"`

	// AgentTimeoutSeconds is the per-agent execution deadline in seconds.
	AgentTimeoutSeconds int `json:"agent_timeout_seconds"`

	// StreamResponses controls whether responses are streamed token-by-token.
	StreamResponses bool `json:"stream_responses"`

	// HistoryWindow is the number of conversation turns (user+assistant pairs)
	// to include in each API request. Older turns are dropped. This is the
	// primary lever for controlling per-request token cost.
	// Default: 20 (40 messages). Set lower for cheaper sessions.
	HistoryWindow int `json:"history_window"`
}

// defaults returns a Config populated entirely with safe default values.
func defaults() (*Config, error) {
	cfgDir, err := appConfigDir()
	if err != nil {
		return nil, fmt.Errorf("config: resolve app config dir: %w", err)
	}

	return &Config{
		Model:               "claude-sonnet-4-6",
		MaxTokens:           8192,
		DataDir:             cfgDir,
		LogLevel:            "info",
		MaxConcurrentAgents: 4,
		AgentTimeoutSeconds: 300,
		StreamResponses:     true,
		HistoryWindow:       20,
	}, nil
}

// appConfigDir returns %APPDATA%\WinClaw, creating it if absent.
func appConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: os.UserConfigDir: %w", err)
	}
	dir := filepath.Join(base, appName)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("config: mkdir %q: %w", dir, err)
	}
	return dir, nil
}

// configPath returns the full path to config.json.
func configPath() (string, error) {
	dir, err := appConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, configFile), nil
}

// Load reads the configuration from disk. If the file does not exist, the
// default configuration is returned and no error is raised. Fields missing from
// the JSON file are filled from defaults so that new options are always valid.
func Load() (*Config, error) {
	cfg, err := defaults()
	if err != nil {
		return nil, err
	}

	path, err := configPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No file yet — return defaults.
			return cfg, nil
		}
		return nil, fmt.Errorf("config: read %q: %w", path, err)
	}

	// Unmarshal into the defaults struct so zero-value JSON fields do not
	// overwrite defaults that were intentionally set above.
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %q: %w", path, err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config: validation: %w", err)
	}

	return cfg, nil
}

// Save writes cfg to disk atomically by first writing a sibling .tmp file and
// then renaming it over the target. This prevents partial writes from
// corrupting the stored configuration.
func (c *Config) Save() error {
	if err := c.validate(); err != nil {
		return fmt.Errorf("config: save validation: %w", err)
	}

	path, err := configPath()
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	// Append a trailing newline for readability.
	data = append(data, '\n')

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("config: write tmp %q: %w", tmp, err)
	}

	if err := os.Rename(tmp, path); err != nil {
		// Best-effort cleanup of the temp file.
		_ = os.Remove(tmp)
		return fmt.Errorf("config: rename %q -> %q: %w", tmp, path, err)
	}

	return nil
}

// validate checks that all fields hold sensible values.
func (c *Config) validate() error {
	if c.Model == "" {
		return errors.New("model must not be empty")
	}
	if c.MaxTokens <= 0 {
		return fmt.Errorf("max_tokens must be positive, got %d", c.MaxTokens)
	}
	if c.DataDir == "" {
		return errors.New("data_dir must not be empty")
	}
	if c.MaxConcurrentAgents <= 0 {
		return fmt.Errorf("max_concurrent_agents must be positive, got %d", c.MaxConcurrentAgents)
	}
	if c.HistoryWindow <= 0 {
		return fmt.Errorf("history_window must be positive, got %d", c.HistoryWindow)
	}
	if c.AgentTimeoutSeconds <= 0 {
		return fmt.Errorf("agent_timeout_seconds must be positive, got %d", c.AgentTimeoutSeconds)
	}
	switch c.LogLevel {
	case "debug", "info", "warning", "error":
		// valid
	default:
		return fmt.Errorf("log_level %q is not one of: debug, info, warning, error", c.LogLevel)
	}
	return nil
}
