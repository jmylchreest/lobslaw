// Command lobslaw is the single binary for every node function.
//
// Which functions a process actually serves — memory, policy,
// compute, gateway, storage — comes from [cluster].functions in the
// config rather than from separate binaries, so a single-node
// deployment and a five-node cluster run the same executable.
//
// Beyond starting a node, the binary hosts the operator subcommands:
// init to scaffold a config, doctor to check the environment,
// cluster for membership and certificates, plugin for skill
// management, audit to verify the local log's hash chain, and memory
// and session to read and edit a stopped node's store directly.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	logfilter "github.com/jmylchreest/slog-logfilter"

	"github.com/jmylchreest/lobslaw/internal/logging"
	"github.com/jmylchreest/lobslaw/internal/mcp"
	"github.com/jmylchreest/lobslaw/internal/node"
	"github.com/jmylchreest/lobslaw/internal/sandbox"
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
	envPath     string
	policyDirs  []string
	logLevel    string
	logFormat   string
	// logSetOnWire records which logging flags the operator actually
	// typed (or set via env). [logging] in the config file fills in
	// the rest — but must not overrule someone who passed
	// --log-level=debug to debug this very boot.
	logSetOnWire map[string]bool
	all          bool
	memory       bool
	policy       bool
	compute      bool
	gateway      bool
	storage      bool
}

