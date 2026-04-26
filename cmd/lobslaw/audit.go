package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jmylchreest/lobslaw/internal/audit"
	"github.com/jmylchreest/lobslaw/pkg/config"
)

// dispatchAudit handles `lobslaw audit <subcmd>` invocations. Returns
// true if it handled the args. Offline-only today: verifies the local
// JSONL sink without needing a running node. `--raft` verification
// against a running cluster uses the AuditService gRPC surface
// directly; adding a client-side wrapper is follow-up work.
func dispatchAudit(args []string) bool {
	idx := findSubcmd(args, "audit")
	if idx < 0 {
		return false
	}
	sub := args[idx+1:]
	if len(sub) == 0 {
		fmt.Fprintln(os.Stderr, "lobslaw audit: subcommand required")
		fmt.Fprintln(os.Stderr, "available subcommands: verify")
		os.Exit(2)
	}
	switch sub[0] {
	case "verify":
		auditVerify(sub[1:])
	default:
		fmt.Fprintf(os.Stderr, "lobslaw audit: unknown subcommand %q\n", sub[0])
		fmt.Fprintln(os.Stderr, "available subcommands: verify")
		os.Exit(2)
	}
	return true
}

func auditVerify(args []string) {
	fs := flag.NewFlagSet("audit verify", flag.ExitOnError)
	cfgPath := fs.String("config", envOr("LOBSLAW_CONFIG", ""), "path to config.toml (used to locate the local audit file)")
	path := fs.String("path", envOr("LOBSLAW_AUDIT_PATH", ""), "explicit path to audit.jsonl; overrides --config")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	resolved := *path
	if resolved == "" {
		if *cfgPath == "" {
			exitWith("audit verify: either --path or --config is required")
		}
		cfg, err := config.Load(config.LoadOptions{Path: *cfgPath})
		if err != nil {
			exitWith(fmt.Sprintf("load config: %v", err))
		}
		if !cfg.Audit.Local.Enabled {
			exitWith("audit verify: local sink is disabled in config; nothing to verify offline")
		}
		resolved = cfg.Audit.Local.Path
		if resolved == "" {
			exitWith("audit verify: [audit.local].path is empty in config")
		}
	}

	if _, err := os.Stat(resolved); err != nil {
		exitWith(fmt.Sprintf("audit verify: %v", err))
	}

	sink, err := audit.NewLocalSink(audit.LocalConfig{Path: resolved})
	if err != nil {
		exitWith(fmt.Sprintf("open audit log %q: %v", resolved, err))
	}
	defer func() { _ = sink.Close() }()

	res, err := sink.VerifyChain(context.Background())
	if err != nil {
		exitWith(fmt.Sprintf("verify chain: %v", err))
	}

	abs, _ := filepath.Abs(resolved)
	if res.OK {
		fmt.Printf("audit OK: %d entries checked at %s\n", res.EntriesChecked, abs)
		return
	}
	fmt.Fprintf(os.Stderr, "audit BROKEN at entry %s (after %d entries) — %s\n",
		res.FirstBreakID, res.EntriesChecked, abs)
	os.Exit(1)
}
