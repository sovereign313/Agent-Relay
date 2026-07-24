package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/sovereign313/Agent-Relay/internal/agent"
	"github.com/sovereign313/Agent-Relay/internal/claude"
	"github.com/sovereign313/Agent-Relay/internal/codex"
	"github.com/sovereign313/Agent-Relay/internal/config"
	"github.com/sovereign313/Agent-Relay/internal/discord"
	"github.com/sovereign313/Agent-Relay/internal/grok"
	"github.com/sovereign313/Agent-Relay/internal/logging"
	"github.com/sovereign313/Agent-Relay/internal/opencode"
	"github.com/sovereign313/Agent-Relay/internal/project"
	"github.com/sovereign313/Agent-Relay/internal/relay"
	"github.com/sovereign313/Agent-Relay/internal/store"
	"github.com/sovereign313/Agent-Relay/internal/telegram"
	"github.com/sovereign313/Agent-Relay/internal/transport"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "agent-relay:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printUsage(stderr)
		return errors.New("a command is required")
	}

	switch args[0] {
	case "run":
		configPath, err := parseConfigFlag("run", args[1:], stderr)
		if err != nil {
			return err
		}
		return runDaemon(configPath)
	case "validate":
		configPath, err := parseConfigFlag("validate", args[1:], stderr)
		if err != nil {
			return err
		}
		cfg, catalog, err := loadRuntime(configPath)
		if err != nil {
			return err
		}
		runners := newRunners(cfg)
		versions, err := validateAgents(context.Background(), runners)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Configuration is valid. Discovered %d projects. Agents: %s\n", len(catalog.List()), formatAgentVersions(versions))
		return nil
	case "projects":
		configPath, err := parseConfigFlag("projects", args[1:], stderr)
		if err != nil {
			return err
		}
		_, catalog, err := loadRuntime(configPath)
		if err != nil {
			return err
		}
		for _, item := range catalog.List() {
			fmt.Fprintf(stdout, "%-24s %s\n", item.ID, item.Path)
		}
		return nil
	case "doctor":
		configPath, err := parseConfigFlag("doctor", args[1:], stderr)
		if err != nil {
			return err
		}
		return runDoctor(configPath, stdout)
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, version)
		return nil
	case "help", "--help", "-h":
		printUsage(stdout)
		return nil
	default:
		printUsage(stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func parseConfigFlag(command string, args []string, stderr io.Writer) (string, error) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "./config.toml", "path to the TOML configuration file")
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if flags.NArg() != 0 {
		return "", fmt.Errorf("%s does not accept positional arguments", command)
	}
	return *configPath, nil
}

func runDaemon(configPath string) error {
	cfg, catalog, err := loadRuntime(configPath)
	if err != nil {
		return err
	}
	logger, closer, err := logging.New(resolveOptionalPath(cfg, cfg.LogFile), cfg.LogMaxBytes, cfg.LogBackups)
	if err != nil {
		return err
	}
	if closer != nil {
		defer closer.Close()
	}

	stateStore, err := store.Open(cfg.ResolvePath(cfg.StateFile))
	if err != nil {
		return err
	}
	runners := newRunners(cfg)
	agentVersions, err := validateAgents(context.Background(), runners)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	telegramTransport, sources, err := newTransports(cfg, logger)
	if err != nil {
		return err
	}
	service := relay.New(ctx, cfg, logger, telegramTransport, stateStore, catalog, runners, version, agentVersions, sources...)
	logger.Info(
		"agent relay starting",
		"version", version,
		"projects", len(catalog.List()),
		"agents", formatAgentVersions(agentVersions),
	)
	for name, configured := range cfg.EnabledAgents() {
		if *configured.FullAccess {
			logger.Warn("agent Full Access is enabled; it can access everything available to its OS user", "agent", name)
		}
	}
	err = service.Run(ctx)
	logger.Info("agent relay stopped")
	return err
}

func newTransports(cfg *config.Config, logger *slog.Logger) (relay.Telegram, []transport.Source, error) {
	var telegramTransport relay.Telegram
	if cfg.TelegramToken != "" && cfg.TelegramToken != "BOT_TOKEN" {
		client := telegram.New(cfg.TelegramToken, cfg.TelegramAPIBase, nil)
		telegramTransport = telegram.NewRelayTransport(client)
	}
	var sources []transport.Source
	if cfg.Discord.Enabled {
		allowed := make(map[string]struct{}, len(cfg.Discord.AllowedUserIDs))
		for _, userID := range cfg.Discord.AllowedUserIDs {
			allowed[userID] = struct{}{}
		}
		gateway, err := discord.New(discord.Config{
			Token:               cfg.Discord.Token,
			AllowedUserIDs:      allowed,
			PrivateChannelsOnly: *cfg.Discord.PrivateChannelsOnly,
		}, logger)
		if err != nil {
			return nil, nil, err
		}
		sources = append(sources, gateway)
	}
	return telegramTransport, sources, nil
}

