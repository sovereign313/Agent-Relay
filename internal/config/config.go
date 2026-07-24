package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	defaultDiscoveryDepth = 5
	defaultQueueSize      = 5
	defaultMaxMessageSize = 32 * 1024
	defaultTaskTimeout    = 2 * time.Hour
	defaultLogMaxBytes    = 10 * 1024 * 1024
	defaultLogBackups     = 3
)

type Config struct {
	TelegramToken         string                 `toml:"telegram_token"`
	TelegramTokenEnv      string                 `toml:"telegram_token_env"`
	AllowedUserIDs        []int64                `toml:"allowed_user_ids"`
	PrivateChatsOnly      *bool                  `toml:"private_chats_only"`
	ProjectRoots          []string               `toml:"project_roots"`
	ProjectAliases        map[string]string      `toml:"project_aliases"`
	ProjectDiscoveryDepth int                    `toml:"project_discovery_depth"`
	DefaultAgent          string                 `toml:"default_agent"`
	Agents                map[string]AgentConfig `toml:"agents"`
	StateFile             string                 `toml:"state_file"`
	LogFile               string                 `toml:"log_file"`
	LogMaxBytes           int64                  `toml:"log_max_bytes"`
	LogBackups            int                    `toml:"log_backups"`
	QueueSize             int                    `toml:"queue_size"`
	MaxMessageBytes       int                    `toml:"max_message_bytes"`
	TaskTimeout           string                 `toml:"task_timeout"`
	TelegramAPIBase       string                 `toml:"telegram_api_base"`
	Codex                 AgentConfig            `toml:"codex"`

	taskTimeout time.Duration
	sourceDir   string
}

