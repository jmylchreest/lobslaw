package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
)

// dispatchCluster handles `lobslaw cluster <subcmd>` invocations.
// Returns true if it handled the args (caller should exit). Returns
// false if args don't match — caller falls through to main agent.
func dispatchCluster(args []string) bool {
	idx := findSubcmd(args, "cluster")
	if idx < 0 {
		return false
	}
	sub := args[idx+1:]
	if len(sub) == 0 {
		fmt.Fprintln(os.Stderr, "lobslaw cluster: subcommand required")
		fmt.Fprintln(os.Stderr, "available subcommands: ca-init, sign-node, export-operator")
		os.Exit(2)
	}
	switch sub[0] {
	case "ca-init":
		clusterCAInit(sub[1:])
	case "sign-node":
		clusterSignNode(sub[1:])
	case "export-operator", "sign-operator":
		// sign-operator is the old name, kept working rather than
		// broken. The new one says what it does: it EXPORTS a keypair,
		// both halves, and the private one then has to travel.
		clusterExportOperator(sub[1:], sub[0] == "sign-operator")
	case "reset":
		clusterReset(sub[1:])
	default:
		fmt.Fprintf(os.Stderr, "lobslaw cluster: unknown subcommand %q\n", sub[0])
		fmt.Fprintln(os.Stderr, "available subcommands: ca-init, sign-node, export-operator, reset")
		os.Exit(2)
	}
	return true
}

func clusterCAInit(args []string) {
	fs := flag.NewFlagSet("cluster ca-init", flag.ExitOnError)
	caCert := fs.String("ca-cert", envOr("LOBSLAW_CA_CERT", ""), "path to write the CA public certificate")
	caKey := fs.String("ca-key", envOr("LOBSLAW_CA_KEY", ""), "path to write the CA private key")
	commonName := fs.String("common-name", "Lobslaw Cluster CA", "CA certificate Subject Common Name")
	validFor := fs.Duration("valid-for", 10*365*24*time.Hour, "CA validity duration (default 10 years)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if *caCert == "" || *caKey == "" {
		exitWith("cluster ca-init: --ca-cert and --ca-key are required (or set LOBSLAW_CA_CERT and LOBSLAW_CA_KEY)")
	}

	if err := ensureDir(*caCert); err != nil {
		exitWith(err.Error())
	}
	if err := ensureDir(*caKey); err != nil {
		exitWith(err.Error())
	}

	certPEM, keyPEM, err := mtls.GenerateCA(mtls.CAOpts{
		CommonName: *commonName,
		ValidFor:   *validFor,
	})
	if err != nil {
		exitWith(fmt.Sprintf("generate CA: %v", err))
	}

	if err := mtls.WriteCAFiles(*caCert, *caKey, certPEM, keyPEM); err != nil {
		exitWith(fmt.Sprintf("write CA files: %v", err))
	}

	fmt.Printf("CA generated:\n  cert: %s\n  key : %s\n", *caCert, *caKey)
	fmt.Printf("Valid for %s.\n", *validFor)
	fmt.Println("Distribute the CA public cert (and key, if you want self-signing on each node) via your secret mechanism of choice.")
}

