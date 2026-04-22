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

	logfilter "github.com/jmylchreest/slog-logfilter"

	"github.com/jmylchreest/lobslaw/internal/logging"
	"github.com/jmylchreest/lobslaw/internal/node"
	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
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
	storage     bool
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
	fs.BoolVar(&out.storage, "storage", false, "enable storage function")
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
	if f.storage {
		explicit = append(explicit, types.FunctionStorage)
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
	if cfg.Storage.Enabled {
		fromCfg = append(fromCfg, types.FunctionStorage)
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
		types.FunctionStorage,
	}
}

// applyLogFilters translates config-file filter entries into
// logfilter.LogFilter values and installs them via the library's
// global API. A no-op when cfgFilters is empty.
func applyLogFilters(cfgFilters []config.LogFilterConfig, logger *slog.Logger) {
	if len(cfgFilters) == 0 {
		return
	}
	filters := make([]logfilter.LogFilter, 0, len(cfgFilters))
	for _, f := range cfgFilters {
		filters = append(filters, logfilter.LogFilter{
			Type:        f.Type,
			Pattern:     f.Pattern,
			Level:       f.Level,
			OutputLevel: f.OutputLevel,
			Enabled:     f.Enabled,
		})
	}
	logfilter.SetFilters(filters)
	logger.Info("log filters applied from config", "count", len(filters))
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

	// Apply any startup filters from [[logging.filters]]. Runtime
	// filter mutation (via NodeService.Reload) lands in Phase 11.
	applyLogFilters(cfg.Logging.Filters, logger)

	funcs := resolveFunctions(f, cfg)
	logger.Info("lobslaw starting",
		"version", Version,
		"commit", Commit,
		"node_id", cfg.Node.ID,
		"functions", funcs,
	)

	nodeCfg, err := buildNodeConfig(cfg, funcs, logger)
	if err != nil {
		logger.Error("node config", "error", err)
		os.Exit(1)
	}

	n, err := node.New(nodeCfg)
	if err != nil {
		logger.Error("node.New", "error", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := n.Start(ctx); err != nil {
		logger.Error("node.Start", "error", err)
		os.Exit(1)
	}
	logger.Info("lobslaw stopped")
}

// buildNodeConfig resolves mTLS creds + the memory encryption key +
// the other fields node.New needs from the parsed config. The main
// binary intentionally does NOT read the CA private key — that field
// isn't present on MTLSConfig in the first place.
func buildNodeConfig(cfg *config.Config, funcs []types.NodeFunction, logger *slog.Logger) (node.Config, error) {
	needsRaft := containsFn(funcs, types.FunctionMemory) || containsFn(funcs, types.FunctionPolicy)

	var creds *mtls.NodeCreds
	if cfg.Cluster.MTLS.CACert != "" || cfg.Cluster.MTLS.NodeCert != "" {
		c, err := mtls.LoadNodeCreds(cfg.Cluster.MTLS.CACert, cfg.Cluster.MTLS.NodeCert, cfg.Cluster.MTLS.NodeKey)
		if err != nil {
			return node.Config{}, fmt.Errorf("load mTLS creds: %w", err)
		}
		creds = c
	} else {
		return node.Config{}, fmt.Errorf("[cluster.mtls] ca_cert / node_cert / node_key paths are required (run `lobslaw cluster ca-init` + `cluster sign-node` first)")
	}

	var memKey crypto.Key
	if needsRaft {
		if cfg.Memory.Encryption.KeyRef == "" {
			return node.Config{}, fmt.Errorf("memory.encryption.key_ref required when memory or policy function is enabled")
		}
		raw, err := config.ResolveSecret(cfg.Memory.Encryption.KeyRef)
		if err != nil {
			return node.Config{}, fmt.Errorf("resolve memory key: %w", err)
		}
		k, err := crypto.ParseKey(raw)
		if err != nil {
			return node.Config{}, fmt.Errorf("parse memory key: %w", err)
		}
		memKey = k
	}

	listen := cfg.Cluster.ListenAddr
	if listen == "" {
		listen = ":7443"
	}

	return node.Config{
		NodeID:         cfg.Node.ID,
		Functions:      funcs,
		ListenAddr:     listen,
		AdvertiseAddr:  cfg.Cluster.AdvertiseAddr,
		SeedNodes:      cfg.Discovery.SeedNodes,
		DataDir:        cfg.Cluster.DataDir,
		Bootstrap:      cfg.Cluster.InitialBootstrap,
		SnapshotTarget: cfg.Memory.Snapshot.Target,
		Creds:          creds,
		MemoryKey:      memKey,
		Logger:         logger,
	}, nil
}

func containsFn(fns []types.NodeFunction, target types.NodeFunction) bool {
	for _, f := range fns {
		if f == target {
			return true
		}
	}
	return false
}