type AgentConfig struct {
	Type       string   `toml:"type"`
	Command    string   `toml:"command"`
	Args       []string `toml:"args"`
	FullAccess *bool    `toml:"full_access"`
	Enabled    *bool    `toml:"enabled"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	cfg.sourceDir = filepath.Dir(abs)
	if cfg.TelegramTokenEnv != "" {
		if cfg.TelegramToken != "" {
			return nil, errors.New("configure only one of telegram_token or telegram_token_env")
		}
		token, ok := os.LookupEnv(cfg.TelegramTokenEnv)
		if !ok || strings.TrimSpace(token) == "" {
			return nil, fmt.Errorf("environment variable %s is not set", cfg.TelegramTokenEnv)
		}
		cfg.TelegramToken = token
	} else if info, statErr := os.Stat(abs); statErr == nil && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("config containing telegram_token must not be accessible by group or others (mode %04o)", info.Mode().Perm())
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.ProjectDiscoveryDepth == 0 {
		c.ProjectDiscoveryDepth = defaultDiscoveryDepth
	}
	if c.StateFile == "" {
		c.StateFile = "./var/state.json"
	}
	if c.QueueSize == 0 {
		c.QueueSize = defaultQueueSize
	}
	if c.LogMaxBytes == 0 {
		c.LogMaxBytes = defaultLogMaxBytes
	}
	if c.LogBackups == 0 {
		c.LogBackups = defaultLogBackups
	}
	if c.MaxMessageBytes == 0 {
		c.MaxMessageBytes = defaultMaxMessageSize
	}
	if c.TaskTimeout == "" {
		c.TaskTimeout = defaultTaskTimeout.String()
	}
	if c.TelegramAPIBase == "" {
		c.TelegramAPIBase = "https://api.telegram.org"
	}
	if c.PrivateChatsOnly == nil {
		value := true
		c.PrivateChatsOnly = &value
	}
	if len(c.Agents) == 0 {
		legacy := c.Codex
		legacy.Type = "codex"
		if legacy.Command == "" {
			legacy.Command = "codex"
		}
		c.Codex = legacy
		c.Agents = map[string]AgentConfig{"codex": legacy}
	}
	for name, configured := range c.Agents {
		if configured.Type == "" {
			configured.Type = name
		}
		if configured.Command == "" {
			configured.Command = configured.Type
		}
		if configured.FullAccess == nil {
			value := true
			configured.FullAccess = &value
		}
		if configured.Enabled == nil {
			value := true
			configured.Enabled = &value
		}
		c.Agents[name] = configured
	}
	if configured, ok := c.Agents["codex"]; ok {
		c.Codex = configured
	}
	if c.DefaultAgent == "" {
		if configured, ok := c.Agents["codex"]; ok && *configured.Enabled {
			c.DefaultAgent = "codex"
		} else {
			for name, configured := range c.Agents {
				if *configured.Enabled && (c.DefaultAgent == "" || name < c.DefaultAgent) {
					c.DefaultAgent = name
				}
			}
		}
	}
}

func (c *Config) Validate() error {
	var problems []error
	if strings.TrimSpace(c.TelegramToken) == "" || c.TelegramToken == "BOT_TOKEN" {
		problems = append(problems, errors.New("telegram_token must be configured"))
	}
	if len(c.AllowedUserIDs) == 0 {
		problems = append(problems, errors.New("allowed_user_ids must contain at least one user ID"))
	}
	if len(c.ProjectRoots) == 0 {
		problems = append(problems, errors.New("project_roots must contain at least one path"))
	}
	if c.ProjectDiscoveryDepth < 1 || c.ProjectDiscoveryDepth > 32 {
		problems = append(problems, errors.New("project_discovery_depth must be between 1 and 32"))
	}
	if c.QueueSize < 1 || c.QueueSize > 100 {
		problems = append(problems, errors.New("queue_size must be between 1 and 100"))
	}
	if c.MaxMessageBytes < 1 || c.MaxMessageBytes > 1024*1024 {
		problems = append(problems, errors.New("max_message_bytes must be between 1 and 1048576"))
	}
	if c.LogMaxBytes < 1024 || c.LogMaxBytes > 1024*1024*1024 {
		problems = append(problems, errors.New("log_max_bytes must be between 1024 and 1073741824"))
	}
	if c.LogBackups < 1 || c.LogBackups > 20 {
		problems = append(problems, errors.New("log_backups must be between 1 and 20"))
	}
	timeout, err := time.ParseDuration(c.TaskTimeout)
	if err != nil || timeout <= 0 {
		problems = append(problems, errors.New("task_timeout must be a positive Go duration such as 2h"))
	} else {
		c.taskTimeout = timeout
	}
	for alias, target := range c.ProjectAliases {
		if normalizeID(alias) == "" {
			problems = append(problems, fmt.Errorf("project alias %q is invalid", alias))
		}
		if filepath.IsAbs(target) {
			problems = append(problems, fmt.Errorf("project alias %q must use a path relative to a project root", alias))
		}
	}
	supported := map[string]bool{"codex": true, "claude": true, "opencode": true, "grok": true}
	enabled := 0
	for name, configured := range c.Agents {
		if normalizeID(name) != name {
			problems = append(problems, fmt.Errorf("agent name %q must use lowercase letters, numbers, and dashes", name))
		}
		if !supported[configured.Type] {
			problems = append(problems, fmt.Errorf("agent %q has unsupported type %q", name, configured.Type))
		}
		if configured.Command == "" {
			problems = append(problems, fmt.Errorf("agent %q command must not be empty", name))
		}
		if configured.Enabled != nil && *configured.Enabled {
			enabled++
		}
	}
	if enabled == 0 {
		problems = append(problems, errors.New("at least one agent must be enabled"))
	}
	defaultConfig, ok := c.Agents[c.DefaultAgent]
	if !ok || defaultConfig.Enabled == nil || !*defaultConfig.Enabled {
		problems = append(problems, fmt.Errorf("default_agent %q must name an enabled agent", c.DefaultAgent))
	}
	return errors.Join(problems...)
}

func (c *Config) ResolvePath(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(c.sourceDir, path))
}

func (c *Config) TaskDuration() time.Duration {
	return c.taskTimeout
}

func (c *Config) IsAllowedUser(id int64) bool {
	for _, allowed := range c.AllowedUserIDs {
		if id == allowed {
			return true
		}
	}
	return false
}

func (c *Config) EnabledAgents() map[string]AgentConfig {
	result := make(map[string]AgentConfig)
	for name, configured := range c.Agents {
		if configured.Enabled != nil && *configured.Enabled {
			result[name] = configured
		}
	}
	return result
}

func normalizeID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
