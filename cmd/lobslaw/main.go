package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jmylchreest/lobslaw/internal/logging"
	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Version and Commit are injected at build time via -ldflags.
var (
	Version = "dev"
	Commit  = "none"
)

type flags struct {
	showVersion bool
	configPath  string
	logLevel    string
	logFormat   string
	all         bool
	memory      bool
	policy      bool
	compute     bool
	gateway     bool
}

func parseFlags(args []string, out *flags) error {
	fs := flag.NewFlagSet("lobslaw", flag.ContinueOnError)
	fs.BoolVar(&out.showVersion, "version", false, "print version and exit")
	fs.StringVar(&out.configPath, "config", "", "path to config.toml (overrides default lookup)")
	fs.StringVar(&out.logLevel, "log-level", "info", "log level: debug|info|warn|error")
	fs.StringVar(&out.logFormat, "log-format", "auto", "log format: auto|json|text")
	fs.BoolVar(&out.all, "all", false, "enable all node functions")
	fs.BoolVar(&out.memory, "memory", false, "enable memory function")
	fs.BoolVar(&out.policy, "policy", false, "enable policy function")
	fs.BoolVar(&out.compute, "compute", false, "enable compute function")
	fs.BoolVar(&out.gateway, "gateway", false, "enable gateway function")
	return fs.Parse(args)
}

func parseLogLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// resolveFunctions picks the enabled node functions.
// Precedence: --all > explicit per-function flags > config enabled
// bits > default (all four).
func resolveFunctions(f flags, cfg *config.Config) []types.NodeFunction {
	if f.all {
		return allFunctions()
	}

	var explicit []types.NodeFunction
	if f.memory {
		explicit = append(explicit, types.FunctionMemory)
	}
	if f.policy {
		explicit = append(explicit, types.FunctionPolicy)
	}
	if f.compute {
		explicit = append(explicit, types.FunctionCompute)
	}
	if f.gateway {
		explicit = append(explicit, types.FunctionGateway)
	}
	if len(explicit) > 0 {
		return explicit
	}

	var fromCfg []types.NodeFunction
	if cfg.Memory.Enabled {
		fromCfg = append(fromCfg, types.FunctionMemory)
	}
	if cfg.Policy.Enabled {
		fromCfg = append(fromCfg, types.FunctionPolicy)
	}
	if cfg.Compute.Enabled {
		fromCfg = append(fromCfg, types.FunctionCompute)
	}
	if cfg.Gateway.Enabled {
		fromCfg = append(fromCfg, types.FunctionGateway)
	}
	if len(fromCfg) > 0 {
		return fromCfg
	}

	return allFunctions()
}

func allFunctions() []types.NodeFunction {
	return []types.NodeFunction{
		types.FunctionMemory,
		types.FunctionPolicy,
		types.FunctionCompute,
		types.FunctionGateway,
	}
}

func main() {
	// Subcommand dispatch: `lobslaw cluster <subcmd> ...` is handled
	// before main-agent flag parsing so subcommands can own their own
	// flag sets and never touch the main Config.
	if dispatchCluster(os.Args[1:]) {
		return
	}

	var f flags
	if err := parseFlags(os.Args[1:], &f); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "lobslaw:", err)
		os.Exit(2)
	}

	if f.showVersion {
		fmt.Printf("lobslaw %s (%s)\n", Version, Commit)
		return
	}

	logger := logging.New(os.Stderr, parseLogLevel(f.logLevel), logging.Format(f.logFormat))
	slog.SetDefault(logger)

	cfg, err := config.Load(config.LoadOptions{Path: f.configPath})
	if err != nil {
		logger.Error("load config", "error", err)
		os.Exit(1)
	}

	funcs := resolveFunctions(f, cfg)
	logger.Info("lobslaw starting",
		"version", Version,
		"commit", Commit,
		"node_id", cfg.Node.ID,
		"functions", funcs,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logger.Warn("subsystems not yet wired — blocking on signal (see PLAN.md phase 2+)")
	<-ctx.Done()
	logger.Info("lobslaw stopping")
}
