package node_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/node"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestNodeProtectsOwnedRaftPathsUntilShutdown(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := node.Config{
		NodeID:         "protected-node",
		Functions:      []types.NodeFunction{types.FunctionMemory},
		ListenAddr:     "127.0.0.1:0",
		DataDir:        filepath.Join(root, "data"),
		SnapshotTarget: "storage:test-backup",
		Creds:          signNodeCert(t, filepath.Join(root, "certs"), "protected-node"),
		MemoryKey:      mustKey(t),
	}
	n, err := node.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Shutdown(context.Background()) })
	for _, path := range []string{filepath.Join(cfg.DataDir, memory.RaftLogFile), filepath.Join(cfg.DataDir, memory.SnapshotDir, "new", "state.bin")} {
		if verdict, err := policy.CheckPath(path); verdict != policy.PathDenied {
			t.Errorf("owned path %q allowed: %v", path, err)
		}
	}
	if err := n.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if verdict, err := policy.CheckPath(filepath.Join(cfg.DataDir, memory.RaftLogFile)); verdict != policy.PathAllowed {
		t.Errorf("registration survived shutdown: %v", err)
	}
}

func TestNodeReleasesRaftPathProtectionOnFailedWiring(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Opening state.db as a database will fail after protection registers.
	if err := os.Mkdir(filepath.Join(dataDir, "state.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	n, err := node.New(node.Config{
		NodeID:         "failed-protected-node",
		Functions:      []types.NodeFunction{types.FunctionMemory},
		ListenAddr:     "127.0.0.1:0",
		DataDir:        dataDir,
		SnapshotTarget: "storage:test-backup",
		Creds:          signNodeCert(t, filepath.Join(root, "certs"), "failed-protected-node"),
		MemoryKey:      mustKey(t),
	})
	if err == nil {
		_ = n.Shutdown(context.Background())
		t.Fatal("expected state.db directory to prevent startup")
	}
	if verdict, err := policy.CheckPath(filepath.Join(dataDir, memory.RaftLogFile)); verdict != policy.PathAllowed {
		t.Errorf("failed wiring leaked registration: %v", err)
	}
}
