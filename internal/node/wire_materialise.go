package node

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/jmylchreest/lobslaw/internal/skills"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Getting what the agent taught itself into the prompt.
//
// The store is the authority; the filesystem is where a skill can
// actually be read. This closes that gap on every node, and it is
// deliberately NOT leader-gated. Every node serves turns, so every
// node needs the cache — a leader-only materialiser would leave the
// assistant differently capable depending on which node answered,
// which is the most confusing failure mode available.
//
// It is safe to run everywhere because the cache is derived state, not
// a write: two nodes materialising the same ACTIVE set produce byte
// identical directories, and neither can affect the other.

// materialiseInterval is how often the cache is reconciled against the
// store.
//
// A poll rather than a watch. The store has no change feed, and adding
// one so a per-node cache can refresh faster would be a large
// mechanism for a small gain: a skill approved a minute ago being
// available a minute later is not a problem anybody has. The boot pass
// is the one that matters, and that is not on a timer.
const materialiseInterval = time.Minute

// skillsCacheDirName sits under DataDir alongside the raft log, not in
// a storage mount. A mount is shared and durable; this is neither. It
// is per-node scratch that can be deleted at any time, and putting it
// somewhere an operator might back up would invite restoring it — over
// a store that had moved on.
const skillsCacheDirName = "skills-cache"

// skillsCacheRoot is where the per-node skill cache lives.
//
// Absolute, because the materialiser insists on it — rightly, since it
// PRUNES directories, and a relative root resolved against a working
// directory that moved would prune somewhere else.
//
// Resolved rather than required, because data_dir is relative in every
// example config and raft already resolves the same string against the
// same working directory. Refusing it here made a node with the
// documented relative data_dir log an error at boot and carry on with
// self-learning enabled and nothing able to materialise — the exact
// silent contradiction startMaterialiser warns about, arriving through
// a different door.
func skillsCacheRoot(dataDir string) (string, error) {
	root, err := filepath.Abs(filepath.Join(dataDir, skillsCacheDirName))
	if err != nil {
		return "", fmt.Errorf("resolve data_dir %q: %w", dataDir, err)
	}
	return root, nil
}

// startMaterialiser reconciles the self-taught cache on boot and then
// on a ticker. Returns nil having started nothing when there is
// nothing to materialise.
func (n *Node) startMaterialiser(ctx context.Context) error {
	if n.selfTaught == nil || n.skillRegistry == nil {
		return nil
	}
	if n.cfg.DataDir == "" {
		// Not an error: a node with no DataDir has no raft either, so
		// there is no store to materialise from. Said out loud anyway,
		// because "self-learning is on and nothing ever appears" is
		// otherwise a silent contradiction.
		n.log.Info("skills: no data dir, so nothing self-taught can be materialised")
		return nil
	}

	// Resolved because the materialiser insists on an absolute root —
	// rightly, since it PRUNES directories, and a relative root
	// resolved against a CWD that moved would prune somewhere else.
	//
	// But data_dir is relative in every example config, and raft
	// already resolves the same string against the same working
	// directory. Refusing it here made a node with a relative
	// data_dir log an error at boot and carry on with self-learning
	// enabled and nothing able to materialise — the exact silent
	// contradiction the comment above this function warns about,
	// arriving through a different door.
	root, err := skillsCacheRoot(n.cfg.DataDir)
	if err != nil {
		return fmt.Errorf("skills materialiser: %w", err)
	}
	mat, err := skills.NewMaterialiser(root, n.log)
	if err != nil {
		return fmt.Errorf("skills materialiser: %w", err)
	}
	n.materialiser = mat

	if err := n.materialiseOnce(); err != nil {
		// Logged, not fatal. A node that cannot write its skill cache
		// is a node with fewer skills, not a broken node, and refusing
		// to boot over it would take the assistant down to protect a
		// cache that is disposable by design.
		n.log.Error("skills: initial materialisation failed", "root", root, "err", err)
	}

	go func() {
		t := time.NewTicker(materialiseInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := n.materialiseOnce(); err != nil {
					n.log.Warn("skills: materialisation failed", "err", err)
				}
			}
		}
	}()
	n.log.Info("skills: self-taught materialiser started",
		"root", root, "interval", materialiseInterval)
	return nil
}

// materialiseOnce makes the cache and the registry match the ACTIVE
// set.
func (n *Node) materialiseOnce() error {
	active, err := n.selfTaught.Active(lobslawv1.SelfTaughtKind_SELF_TAUGHT_KIND_SKILL)
	if err != nil {
		return err
	}
	res, err := n.materialiser.Materialise(artefactsFor(active))
	if err != nil {
		return err
	}

	// Pruned first. The registry holds a candidate per directory, so a
	// directory removed from disk that is still registered is a skill
	// the index advertises and nothing can read — worse than either
	// having it or not.
	for _, dir := range res.Pruned {
		n.skillRegistry.Remove(dir)
	}
	for name, why := range res.Refused {
		n.log.Warn("skills: a self-taught artefact could not be materialised",
			"artefact", name, "reason", why)
	}
	for _, err := range n.skillRegistry.ScanAgent(n.materialiser.Root()) {
		n.log.Warn("skills: agent-authored skill failed to register", "err", err)
	}
	n.reportShadowed(active)
	return nil
}

// reportShadowed says when an agent-authored skill lost its name to an
// operator's.
//
// Tier-first precedence already decides this correctly and silently.
// Silently is the problem: the artefact is ACTIVE in the store, listed
// by `lobslaw learned`, and never once reaches the prompt. Somebody
// looking at the store has no way to tell, and the answer — an
// operator skill of the same name outranks it — is not one they would
// arrive at by looking harder.
func (n *Node) reportShadowed(active []*lobslawv1.SelfTaughtRecord) {
	for _, rec := range active {
		winner, err := n.skillRegistry.Get(rec.Name)
		if err != nil || winner.Tier == skills.TierAgent {
			continue
		}
		// Once per node lifetime per name, not once a minute.
		if _, seen := n.shadowedSkills.LoadOrStore(rec.Name, struct{}{}); seen {
			continue
		}
		n.log.Info("skills: a self-taught skill is shadowed by one of higher provenance",
			"skill", rec.Name, "winner_dir", winner.ManifestDir)
	}
}

// artefactsFor translates store records into what the materialiser
// writes. The translation lives here because this is the only place
// that already knows both packages.
func artefactsFor(records []*lobslawv1.SelfTaughtRecord) []skills.Artefact {
	out := make([]skills.Artefact, 0, len(records))
	for _, rec := range records {
		// The PENDING revision is deliberately not materialised. A
		// refinement waiting on approval is a proposal, and writing it
		// to the cache would put it in the prompt — which is precisely
		// what proposing instead of applying exists to prevent.
		out = append(out, skills.Artefact{
			Name:        rec.Name,
			Description: rec.Description,
			Body:        rec.Body,
			Version:     rec.Version,
			Files:       rec.Files,
		})
	}
	return out
}