func clusterSignNode(args []string) {
	fs := flag.NewFlagSet("cluster sign-node", flag.ExitOnError)
	cfgPath := fs.String("config", envOr("LOBSLAW_CONFIG", ""), "path to config.toml; pre-fills --ca-cert/--node-cert/--node-key from [cluster.mtls]")
	caCert := fs.String("ca-cert", envOr("LOBSLAW_CA_CERT", ""), "path to the CA public certificate")
	caKey := fs.String("ca-key", envOr("LOBSLAW_CA_KEY", ""), "path to the CA private key (consumed only by this subcommand; intentionally not in runtime config)")
	nodeCert := fs.String("node-cert", envOr("LOBSLAW_NODE_CERT", ""), "path to write the signed node certificate")
	nodeKey := fs.String("node-key", envOr("LOBSLAW_NODE_KEY", ""), "path to write the node private key")
	nodeID := fs.String("node-id", "", "node identifier (cert CN/SAN); defaults to the short hostname so the cert binds to the host running it")
	validFor := fs.Duration("valid-for", 365*24*time.Hour, "node certificate validity duration")
	copyCA := fs.Bool("copy-ca-public", true, "also copy the CA public cert next to the node cert for the main container")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if *cfgPath != "" {
		cfg, err := config.Load(config.LoadOptions{Path: *cfgPath, SkipEnv: true})
		if err != nil {
			exitWith(fmt.Sprintf("cluster sign-node: load --config %q: %v", *cfgPath, err))
		}
		if *caCert == "" {
			*caCert = cfg.Cluster.MTLS.CACert
		}
		if *nodeCert == "" {
			*nodeCert = cfg.Cluster.MTLS.NodeCert
		}
		if *nodeKey == "" {
			*nodeKey = cfg.Cluster.MTLS.NodeKey
		}
		// Runtime config never carries the CA private key. Probe the
		// init-flow layout: ca-key.pem next to ca.pem.
		if *caKey == "" && *caCert != "" {
			guess := filepath.Join(filepath.Dir(*caCert), "ca-key.pem")
			if _, err := os.Stat(guess); err == nil {
				*caKey = guess
			}
		}
	}

	missing := []string{}
	if *caCert == "" {
		missing = append(missing, "--ca-cert")
	}
	if *caKey == "" {
		missing = append(missing, "--ca-key")
	}
	if *nodeCert == "" {
		missing = append(missing, "--node-cert")
	}
	if *nodeKey == "" {
		missing = append(missing, "--node-key")
	}
	if len(missing) > 0 {
		exitWith(fmt.Sprintf("cluster sign-node: missing required flags: %v (or pass --config to read paths from [cluster.mtls])", missing))
	}
	if *nodeID == "" {
		*nodeID = derivedNodeID()
	}

	if err := ensureDir(*nodeCert); err != nil {
		exitWith(err.Error())
	}
	if err := ensureDir(*nodeKey); err != nil {
		exitWith(err.Error())
	}

	ca, key, err := mtls.LoadCA(*caCert, *caKey)
	if err != nil {
		exitWith(fmt.Sprintf("load CA: %v", err))
	}

	certPEM, keyPEM, err := mtls.SignNodeCert(ca, key, mtls.SignOpts{
		NodeID:   *nodeID,
		ValidFor: *validFor,
	})
	if err != nil {
		exitWith(fmt.Sprintf("sign node cert: %v", err))
	}

	if err := mtls.WriteNodeFiles(*nodeCert, *nodeKey, certPEM, keyPEM); err != nil {
		exitWith(fmt.Sprintf("write node files: %v", err))
	}

	if *copyCA {
		dstCA := filepath.Join(filepath.Dir(*nodeCert), "ca.pem")
		caPEM, err := os.ReadFile(*caCert)
		if err != nil {
			exitWith(fmt.Sprintf("read CA cert for copy: %v", err))
		}
		if err := mtls.WriteCAPublic(dstCA, caPEM); err != nil {
			exitWith(fmt.Sprintf("copy CA public cert: %v", err))
		}
	}

	fmt.Printf("Signed node certificate for %q:\n  cert: %s\n  key : %s\n", *nodeID, *nodeCert, *nodeKey)
	fmt.Printf("Valid for %s.\n", *validFor)
}

