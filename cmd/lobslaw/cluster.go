package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/mtls"
)

// dispatchCluster handles `lobslaw cluster <subcmd>` invocations.
// Returns true if it handled the args (caller should exit). Returns
// false if args don't match — caller falls through to main agent.
func dispatchCluster(args []string) bool {
	if len(args) < 2 || args[0] != "cluster" {
		return false
	}
	switch args[1] {
	case "ca-init":
		clusterCAInit(args[2:])
	case "sign-node":
		clusterSignNode(args[2:])
	default:
		fmt.Fprintf(os.Stderr, "lobslaw cluster: unknown subcommand %q\n", args[1])
		fmt.Fprintln(os.Stderr, "available subcommands: ca-init, sign-node")
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
	caCert := fs.String("ca-cert", envOr("LOBSLAW_CA_CERT", ""), "path to the CA public certificate")
	caKey := fs.String("ca-key", envOr("LOBSLAW_CA_KEY", ""), "path to the CA private key (consumed only by this subcommand)")
	nodeCert := fs.String("node-cert", envOr("LOBSLAW_NODE_CERT", ""), "path to write the signed node certificate")
	nodeKey := fs.String("node-key", envOr("LOBSLAW_NODE_KEY", ""), "path to write the node private key")
	nodeID := fs.String("node-id", envOr("LOBSLAW_NODE_ID", ""), "node identifier (becomes the cert SAN — peer identity)")
	validFor := fs.Duration("valid-for", 365*24*time.Hour, "node certificate validity duration")
	copyCA := fs.Bool("copy-ca-public", true, "also copy the CA public cert next to the node cert for the main container")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
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
	if *nodeID == "" {
		missing = append(missing, "--node-id")
	}
	if len(missing) > 0 {
		exitWith(fmt.Sprintf("cluster sign-node: missing required flags: %v (or set equivalent LOBSLAW_* env vars)", missing))
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
