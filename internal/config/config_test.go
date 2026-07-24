package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadAppliesDefaultsAndResolvesPaths(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.toml")
	data := `telegram_token = "test-token"
allowed_user_ids = [42]
project_roots = ["./projects"]
`
	if err := os.WriteFile(configPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QueueSize != 5 {
		t.Fatalf("QueueSize = %d, want 5", cfg.QueueSize)
	}
	if cfg.TaskDuration() != 2*time.Hour {
		t.Fatalf("TaskDuration = %s, want 2h", cfg.TaskDuration())
	}
	if !*cfg.PrivateChatsOnly || !*cfg.Codex.FullAccess {
		t.Fatal("secure chat and Full Access defaults were not applied")
	}
	want := filepath.Join(root, "projects")
	if got := cfg.ResolvePath(cfg.ProjectRoots[0]); got != want {
		t.Fatalf("ResolvePath = %q, want %q", got, want)
	}
}

func TestLoadRejectsPlaceholderToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	data := `telegram_token = "BOT_TOKEN"
allowed_user_ids = [42]
project_roots = ["/tmp"]
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded with placeholder token")
	}
}

func TestLoadRejectsInsecurePlaintextTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	data := `telegram_token = "secret"
allowed_user_ids = [42]
project_roots = ["/tmp"]
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted a group/world-readable plaintext token")
	}
}

func TestLoadSupportsTokenEnvironmentVariable(t *testing.T) {
	t.Setenv("AGENT_RELAY_TEST_TOKEN", "environment-secret")
	path := filepath.Join(t.TempDir(), "config.toml")
	data := `telegram_token_env = "AGENT_RELAY_TEST_TOKEN"
allowed_user_ids = [42]
project_roots = ["/tmp"]
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TelegramToken != "environment-secret" {
		t.Fatalf("token = %q", cfg.TelegramToken)
	}
}

func TestLoadConfiguresMultipleAgents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	data := `telegram_token = "secret"
allowed_user_ids = [42]
project_roots = ["/tmp"]
default_agent = "claude"

[agents.codex]
type = "codex"
command = "codex"

[agents.claude]
type = "claude"
command = "claude"

[agents.grok]
type = "grok"
command = "grok"
enabled = false
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	enabled := cfg.EnabledAgents()
	if cfg.DefaultAgent != "claude" || len(enabled) != 2 {
		t.Fatalf("default = %q, enabled = %#v", cfg.DefaultAgent, enabled)
	}
	if enabled["claude"].Type != "claude" || !*enabled["claude"].FullAccess {
		t.Fatalf("Claude config = %#v", enabled["claude"])
	}
	if _, ok := enabled["grok"]; ok {
		t.Fatal("disabled Grok agent was enabled")
	}
}

func TestLoadSupportsDiscordWithoutTelegram(t *testing.T) {
	t.Setenv("AGENT_RELAY_TEST_DISCORD_TOKEN", "discord-secret")
	path := filepath.Join(t.TempDir(), "config.toml")
	data := `project_roots = ["/tmp"]

[discord]
enabled = true
token_env = "AGENT_RELAY_TEST_DISCORD_TOKEN"
allowed_user_ids = ["123456789012345678"]
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Discord.Token != "discord-secret" || !*cfg.Discord.PrivateChannelsOnly {
		t.Fatalf("Discord config = %#v", cfg.Discord)
	}
	if got := cfg.SecretEnvNames(); len(got) != 1 || got[0] != "AGENT_RELAY_TEST_DISCORD_TOKEN" {
		t.Fatalf("secret env names = %#v", got)
	}
}