// clusterReset wipes a node's raft state (raft.db + snapshots/) and
// optionally state.db so a stale-orphan node can rejoin or rebootstrap.
// Refuses to run if the data dir is locked by a running lobslaw
// process (raftboltdb takes an exclusive lock on raft.db; the rm
// would succeed but the running node would keep its in-memory state).
func clusterReset(args []string) {
	fs := flag.NewFlagSet("cluster reset", flag.ExitOnError)
	cfgPath := fs.String("config", envOr("LOBSLAW_CONFIG", ""), "path to config.toml; reads [cluster] data_dir for the wipe target")
	dataDir := fs.String("data-dir", "", "explicit data dir (overrides --config)")
	includeState := fs.Bool("include-state", false, "also wipe state.db (memory FSM); leave false to preserve memory across the reset where possible")
	yes := fs.Bool("yes", false, "skip the confirmation prompt; required for non-interactive use")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	dir := *dataDir
	if dir == "" {
		if *cfgPath == "" {
			exitWith("cluster reset: either --data-dir or --config is required")
		}
		cfg, err := config.Load(config.LoadOptions{Path: *cfgPath, SkipEnv: true})
		if err != nil {
			exitWith(fmt.Sprintf("load --config %q: %v", *cfgPath, err))
		}
		dir = cfg.Cluster.DataDir
	}
	if dir == "" {
		exitWith("cluster reset: data dir is empty (config has no [cluster] data_dir, and no --data-dir provided)")
	}

	targets := []string{
		filepath.Join(dir, "raft.db"),
		filepath.Join(dir, "snapshots"),
	}
	if *includeState {
		targets = append(targets, filepath.Join(dir, "state.db"))
	}

	existing := []string{}
	for _, t := range targets {
		if _, err := os.Stat(t); err == nil {
			existing = append(existing, t)
		}
	}
	if len(existing) == 0 {
		fmt.Println("cluster reset: nothing to remove (data dir is already clean)")
		return
	}

	// Lock probe: if a running node has raft.db open, raftboltdb
	// holds an exclusive flock. Try to grab it; if we can't, refuse
	// to wipe — operator should stop the node first.
	if probe := os.MkdirAll(dir, 0o755); probe != nil {
		exitWith(fmt.Sprintf("ensure data dir: %v", probe))
	}
	raftDB := filepath.Join(dir, "raft.db")
	if _, err := os.Stat(raftDB); err == nil {
		f, err := os.OpenFile(raftDB, os.O_RDWR, 0)
		if err != nil {
			exitWith(fmt.Sprintf("open raft.db for lock probe: %v (is a node currently running?)", err))
		}
		_ = f.Close()
	}

	if !*yes {
		fmt.Println("about to remove:")
		for _, t := range existing {
			fmt.Println("  -", t)
		}
		fmt.Println("re-run with --yes to confirm.")
		return
	}

	for _, t := range existing {
		if err := os.RemoveAll(t); err != nil {
			exitWith(fmt.Sprintf("remove %q: %v", t, err))
		}
		fmt.Println("removed:", t)
	}
	fmt.Println("cluster reset complete. next start will join via seed_nodes if reachable, else solo-bootstrap (when [cluster] bootstrap=true).")
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func ensureDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create directory %q: %w", dir, err)
	}
	return nil
}

func exitWith(msg string) {
	fmt.Fprintln(os.Stderr, "lobslaw:", msg)
	os.Exit(1)
}

