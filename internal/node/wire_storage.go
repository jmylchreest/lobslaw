package node

import (
	"context"
	"fmt"
	"maps"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/tools"
	"github.com/jmylchreest/lobslaw/pkg/config"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func (n *Node) seedStorageMountsFromConfig(ctx context.Context) error {
	// Populate the MountResolver independent of Raft leadership —
	// every node needs local label → path resolution for fs tools,
	// even followers that aren't responsible for propagating
	// writes.
	n.refreshMountResolver()

	if n.storageSvc == nil || n.store == nil {
		return nil
	}
	if !n.raft.IsLeader() {
		return nil
	}
	seeded := []string{}
	for _, m := range n.cfg.Storage.Mounts {
		if m.Label == "" || m.Type == "" {
			continue
		}
		if _, err := n.store.Get(memory.BucketStorageMounts, m.Label); err == nil {
			continue
		}
		req := &lobslawv1.AddMountRequest{Mount: mountToProto(m)}
		if _, err := n.storageSvc.AddMount(ctx, req); err != nil {
			return fmt.Errorf("seed mount %q: %w", m.Label, err)
		}
		n.log.Debug("storage: seeded mount from config",
			"label", m.Label, "type", m.Type, "path", m.Path)
		seeded = append(seeded, m.Label)
	}
	if len(seeded) > 0 {
		n.log.Info("storage: seeded mounts from config", "count", len(seeded), "labels", seeded)
	}
	return nil
}

// refreshMountResolver rebuilds the local mount-label → path map
// from [[storage.mounts]]. Called during boot + when config hot-
// reloads. Only handles local-type mounts today (the fs builtins
// are local-filesystem anyway); remote-backend mounts (S3, rclone)
// are addressed by a different surface.
func (n *Node) refreshMountResolver() {
	if n.mountResolver == nil {
		n.mountResolver = tools.NewMountResolver()
	}
	for _, m := range n.cfg.Storage.Mounts {
		if m.Label == "" || m.Type != "local" || m.Path == "" {
			continue
		}
		mode, err := tools.ParseMountMode(m.Mode)
		if err != nil {
			n.log.Warn("storage mount has invalid mode; defaulting to read-only",
				"label", m.Label, "mode", m.Mode, "error", err)
			mode = tools.MountMode{Read: true}
		}
		n.mountResolver.Register(m.Label, m.Path, mode, m.Excludes)
	}
	tools.SetActiveMountResolver(n.mountResolver)
}

// seedDefaultPolicyRules writes a platform-trusted allow rule for
// every stdlib builtin tool. Without these, the default-deny posture
// blocks current_time (and every future stdlib addition) — the LLM
// calls the tool correctly, the executor denies it, and the model
// apologises to the user. Platform builtins are Go code inside the
// trust boundary; denying them by default is theater.
//
// Rules are idempotent: deterministic IDs of the form
// "lobslaw-builtin-<tool>", Priority 1 so operator rules at higher
// priority win. An operator who wants to deny current_time for a
// specific scope writes subject=<scope> effect=deny priority=10.
//
// Only runs on the Raft leader — followers get these entries via
// replication. No-op on nodes without a Raft stack.

// mountToProto converts one [[storage.mounts]] entry into the
// replicated form the backends are handed.
//
// Everything past label/type/path/bucket has to survive this
// conversion. Dropping it does not degrade a mount, it makes one
// impossible: the
// rclone backend refuses a mount whose remote is empty and the NFS
// backend refuses one with no server or export — and there were no
// config keys to set any of them with. So a `type = "rclone"` or
// `type = "nfs"` mount declared in TOML could never start, and the
// endpoint and credentials written beside it reached nothing.
func mountToProto(m config.StorageMountConfig) *lobslawv1.StorageMount {
	return &lobslawv1.StorageMount{
		Label:   m.Label,
		Type:    m.Type,
		Path:    m.Path,
		Bucket:  m.Bucket,
		Server:  m.Server,
		Export:  m.Export,
		Remote:  m.Remote,
		Options: maps.Clone(m.Options),

		// Cloned, not aliased. The config is hot-reloadable and this
		// map is about to be replicated; sharing the backing store
		// would let a reload mutate a raft payload.

	}
}
