package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/mtls"
)

// dispatchInit handles `lobslaw init`. Returns true if handled.
func dispatchInit(args []string) bool {
	idx := findSubcmd(args, "init")
	if idx < 0 {
		return false
	}
	lobslawInit(args[idx+1:])
	return true
}

// initAnswers bundles the values the init flow collects, either from
// stdin or from env vars in non-interactive mode.
type initAnswers struct {
	Dir              string
	ProviderLabel    string
	ProviderEndpoint string
	ProviderModel    string
	ProviderAPIKey   string
	SoulName         string
}

func lobslawInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	dir := fs.String("dir", defaultInitDir(), "where to write config.toml, .env, SOUL.md, certs/")
	nonInteractive := fs.Bool("non-interactive", envOr("LOBSLAW_NONINTERACTIVE", "") != "", "skip prompts; read values from LOBSLAW_INIT_* env vars")
	force := fs.Bool("force", false, "overwrite existing files in --dir")
	_ = fs.Parse(args)

	// Collect answers.
	ans := initAnswers{Dir: *dir}
	if *nonInteractive {
		if err := fillFromEnv(&ans); err != nil {
			exitWith(fmt.Sprintf("init --non-interactive: %v", err))
		}
	} else {
		if err := fillFromStdin(&ans); err != nil {
			exitWith(fmt.Sprintf("init: %v", err))
		}
	}

	// Abort before clobbering, unless --force.
	if !*force {
		if existing := existingInitFiles(ans.Dir); len(existing) > 0 {
			exitWith(fmt.Sprintf("init: %s already exist; pass --force to overwrite", strings.Join(existing, ", ")))
		}
	}

	if err := runInit(ans); err != nil {
		exitWith(fmt.Sprintf("init: %v", err))
	}

	fmt.Printf("lobslaw configured in %s\n", ans.Dir)
	fmt.Printf("next steps:\n")
	fmt.Printf("  1. review %s\n", filepath.Join(ans.Dir, "config.toml"))
	fmt.Printf("  2. export $(grep -v '^#' %s | xargs)   # load secrets from .env\n", filepath.Join(ans.Dir, ".env"))
	fmt.Printf("  3. LOBSLAW_CONFIG=%s lobslaw\n", filepath.Join(ans.Dir, "config.toml"))
	fmt.Printf("  (or: lobslaw doctor --config %s   to verify)\n", filepath.Join(ans.Dir, "config.toml"))
}

// defaultInitDir resolves the default output dir: $XDG_CONFIG_HOME/
// lobslaw, falling back to $HOME/.config/lobslaw, then ./lobslaw.
func defaultInitDir() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "lobslaw")
	}
	if v := os.Getenv("HOME"); v != "" {
		return filepath.Join(v, ".config", "lobslaw")
	}
	return "./lobslaw"
}

// existingInitFiles returns any init-managed files that already
// exist in dir. Used to gate --force.
func existingInitFiles(dir string) []string {
	var out []string
	for _, name := range []string{"config.toml", ".env", "SOUL.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			out = append(out, name)
		}
	}
	return out
}

// fillFromStdin runs the interactive prompt flow. Readline-style:
// one question per line, ENTER accepts default. Detects non-TTY
// stdin and errors with an actionable message so container users
// know to pass --non-interactive or set LOBSLAW_NONINTERACTIVE=1.
func fillFromStdin(ans *initAnswers) error {
	fi, err := os.Stdin.Stat()
	if err == nil && (fi.Mode()&os.ModeCharDevice) == 0 {
		return fmt.Errorf("stdin is not a TTY; pass --non-interactive + LOBSLAW_INIT_* env vars, or run with `docker exec -it` / `kubectl exec -it`")
	}

	r := bufio.NewReader(os.Stdin)
	fmt.Println("lobslaw init — set up a fresh single-node configuration.")
	fmt.Println()

	ans.ProviderLabel = prompt(r, "provider label (internal name)", "fast")
	ans.ProviderEndpoint = prompt(r, "provider endpoint URL", "https://openrouter.ai/api/v1")
	ans.ProviderModel = prompt(r, "provider model", "meta-llama/llama-3.1-8b-instruct")
	ans.ProviderAPIKey = promptSecret(r, "provider API key (stored in .env, chmod 0600)")
	ans.SoulName = prompt(r, "assistant name (appears in SOUL.md)", "assistant")
	return nil
}

