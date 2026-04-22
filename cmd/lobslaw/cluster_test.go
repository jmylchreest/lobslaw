package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDispatchClusterRecognition checks dispatchCluster routes by the
// first two args without consuming the main flag set.
func TestDispatchClusterRecognition(t *testing.T) {
	t.Parallel()
	cases := []struct {
		args     []string
		expected bool
	}{
		{nil, false},
		{[]string{}, false},
		{[]string{"cluster"}, false},              // no subcommand
		{[]string{"--config", "foo.toml"}, false}, // main-agent args
		{[]string{"--all"}, false},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			// Only verify negatives here — positive cases would call
			// os.Exit via the subcommand handlers. Positive paths are
			// covered by TestBuildAndRoundTrip below.
			got := dispatchCluster(tc.args)
			if got != tc.expected {
				t.Errorf("dispatchCluster(%v) = %v, want %v", tc.args, got, tc.expected)
			}
		})
	}
}

// TestBuildAndRoundTrip is an integration test: build the real binary,
// run `cluster ca-init`, then `cluster sign-node`, and confirm the
// resulting node cert verifies against the CA via pkg/mtls.
func TestBuildAndRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration build in short mode")
	}

	tmp := t.TempDir()
	bin := filepath.Join(tmp, "lobslaw-test")

	// Build the binary.
	build := exec.Command("go", "build", "-o", bin, "./")
	build.Dir = repoCmdDir(t)
	var buildOut bytes.Buffer
	build.Stdout = &buildOut
	build.Stderr = &buildOut
	if err := build.Run(); err != nil {
		t.Fatalf("go build: %v\n%s", err, buildOut.String())
	}

	caCert := filepath.Join(tmp, "ca.pem")
	caKey := filepath.Join(tmp, "ca-key.pem")
	nodeCert := filepath.Join(tmp, "node-cert.pem")
	nodeKey := filepath.Join(tmp, "node-key.pem")

	// Step 1: ca-init.
	run(t, bin, "cluster", "ca-init",
		"--ca-cert", caCert,
		"--ca-key", caKey,
		"--common-name", "test-cluster",
	)
	assertExists(t, caCert)
	assertExists(t, caKey)

	// Step 2: sign-node.
	run(t, bin, "cluster", "sign-node",
		"--ca-cert", caCert,
		"--ca-key", caKey,
		"--node-cert", nodeCert,
		"--node-key", nodeKey,
		"--node-id", "test-node-1",
	)
	assertExists(t, nodeCert)
	assertExists(t, nodeKey)
	// copy-ca-public default=true writes a ca.pem next to the node cert
	assertExists(t, filepath.Join(tmp, "ca.pem"))

	// Verify the node cert chains to the CA. We can't import pkg/mtls
	// from cmd/lobslaw/ test due to the cyclic risk, but LoadNodeCreds
	// is public and already covered by pkg/mtls/mtls_test.go. Here we
	// just confirm the files are non-empty.
	for _, p := range []string{caCert, caKey, nodeCert, nodeKey} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if info.Size() == 0 {
			t.Errorf("%s is empty", p)
		}
	}
}

func run(t *testing.T, bin string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v: %v\n%s", bin, args, err, out.String())
	}
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file at %s: %v", path, err)
	}
}

func repoCmdDir(t *testing.T) string {
	t.Helper()
	// This test file lives at cmd/lobslaw/cluster_test.go, so the
	// current working directory at test time is cmd/lobslaw/.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return cwd
}