func runDoctor(configPath string, output io.Writer) error {
	cfg, catalog, err := loadRuntime(configPath)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	telegramTransport, sources, err := newTransports(cfg, logger)
	if err != nil {
		return err
	}
	var problems []error
	fmt.Fprintf(output, "Projects: %d discovered\n", len(catalog.List()))
	for _, root := range cfg.ProjectRoots {
		resolved := cfg.ResolvePath(root)
		if err := checkWritableDirectory(resolved); err != nil {
			fmt.Fprintf(output, "FAIL project root %s: %v\n", resolved, err)
			problems = append(problems, err)
		} else {
			fmt.Fprintf(output, "OK   project root %s\n", resolved)
		}
	}
	for _, path := range []string{filepath.Dir(cfg.ResolvePath(cfg.StateFile)), filepath.Dir(resolveOptionalPath(cfg, cfg.LogFile))} {
		if path == "." || path == "" {
			continue
		}
		if err := checkWritableDirectory(path); err != nil {
			fmt.Fprintf(output, "FAIL writable directory %s: %v\n", path, err)
			problems = append(problems, err)
		} else {
			fmt.Fprintf(output, "OK   writable directory %s\n", path)
		}
	}
	statePath := cfg.ResolvePath(cfg.StateFile)
	if _, err := store.Open(statePath); err != nil {
		fmt.Fprintf(output, "FAIL state file %s: %v\n", statePath, err)
		problems = append(problems, err)
	} else {
		fmt.Fprintf(output, "OK   state file %s\n", statePath)
	}
	var senders []transport.Sender
	if telegramTransport != nil {
		senders = append(senders, telegramTransport)
	}
	for _, source := range sources {
		senders = append(senders, source)
	}
	for _, sender := range senders {
		prober, ok := sender.(transport.Prober)
		if !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := prober.Probe(ctx)
		cancel()
		if err != nil {
			fmt.Fprintf(output, "FAIL %s transport: %v\n", sender.Name(), err)
			problems = append(problems, err)
		} else {
			fmt.Fprintf(output, "OK   %s transport credentials and connectivity\n", sender.Name())
		}
	}
	runners := newRunners(cfg)
	names := make([]string, 0, len(runners))
	for name := range runners {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		runner := runners[name]
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		agentVersion, err := runner.Validate(ctx)
		cancel()
		if err != nil {
			fmt.Fprintf(output, "FAIL agent %s: %v\n", name, err)
			problems = append(problems, err)
		} else {
			fmt.Fprintf(output, "OK   agent %s: %s\n", name, agentVersion)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("doctor found %d problem(s)", len(problems))
	}
	fmt.Fprintln(output, "Doctor: all checks passed")
	return nil
}

func checkWritableDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(path, ".agent-relay-doctor-*")
	if err != nil {
		return err
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Remove(name)
}

func newRunners(cfg *config.Config) map[string]agent.Runner {
	removeEnv := cfg.SecretEnvNames()
	runners := make(map[string]agent.Runner)
	for name, configured := range cfg.EnabledAgents() {
		switch configured.Type {
		case "codex":
			runners[name] = &codex.ExecRunner{
				Command: configured.Command, Args: configured.Args,
				FullAccess: *configured.FullAccess, RemoveEnv: removeEnv,
			}
		case "claude":
			runners[name] = &claude.Runner{
				Command: configured.Command, Args: configured.Args,
				FullAccess: *configured.FullAccess, RemoveEnv: removeEnv,
			}
		case "opencode":
			runners[name] = &opencode.Runner{
				Command: configured.Command, Args: configured.Args,
				FullAccess: *configured.FullAccess, RemoveEnv: removeEnv,
			}
		case "grok":
			runners[name] = &grok.Runner{
				Command: configured.Command, Args: configured.Args,
				FullAccess: *configured.FullAccess, RemoveEnv: removeEnv,
			}
		}
	}
	return runners
}

func validateAgents(parent context.Context, runners map[string]agent.Runner) (map[string]string, error) {
	versions := make(map[string]string, len(runners))
	names := make([]string, 0, len(runners))
	for name := range runners {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ctx, cancel := context.WithTimeout(parent, 15*time.Second)
		version, err := runners[name].Validate(ctx)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("validate agent %s: %w", name, err)
		}
		versions[name] = version
	}
	return versions, nil
}

func formatAgentVersions(versions map[string]string) string {
	names := make([]string, 0, len(versions))
	for name := range versions {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+versions[name])
	}
	return strings.Join(parts, ", ")
}

func loadRuntime(configPath string) (*config.Config, *project.Catalog, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, err
	}
	roots := make([]string, 0, len(cfg.ProjectRoots))
	for _, root := range cfg.ProjectRoots {
		roots = append(roots, cfg.ResolvePath(root))
	}
	catalog, err := project.Discover(roots, cfg.ProjectAliases, cfg.ProjectDiscoveryDepth)
	if err != nil {
		return nil, nil, err
	}
	return cfg, catalog, nil
}

func resolveOptionalPath(cfg *config.Config, path string) string {
	if path == "" {
		return ""
	}
	return cfg.ResolvePath(path)
}

func printUsage(writer io.Writer) {
	fmt.Fprintln(writer, `Usage:
  agent-relay run --config ./config.toml
  agent-relay validate --config ./config.toml
  agent-relay doctor --config ./config.toml
  agent-relay projects --config ./config.toml
  agent-relay version`)
}