// clusterExportOperator issues a credential for a PERSON, keypair and all.
//
// Distinct from sign-node because a node certificate is the wrong
// thing to hand somebody: it carries ServerAuth, so a laptop holding
// one can present itself as a cluster member. Revoking one operator
// would also mean rotating a node's identity, and every action they
// took would be attributed to a host rather than to them.
func clusterExportOperator(args []string, legacyName bool) {
	if legacyName {
		// A deprecation notice, not a refusal. Somebody with this in a
		// runbook should get the credential they asked for and learn
		// the new name at the same time.
		fmt.Fprintln(os.Stderr,
			"note: `cluster sign-operator` is now `cluster export-operator` — "+
				"renamed because it hands over a PRIVATE KEY, which then has to travel. "+
				"`lobslaw enrol` avoids that; see the operator-credentials docs.")
	}
	fs := flag.NewFlagSet("cluster export-operator", flag.ExitOnError)
	cfgPath := fs.String("config", envOr("LOBSLAW_CONFIG", ""),
		"path to config.toml; pre-fills --ca-cert from [cluster.mtls]")
	caCert := fs.String("ca-cert", envOr("LOBSLAW_CA_CERT", ""), "path to the CA public certificate")
	caKey := fs.String("ca-key", envOr("LOBSLAW_CA_KEY", ""),
		"path to the CA private key (consumed only by this subcommand)")
	out := fs.String("out", "", "directory to write operator.pem, operator-key.pem and ca.pem into")
	validFor := fs.Duration("valid-for", 90*24*time.Hour,
		"validity duration; shorter than a node's by default because a person's credential travels")
	rest, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		os.Exit(2)
	}
	if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
		exitWith("cluster export-operator: exactly one operator name is required\n\n" +
			"  lobslaw cluster export-operator alice --out ~/.config/lobslaw/prod")
	}
	name := strings.TrimSpace(rest[0])

	if *cfgPath != "" {
		cfg, err := config.Load(config.LoadOptions{Path: *cfgPath, SkipEnv: true})
		if err != nil {
			exitWith(fmt.Sprintf("cluster export-operator: load --config %q: %v", *cfgPath, err))
		}
		if *caCert == "" {
			*caCert = cfg.Cluster.MTLS.CACert
		}
		// The runtime config never carries the CA private key. Probe
		// the init-flow layout, as sign-node does.
		if *caKey == "" && *caCert != "" {
			guess := filepath.Join(filepath.Dir(*caCert), "ca-key.pem")
			if _, err := os.Stat(guess); err == nil {
				*caKey = guess
			}
		}
	}
	if *caCert == "" || *caKey == "" {
		exitWith("cluster export-operator: --ca-cert and --ca-key are required " +
			"(or pass --config to read them from [cluster.mtls])")
	}
	if *out == "" {
		exitWith("cluster export-operator: --out is required; it names the directory the operator " +
			"will point --context at")
	}

	certPath := filepath.Join(*out, "operator.pem")
	keyPath := filepath.Join(*out, "operator-key.pem")
	if err := ensureDir(certPath); err != nil {
		exitWith(err.Error())
	}

	ca, key, err := mtls.LoadCA(*caCert, *caKey)
	if err != nil {
		exitWith(fmt.Sprintf("load CA: %v", err))
	}
	certPEM, keyPEM, err := mtls.SignOperatorCert(ca, key, mtls.SignOpts{
		NodeID:   name,
		ValidFor: *validFor,
	})
	if err != nil {
		exitWith(fmt.Sprintf("sign operator cert: %v", err))
	}
	if err := mtls.WriteNodeFiles(certPath, keyPath, certPEM, keyPEM); err != nil {
		exitWith(fmt.Sprintf("write operator files: %v", err))
	}

	// The CA public cert travels with it. An operator needs it to
	// verify the cluster, and a credential they cannot use without
	// hunting down a second file is one they will copy carelessly.
	caPEM, err := os.ReadFile(*caCert)
	if err != nil {
		exitWith(fmt.Sprintf("read CA cert for copy: %v", err))
	}
	if err := mtls.WriteCAPublic(filepath.Join(*out, "ca.pem"), caPEM); err != nil {
		exitWith(fmt.Sprintf("copy CA public cert: %v", err))
	}

	fmt.Printf("Signed an OPERATOR certificate for %q:\n", name)
	fmt.Printf("  cert: %s\n  key : %s\n  ca  : %s\n",
		certPath, keyPath, filepath.Join(*out, "ca.pem"))
	fmt.Printf("Valid for %s.\n\n", *validFor)
	fmt.Println("This credential administers the cluster and CANNOT take part in replication:")
	fmt.Println("  - client authentication only, so nothing can serve with it")
	fmt.Println("  - refused on the raft transport, enforced at the server")
	fmt.Println("Revoking it does not require rotating any node's identity.")
	fmt.Println()
	fmt.Println("This command EXPORTS a private key, which now has to reach that laptop somehow.")
	fmt.Println("`lobslaw enrol` avoids that: the key is generated on the laptop and never moves.")
	fmt.Println("Use this one for bootstrap — the first operator has nobody to approve them.")

	// The credential is useless until the operator can point a command
	// at it, and the alternative to this is four flags on every
	// invocation — which becomes a shell alias, which is this file with
	// none of the checking.
	fmt.Printf("\nTo use it without passing four flags, add this to %s:\n\n%s\n",
		contextsPath(), operatorContextSnippet(certPath, keyPath, filepath.Join(*out, "ca.pem")))
	fmt.Println("Then: lobslaw --context CLUSTER_NAME memory list")
}

// operatorContextSnippet renders a contexts.toml block for a
// freshly-signed credential. The cluster name and address are left as
// placeholders because neither is known here: this command signs a
// certificate, and the same certificate is valid at every node.
func operatorContextSnippet(cert, key, ca string) string {
	return fmt.Sprintf(`  [contexts.CLUSTER_NAME]
  addr    = "NODE_HOST:PORT"
  ca_cert = %q
  cert    = %q
  key     = %q
`, ca, cert, key)
}