func parseFlags(args []string, out *flags) error {
	fs := flag.NewFlagSet("lobslaw", flag.ContinueOnError)
	fs.BoolVar(&out.showVersion, "version", false, "print version and exit")
	fs.StringVar(&out.configPath, "config", "", "path to config.toml (overrides default lookup)")
	fs.StringVar(&out.envPath, "env", "", "path to .env file (overrides default lookup: $LOBSLAW_ENV, ./.env, $XDG_CONFIG_HOME/lobslaw/.env, ~/.config/lobslaw/.env)")
	// --policy-dir is repeatable so operators can layer multiple
	// sources on the CLI; later entries override earlier per the
	// Registry's last-write-wins semantics (matches git config's
	// system/global/local layering).
	fs.Func("policy-dir", "policy.d directory (repeatable; later overrides earlier)",
		func(v string) error {
			if v == "" {
				return nil
			}
			out.policyDirs = append(out.policyDirs, v)
			return nil
		})
	fs.StringVar(&out.logLevel, "log-level", envOr("LOBSLAW_LOG_LEVEL", "info"), "log level: debug|info|warn|error (env: LOBSLAW_LOG_LEVEL)")
	fs.StringVar(&out.logFormat, "log-format", envOr("LOBSLAW_LOG_FORMAT", "auto"), "log format: auto|json|text (env: LOBSLAW_LOG_FORMAT)")
	fs.BoolVar(&out.all, "all", false, "enable all node functions")
	fs.BoolVar(&out.memory, "memory", false, "enable memory function")
	fs.BoolVar(&out.policy, "policy", false, "enable policy function")
	fs.BoolVar(&out.compute, "compute", false, "enable compute function")
	fs.BoolVar(&out.gateway, "gateway", false, "enable gateway function")
	fs.BoolVar(&out.storage, "storage", false, "enable storage function")
	if err := fs.Parse(args); err != nil {
		return err
	}
	out.logSetOnWire = map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { out.logSetOnWire[fl.Name] = true })
	// An env var is as explicit as a flag: somebody set it on purpose,
	// and it is the mechanism a container deployment has.
	for name, env := range map[string]string{
		"log-level":  "LOBSLAW_LOG_LEVEL",
		"log-format": "LOBSLAW_LOG_FORMAT",
	} {
		if os.Getenv(env) != "" {
			out.logSetOnWire[name] = true
		}
	}
	return nil
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
	// The canonical set. policy and gateway are accepted in config and
	// normalised away, so they are not offered here.
	return []types.NodeFunction{
		types.FunctionMemory,
		types.FunctionCompute,
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
	// Hidden reexec subcommand: when the parent agent spawns a sandboxed
	// tool, it invokes /proc/self/exe with "sandbox-exec" as the first
	// arg. Dispatched before any config / logging / node setup so the
	// helper child stays small and deterministic.
	if dispatchSandboxExec(os.Args[1:]) {
		return
	}

	// Subcommand dispatch: `lobslaw cluster <subcmd> ...` is handled
	// before main-agent flag parsing so subcommands can own their own
	// flag sets and never touch the main Config. hoistGlobalFlagsToEnv
	// makes `lobslaw --config X cluster sign-node` behave the same as
	// `lobslaw cluster sign-node --config X` — the global flag value
	// reaches the subcommand via $LOBSLAW_CONFIG.
	hoistGlobalFlagsToEnv(os.Args[1:])
	for _, d := range topLevelDispatchers() {
		if d.dispatch(os.Args[1:]) {
			return
		}
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

	// Load any .env file before config so config's env:VAR secret
	// references pick up values supplied via .env. Missing .env is
	// a no-op; syntax errors are loud.
	if err := config.LoadDotenv(f.envPath); err != nil {
		logger.Error("load .env", "error", err)
		os.Exit(1)
	}

	cfg, err := config.Load(config.LoadOptions{Path: f.configPath})
	if err != nil {
		logger.Error("load config", "error", err)
		os.Exit(1)
	}

	// [logging] level / format reach the logger HERE rather than at
	// construction, because building it needs to come first — loading
	// the config is one of the things that has to be able to report an
	// error. So config can only govern what is logged after this
	// point, which is nearly everything.
	//
	// An explicit --log-level (or LOBSLAW_LOG_LEVEL) still wins. The
	// file is where the deployment's normal level lives; the flag is
	// what somebody types when they are debugging this boot, and the
	// file must not overrule them.
	if level, format, changed := effectiveLogging(f.logLevel, f.logFormat, f.logSetOnWire, cfg.Logging); changed {
		f.logLevel, f.logFormat = level, format
		logger = logging.New(os.Stderr, parseLogLevel(level), logging.Format(format))
		slog.SetDefault(logger)
	}

	// Apply any startup filters from [[logging.filters]]. Runtime
	// filter mutation (via NodeService.Reload) lands in Phase 11.
	applyLogFilters(cfg.Logging.Filters, logger)

	// Resolve the effective list of policy directories the node
	// will watch once Phase 5 wires compute.Registry into the boot
	// path. Log it now so operators can verify precedence without
	// having to wait for actual tool invocations to surface the
	// chosen paths.
	policyDirs := resolvePolicyDirs(f.policyDirs, cfg)
	logger.Info("sandbox policy dirs resolved",
		"dirs", policyDirs,
		"source", policyDirsSource(f.policyDirs, cfg))

	funcs := resolveFunctions(f, cfg)
	nodeID := derivedNodeID()
	logger.Info("lobslaw starting",
		"version", Version,
		"commit", Commit,
		"node_id", nodeID,
		"functions", funcs,
	)

	nodeCfg, err := buildNodeConfig(cfg, nodeID, funcs, logger)
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

	// SIGHUP triggers an mTLS cert reload from disk — the conventional
	// "re-read your config files" signal. Atomic-swaps the live cert;
	// in-flight handshakes are unaffected, new handshakes pick up the
	// rotated material on the next connection.
	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	defer signal.Stop(hupCh)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hupCh:
				if nodeCfg.Creds == nil {
					logger.Warn("SIGHUP: no mTLS creds configured; ignoring")
					continue
				}
				if err := nodeCfg.Creds.Reload(); err != nil {
					logger.Error("SIGHUP: cert reload failed", "error", err)
					continue
				}
				logger.Info("SIGHUP: mTLS certs reloaded", "node_id", nodeCfg.Creds.NodeID)
			}
		}
	}()

	if err := n.Start(ctx); err != nil {
		logger.Error("node.Start", "error", err)
		// gocritic exitAfterDefer: the deferred signal.Stop(hupCh) is
		// knowingly skipped. Unregistering a signal handler
		// immediately before the process exits has no observable
		// effect, and restructuring main to return an exit code just
		// to satisfy the check is not worth the control-flow change.
		os.Exit(1) //nolint:gocritic // exitAfterDefer: signal.Stop is moot at process exit
	}
	logger.Info("lobslaw stopped")
}

