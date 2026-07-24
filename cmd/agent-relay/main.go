package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/sovereign313/Agent-Relay/internal/agent"
	"github.com/sovereign313/Agent-Relay/internal/claude"
	"github.com/sovereign313/Agent-Relay/internal/codex"
	"github.com/sovereign313/Agent-Relay/internal/config"
	"github.com/sovereign313/Agent-Relay/internal/grok"
	"github.com/sovereign313/Agent-Relay/internal/logging"
	"github.com/sovereign313/Agent-Relay/internal/opencode"
	"github.com/sovereign313/Agent-Relay/internal/project"
	"github.com/sovereign313/Agent-Relay/internal/relay"
	"github.com/sovereign313/Agent-Relay/internal/store"
	"github.com/sovereign313/Agent-Relay/internal/telegram"
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
	client := telegram.New(cfg.TelegramToken, cfg.TelegramAPIBase, nil)
	runners := newRunners(cfg)
	agentVersions, err := validateAgents(context.Background(), runners)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	service := relay.New(ctx, cfg, logger, client, stateStore, catalog, runners, version, agentVersions)
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

func newRunners(cfg *config.Config) map[string]agent.Runner {
	var removeEnv []string
	if cfg.TelegramTokenEnv != "" {
		removeEnv = append(removeEnv, cfg.TelegramTokenEnv)
	}
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
  agent-relay projects --config ./config.toml
  agent-relay version`)
}