// fillFromEnv populates answers from LOBSLAW_INIT_* env vars for
// container-driven onboarding. Required values missing → error.
func fillFromEnv(ans *initAnswers) error {
	ans.ProviderLabel = envOr("LOBSLAW_INIT_PROVIDER_LABEL", "fast")
	ans.ProviderEndpoint = os.Getenv("LOBSLAW_INIT_PROVIDER_ENDPOINT")
	ans.ProviderModel = os.Getenv("LOBSLAW_INIT_PROVIDER_MODEL")
	ans.ProviderAPIKey = os.Getenv("LOBSLAW_INIT_PROVIDER_API_KEY")
	ans.SoulName = envOr("LOBSLAW_INIT_SOUL_NAME", "assistant")

	missing := []string{}
	if ans.ProviderEndpoint == "" {
		missing = append(missing, "LOBSLAW_INIT_PROVIDER_ENDPOINT")
	}
	if ans.ProviderModel == "" {
		missing = append(missing, "LOBSLAW_INIT_PROVIDER_MODEL")
	}
	if ans.ProviderAPIKey == "" {
		missing = append(missing, "LOBSLAW_INIT_PROVIDER_API_KEY")
	}
	if len(missing) > 0 {
		return fmt.Errorf("required env vars missing: %s", strings.Join(missing, ", "))
	}
	return nil
}

// prompt asks question with default; empty reply → default.
func prompt(r *bufio.Reader, question, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", question, def)
	} else {
		fmt.Printf("%s: ", question)
	}
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		exitWith(fmt.Sprintf("read stdin: %v", err))
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

// promptSecret asks for a value with no default; empty reply rejected.
// Not hidden from the terminal — terminal hiding requires x/term and
// portability is poor across docker exec, kubectl exec, etc. The
// value only lives in the generated .env (chmod 0600).
func promptSecret(r *bufio.Reader, question string) string {
	for {
		v := prompt(r, question, "")
		if v != "" {
			return v
		}
		fmt.Fprintln(os.Stderr, "(required; please enter a value)")
	}
}

// runInit does the actual file writes + cert generation after
// answers are collected.
func runInit(ans initAnswers) error {
	certsDir := filepath.Join(ans.Dir, "certs")
	dataDir := filepath.Join(ans.Dir, "data")
	auditDir := filepath.Join(ans.Dir, "audit")
	for _, d := range []string{ans.Dir, certsDir, dataDir, auditDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %q: %w", d, err)
		}
	}

	// 32 random bytes, base64-std. Matches what memory.Key expects
	// from env:LOBSLAW_MEMORY_KEY resolution.
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		return fmt.Errorf("generate memory key: %w", err)
	}
	memKey := base64.StdEncoding.EncodeToString(keyBytes)

	// CA + node cert. 10-year CA; 1-year node cert, matching
	// `cluster ca-init` defaults.
	caCertPEM, caKeyPEM, err := mtls.GenerateCA(mtls.CAOpts{
		CommonName: "Lobslaw Cluster CA",
		ValidFor:   10 * 365 * 24 * time.Hour,
	})
	if err != nil {
		return fmt.Errorf("generate CA: %w", err)
	}
	caCertPath := filepath.Join(certsDir, "ca.pem")
	caKeyPath := filepath.Join(certsDir, "ca-key.pem")
	if err := mtls.WriteCAFiles(caCertPath, caKeyPath, caCertPEM, caKeyPEM); err != nil {
		return fmt.Errorf("write CA: %w", err)
	}

	caCert, caKey, err := mtls.LoadCA(caCertPath, caKeyPath)
	if err != nil {
		return fmt.Errorf("reload CA: %w", err)
	}
	nodeCertPEM, nodeKeyPEM, err := mtls.SignNodeCert(caCert, caKey, mtls.SignOpts{
		NodeID:   derivedNodeID(),
		ValidFor: 365 * 24 * time.Hour,
	})
	if err != nil {
		return fmt.Errorf("sign node cert: %w", err)
	}
	nodeCertPath := filepath.Join(certsDir, "node-cert.pem")
	nodeKeyPath := filepath.Join(certsDir, "node-key.pem")
	if err := mtls.WriteNodeFiles(nodeCertPath, nodeKeyPath, nodeCertPEM, nodeKeyPEM); err != nil {
		return fmt.Errorf("write node cert: %w", err)
	}

	// .env — memory key + provider API key. chmod 0600 is required
	// because the provider key is a bearer credential.
	envPath := filepath.Join(ans.Dir, ".env")
	// The provider API key env var name deliberately does NOT match
	// the LOBSLAW_PROVIDER_<LABEL>_<FIELD> pattern — that pattern is
	// the provider-env-override system (pkg/config/env_providers),
	// which treats its values as ref strings (env:VAR) and would
	// try to re-resolve a raw key as a ref, failing with "unknown
	// secret-ref scheme". Using LOBSLAW_<LABEL>_API_KEY keeps the
	// two mechanisms cleanly separated.
	keyVar := fmt.Sprintf("LOBSLAW_%s_API_KEY", strings.ToUpper(ans.ProviderLabel))
	envContent := fmt.Sprintf(
		"# Generated by `lobslaw init` at %s\n"+
			"# DO NOT commit this file — it contains secrets.\n"+
			"LOBSLAW_MEMORY_KEY=%s\n"+
			"%s=%s\n",
		time.Now().UTC().Format(time.RFC3339),
		memKey,
		keyVar, ans.ProviderAPIKey,
	)
	if err := os.WriteFile(envPath, []byte(envContent), 0o600); err != nil {
		return fmt.Errorf("write .env: %w", err)
	}

	// config.toml — rendered from a single-node template.
	cfgPath := filepath.Join(ans.Dir, "config.toml")
	if err := writeConfigTOML(cfgPath, ans, dataDir, auditDir, caCertPath, nodeCertPath, nodeKeyPath, keyVar); err != nil {
		return err
	}

	// SOUL.md — minimal sensible default.
	soulPath := filepath.Join(ans.Dir, "SOUL.md")
	soulContent := fmt.Sprintf(`---
name: %s
scope: default
emotive_style:
  emoji_usage: minimal
  excitement: 5
  formality: 5
  directness: 6
  sarcasm: 2
  humor: 3
---

You are %s. Answer clearly and truthfully. Refuse destructive
actions that aren't explicitly authorized. Prefer short, direct
replies over preamble.
`, ans.SoulName, ans.SoulName)
	if err := os.WriteFile(soulPath, []byte(soulContent), 0o644); err != nil {
		return fmt.Errorf("write SOUL.md: %w", err)
	}

	return nil
}

