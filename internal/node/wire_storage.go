package node

import (
	"context"
	"fmt"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
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
		req := &lobslawv1.AddMountRequest{
			Mount: &lobslawv1.StorageMount{
				Label:  m.Label,
				Type:   m.Type,
				Path:   m.Path,
				Bucket: m.Bucket,
			},
		}
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
		n.mountResolver = compute.NewMountResolver()
	}
	for _, m := range n.cfg.Storage.Mounts {
		if m.Label == "" || m.Type != "local" || m.Path == "" {
			continue
		}
		mode, err := compute.ParseMountMode(m.Mode)
		if err != nil {
			n.log.Warn("storage mount has invalid mode; defaulting to read-only",
				"label", m.Label, "mode", m.Mode, "error", err)
			mode = compute.MountMode{Read: true}
		}
		n.mountResolver.Register(m.Label, m.Path, mode, m.Excludes)
	}
	compute.SetActiveMountResolver(n.mountResolver)
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
