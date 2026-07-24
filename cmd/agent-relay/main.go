package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sovereign313/Agent-Relay/internal/codex"
	"github.com/sovereign313/Agent-Relay/internal/config"
	"github.com/sovereign313/Agent-Relay/internal/logging"
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
		runner := newRunner(cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		codexVersion, err := runner.Validate(ctx)
		cancel()
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Configuration is valid. Discovered %d projects. Codex: %s\n", len(catalog.List()), codexVersion)
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
	runner := newRunner(cfg)
	validateContext, validateCancel := context.WithTimeout(context.Background(), 15*time.Second)
	codexVersion, err := runner.Validate(validateContext)
	validateCancel()
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	service := relay.New(ctx, cfg, logger, client, stateStore, catalog, runner, version, codexVersion)
	logger.Info(
		"agent relay starting",
		"version", version,
		"projects", len(catalog.List()),
		"full_access", *cfg.Codex.FullAccess,
		"codex_version", codexVersion,
	)
	if *cfg.Codex.FullAccess {
		logger.Warn("Codex Full Access is enabled; the relay can access everything available to its OS user")
	}
	err = service.Run(ctx)
	logger.Info("agent relay stopped")
	return err
}

func newRunner(cfg *config.Config) *codex.ExecRunner {
	var removeEnv []string
	if cfg.TelegramTokenEnv != "" {
		removeEnv = append(removeEnv, cfg.TelegramTokenEnv)
	}
	return &codex.ExecRunner{
		Command:    cfg.Codex.Command,
		Args:       cfg.Codex.Args,
		FullAccess: *cfg.Codex.FullAccess,
		RemoveEnv:  removeEnv,
	}
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
