package policy

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jmylchreest/lobslaw/internal/memory"
)

// Every local node contributes its own roots; constructing another node
// must not replace the first node's protections. This is shared by the
// executor, builtins and approval minting, just like the static floor.
var raftPaths = struct {
	sync.RWMutex
	roots map[raftOwnedPaths]int
}{roots: make(map[raftOwnedPaths]int)}

type raftOwnedPaths struct {
	root, log, snapshots string
}

// ProtectRaftPaths registers the data directory used by a local Raft node.
// It identifies owned files, not configurable policy: no operator allow
// rule can turn off the protection. Release only after the node stops.
func ProtectRaftPaths(dataDir string) (release func(), err error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("raft data directory is empty")
	}
	root, err := resolveExistingPath(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve raft data directory: %w", err)
	}
	logPath, err := resolveExistingPath(filepath.Join(root, memory.RaftLogFile))
	if err != nil {
		return nil, fmt.Errorf("resolve raft log: %w", err)
	}
	snapshots, err := resolveExistingPath(filepath.Join(root, memory.SnapshotDir))
	if err != nil {
		return nil, fmt.Errorf("resolve raft snapshots: %w", err)
	}
	owned := raftOwnedPaths{root: root, log: logPath, snapshots: snapshots}
	raftPaths.Lock()
	raftPaths.roots[owned]++
	raftPaths.Unlock()
	return sync.OnceFunc(func() {
		raftPaths.Lock()
		defer raftPaths.Unlock()
		raftPaths.roots[owned]--
		if raftPaths.roots[owned] == 0 {
			delete(raftPaths.roots, owned)
		}
	}), nil
}

func checkRaftPath(path string) error {
	raftPaths.RLock()
	defer raftPaths.RUnlock()
	for owned := range raftPaths.roots {
		if path == owned.root || path == owned.log || path == filepath.Join(owned.root, memory.RaftLogFile) {
			return &HardlineError{Pattern: "raft-log", Detail: "this is lobslaw's own Raft log"}
		}
		if withinRaftSnapshots(path, owned.snapshots) || withinRaftSnapshots(path, filepath.Join(owned.root, memory.SnapshotDir)) {
			return &HardlineError{Pattern: "raft-snapshot", Detail: "this is lobslaw's Raft snapshot store"}
		}
	}
	return nil
}

func withinRaftSnapshots(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}