// buildNodeConfig resolves mTLS creds + the memory encryption key +
// the other fields node.New needs from the parsed config. The main
// binary intentionally does NOT read the CA private key — that field
// isn't present on MTLSConfig in the first place.
func buildNodeConfig(cfg *config.Config, nodeID string, funcs []types.NodeFunction, logger *slog.Logger) (node.Config, error) {
	needsRaft := containsFn(funcs, types.FunctionMemory) || containsFn(funcs, types.FunctionPolicy)

	// Merge .mcp.json from the same dir as config.toml. Trust model
	// is identical to the [[mcp.servers]] block: operator-controlled
	// path. Same file perms guard the file; on k8s a ConfigMap drops
	// the file at /etc/lobslaw/.mcp.json next to config.toml. Names
	// in config.toml win on collision (first-write semantics).
	if cfg.Dir() != "" {
		manifestPath := filepath.Join(cfg.Dir(), ".mcp.json")
		if _, err := os.Stat(manifestPath); err == nil {
			m, err := mcp.LoadManifest(manifestPath)
			if err != nil {
				return node.Config{}, fmt.Errorf("load %s: %w", manifestPath, err)
			}
			if cfg.MCP.Servers == nil {
				cfg.MCP.Servers = make(map[string]config.MCPServerConfig, len(m.MCPServers))
			}
			added := []string{}
			for name, s := range m.MCPServers {
				if _, exists := cfg.MCP.Servers[name]; exists {
					logger.Warn("mcp: .mcp.json entry shadowed by config.toml", "name", name, "source", manifestPath)
					continue
				}
				cfg.MCP.Servers[name] = config.MCPServerConfig{
					Command:   s.Command,
					Args:      s.Args,
					Env:       s.Env,
					SecretEnv: s.SecretEnv,
					Disabled:  s.Disabled,
					Install:   s.Install,
				}
				added = append(added, name)
			}
			if len(added) > 0 {
				logger.Info("mcp: merged servers from .mcp.json", "path", manifestPath, "count", len(added), "names", added)
			}
		} else if !os.IsNotExist(err) {
			return node.Config{}, fmt.Errorf("stat %s: %w", manifestPath, err)
		}
	}

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

	bcastPort := cfg.Discovery.BroadcastPort
	if bcastPort == 0 {
		bcastPort = 7445
	}
	bcastAddr := cfg.Discovery.BroadcastAddress
	if bcastAddr == "" {
		bcastAddr = "255.255.255.255"
	}

	return node.Config{
		NodeID:           nodeID,
		Functions:        funcs,
		ListenAddr:       listen,
		AdvertiseAddr:    cfg.Cluster.AdvertiseAddr,
		SeedNodes:        cfg.Discovery.SeedNodes,
		DataDir:          cfg.Cluster.DataDir,
		Bootstrap:        resolveBootstrap(cfg.Cluster.Bootstrap),
		BootstrapTimeout: cfg.Cluster.BootstrapTimeout,
		SnapshotTarget:   cfg.Memory.Snapshot.Target,
		MemoryDream:      cfg.Memory.Dream,
		MemorySession:    cfg.Memory.Session,

		MemoryWriteApproval:       cfg.Memory.WriteApproval,
		MemoryPinnedProfileChars:  cfg.Memory.PinnedProfileChars,
		MemoryPinnedNotesChars:    cfg.Memory.PinnedNotesChars,
		SelfLearningMode:          cfg.SelfLearning.Mode,
		ReviewSkillToolIterations: cfg.SelfLearning.ReviewSkillToolIterations,
		ReviewMemoryTurnInterval:  cfg.SelfLearning.ReviewMemoryTurnInterval,
		SelfTaughtHistoryDepth:    cfg.SelfLearning.HistoryDepth,
		SelfTaughtMaxFileBytes:    cfg.SelfLearning.MaxArtefactFileBytes,
		SelfTaughtMaxTotalBytes:   cfg.SelfLearning.MaxArtefactTotalBytes,

		SelfTaughtStaleAfterDays:     cfg.SelfLearning.StaleAfterDays,
		SelfTaughtArchiveAfterDays:   cfg.SelfLearning.ArchiveAfterDays,
		SelfTaughtProposalExpiryDays: cfg.SelfLearning.ProposalExpiryDays,
		NotifyChannels:               cfg.SelfLearning.Notify.Channels,
		NotifySubjects:               cfg.SelfLearning.Notify.Subjects,
		NotifyInterval:               cfg.SelfLearning.Notify.Interval,
		Trace:                        cfg.Trace,
		MTLS:                         cfg.Cluster.MTLS,

		SessionGrantTTL:     cfg.Security.SessionGrantTTL,
		Identity:            cfg.Identity,
		Policy:              cfg.Policy,
		BroadcastEnabled:    cfg.Discovery.Broadcast,
		BroadcastAddress:    fmt.Sprintf("%s:%d", bcastAddr, bcastPort),
		BroadcastListenAddr: fmt.Sprintf(":%d", bcastPort),
		BroadcastInterval:   cfg.Discovery.BroadcastInterval,
		Creds:               creds,
		MemoryKey:           memKey,
		Compute:             cfg.Compute,
		Hooks:               cfg.Hooks,
		Auth:                cfg.Auth,
		Gateway:             cfg.Gateway,
		Audit:               cfg.Audit,
		Storage:             cfg.Storage,
		Skills:              cfg.Skills,
		MCP:                 cfg.MCP,
		Security:            cfg.Security,
		Users:               cfg.Users,
		Binaries:            cfg.Binaries,
		SoulPath:            cfg.Soul.Path,
		Logger:              logger,
	}, nil
}