// configTemplate is the minimal single-node dev config. Memory is
// enabled with a local-filesystem snapshot target so startup doesn't
// trip the "memory without snapshot target" guard in validateConfig.
// Policy + scheduler + audit + gateway are all on; operators trim
// anything they don't want after review.
const configTemplate = `# Generated by lobslaw init.
# Env-var overrides: LOBSLAW__SECTION__KEY=value
# Secret refs: env:NAME (read from .env or process env)
#
# Node identity is derived at boot from $LOBSLAW_NODE_ID or the
# short hostname; the cert signed below is bound to that same value.

[cluster]
listen_addr = "0.0.0.0:7443"
data_dir    = "{{.DataDir}}"
# bootstrap = true (default): if no peer is reachable via seed_nodes
# within bootstrap_timeout, form a fresh single-voter cluster.
# Set to false on production joiners that must never split-brain.
# bootstrap = true
# bootstrap_timeout = "30s"

[cluster.mtls]
ca_cert   = "{{.CACert}}"
node_cert = "{{.NodeCert}}"
node_key  = "{{.NodeKey}}"

[memory]
enabled = true

[memory.encryption]
key_ref = "env:LOBSLAW_MEMORY_KEY"

[memory.snapshot]
target    = "storage:local-snapshots"
cadence   = "1h"
retention = "7d"

[[storage.mounts]]
label = "local-snapshots"
type  = "local"
path  = "{{.DataDir}}/snapshots"

[policy]
enabled = true

[storage]
enabled = true

[compute]
enabled = true
# default_chain is omitted — no [[compute.chains]] are defined here
# so the resolver falls through to first-provider selection. Add
# chains + set default_chain if you want multi-provider routing.

[[compute.providers]]
label       = "{{.ProviderLabel}}"
endpoint    = "{{.ProviderEndpoint}}"
model       = "{{.ProviderModel}}"
api_key_ref = "env:{{.ProviderKeyVar}}"
trust_tier  = "public"
capabilities = ["function-calling"]

[compute.budgets]
max_tool_calls_per_turn   = 30
max_spend_usd_per_turn    = 0.50
max_egress_bytes_per_turn = 10000000

[gateway]
enabled              = true
http_port            = 8443
confirmation_timeout = "5m"

[[gateway.channels]]
type = "rest"

[soul]
path  = "{{.SoulPath}}"
scope = "default"

[scheduler]
enabled       = true
tick_interval = "1m"
claim_lease   = "5m"

[audit.local]
enabled     = true
path        = "{{.AuditDir}}/audit.jsonl"
max_size_mb = 100
max_files   = 10

[audit.raft]
enabled = true

[discovery]
broadcast = false

[observability]
tracing_exporter = "stdout"

[config]
watch       = true
debounce_ms = 1500
`

func writeConfigTOML(path string, ans initAnswers, dataDir, auditDir, caCert, nodeCert, nodeKey, providerKeyVar string) error {
	tmpl, err := template.New("config").Parse(configTemplate)
	if err != nil {
		return fmt.Errorf("parse template: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create %q: %w", path, err)
	}
	defer f.Close()
	return tmpl.Execute(f, map[string]string{
		"DataDir":          dataDir,
		"AuditDir":         auditDir,
		"CACert":           caCert,
		"NodeCert":         nodeCert,
		"NodeKey":          nodeKey,
		"ProviderLabel":    ans.ProviderLabel,
		"ProviderEndpoint": ans.ProviderEndpoint,
		"ProviderModel":    ans.ProviderModel,
		"ProviderKeyVar":   providerKeyVar,
		"SoulPath":         filepath.Join(ans.Dir, "SOUL.md"),
	})
}