// resolveBootstrap defaults the [cluster] bootstrap flag to true so
// solo and first-of-cluster runs Just Work; operators flip it to
// false on production joiners to forbid accidental split-brain.
func resolveBootstrap(v *bool) bool {
	if v == nil {
		return true
	}
	return *v
}

func containsFn(fns []types.NodeFunction, target types.NodeFunction) bool {
	for _, f := range fns {
		if f == target {
			return true
		}
	}
	return false
}

// resolvePolicyDirs implements the sandbox policy.d discovery chain
// at process start. Precedence (later overrides earlier when both
// are present — same "last write wins" as Registry.SetPolicy):
//
//  1. Default discovery    — ~/.config/lobslaw/policy.d,
//     <configDir>/policy.d, <cwd>/policy.d
//  2. Config file's policy_dirs  — replaces #1 entirely if set
//  3. CLI --policy-dir (repeated) — replaces #1 and #2 if set
//
// Explicit sources replace rather than merge with defaults because
// "if I set --policy-dir, don't sneak in extras" is the universal
// CLI-ergonomics expectation.
func resolvePolicyDirs(cliDirs []string, cfg *config.Config) []string {
	switch {
	case len(cliDirs) > 0:
		return sandbox.DiscoverPolicyDirs(cliDirs, cfg.Dir())
	case len(cfg.Sandbox.PolicyDirs) > 0:
		return sandbox.DiscoverPolicyDirs(cfg.Sandbox.PolicyDirs, cfg.Dir())
	default:
		return sandbox.DiscoverPolicyDirs(nil, cfg.Dir())
	}
}

// policyDirsSource returns a short label describing where the
// effective policy_dirs list came from — handy in startup logs so
// operators can see "was my --policy-dir actually used?".
func policyDirsSource(cliDirs []string, cfg *config.Config) string {
	switch {
	case len(cliDirs) > 0:
		return "cli"
	case len(cfg.Sandbox.PolicyDirs) > 0:
		return "config"
	default:
		return "default-discovery"
	}
}

// effectiveLogging folds [logging] into the flag-derived level and
// format, reporting whether anything moved.
//
// An explicit --log-level (or LOBSLAW_LOG_LEVEL) wins. The config file
// is where a deployment's normal level lives; the flag is what
// somebody types when they are debugging this boot, and the file must
// not overrule them.
func effectiveLogging(flagLevel, flagFormat string, setOnWire map[string]bool, cfg config.LoggingConfig) (level, format string, changed bool) {
	level, format = flagLevel, flagFormat
	if v := strings.TrimSpace(cfg.Level); v != "" && !setOnWire["log-level"] {
		level, changed = v, true
	}
	if v := strings.TrimSpace(cfg.Format); v != "" && !setOnWire["log-format"] {
		format, changed = v, true
	}
	return level, format, changed
}
