package node

import (
	"context"
	"fmt"
	"time"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/skills"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Loading imported skills from the cluster store.
//
// The store is the authority; the cache is where a skill can be
// executed, because `invoker.buildPolicy` grants Landlock read+exec on
// a DIRECTORY and the store cannot be exec'd.
//
// Reconciled on the same interval as the self-taught cache and for the
// same reasons: every node serves turns so every node needs the cache,
// two nodes materialising the same set produce byte-identical
// directories, and the store has no change feed worth building one
// for.

// skillSigning resolves the operator's signing stance.
//
// Read here for the first time. `skills.signing_policy` and
// `trusted_publishers` have been parsed, documented and validated
// since the config existed, and nothing consumed either: the mount
// watcher calls Registry.Scan, which is SigningOff, so a deployment
// setting signing_policy = "require" got no verification at all. The
// same shape as min_trust_tier before it was enforced — a setting that
// reads as working.
func (n *Node) skillSigning() (skills.SigningPolicy, *skills.Verifier, error) {
	policy := skills.ParseSigningPolicy(n.cfg.Skills.SigningPolicy)
	// The deprecated boolean only speaks when the tri-state is silent,
	// so an operator who set both does not find the older one winning.
	if n.cfg.Skills.SigningPolicy == "" && n.cfg.Skills.RequireSigned {
		policy = skills.SigningRequire
	}
	if policy == skills.SigningOff {
		return policy, nil, nil
	}

	verifier := skills.NewVerifier()
	if path := n.cfg.Skills.TrustedPublishers; path != "" {
		if err := verifier.LoadTrustedPublishersFile(path); err != nil {
			return policy, nil, fmt.Errorf("skills: trusted publishers %q: %w", path, err)
		}
	}
	// A require policy with no keys can never admit anything. Refused
	// rather than left to fail per-skill at scan time, where it reads
	// as "every skill is broken" instead of "no publisher is trusted".
	if policy == skills.SigningRequire && verifier.Count() == 0 {
		return policy, nil, fmt.Errorf(
			"skills: signing_policy is %q but no trusted publishers are configured, "+
				"so every skill would be refused; set skills.trusted_publishers", policy)
	}
	return policy, verifier, nil
}

// startSkillStoreLoader materialises imported skills and keeps the
// registry in step.
func (n *Node) startSkillStoreLoader(ctx context.Context) error {
	if n.skillStore == nil || n.skillRegistry == nil || n.materialiser == nil {
		return nil
	}
	// The policy and the service registration moved to
	// wireSkillStore, which runs before Serve. Both are read from
	// *Node here so the loaders hold exactly the stance the service
	// does.
	if err := n.loadStoredSkills(); err != nil {
		n.log.Error("skills: initial store load failed", "err", err)
	}
	go func() {
		t := time.NewTicker(materialiseInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := n.loadStoredSkills(); err != nil {
					n.log.Warn("skills: store load failed", "err", err)
				}
			}
		}
	}()
	n.log.Info("skills: loading imported skills from the cluster store",
		"root", n.materialiser.ImportedRoot(), "signing_policy", n.skillSigningPolicy)
	return nil
}

// loadStoredSkills makes the imported cache and the registry match the
// store's active set.
func (n *Node) loadStoredSkills() error {
	active, err := n.skillStore.Active()
	if err != nil {
		return err
	}
	converted, missing := n.storedSkills(active)
	for name, why := range missing {
		// A record whose blobs are gone is a skill that cannot run.
		// Named rather than skipped silently, because the operator's
		// next question is which one.
		n.log.Warn("skills: an imported skill could not be assembled",
			"skill", name, "reason", why)
	}

	res, err := n.materialiser.MaterialiseStored(converted)
	if err != nil {
		return err
	}
	// Pruned first, for the same reason the agent side does it: the
	// registry holds a candidate per directory, and one removed from
	// disk but still registered is a skill the index advertises and
	// nothing can read.
	for _, dir := range res.Pruned {
		n.skillRegistry.Remove(dir)
	}
	for name, why := range res.Refused {
		n.log.Warn("skills: an imported skill could not be materialised",
			"skill", name, "reason", why)
	}
	for _, err := range n.skillRegistry.ScanImported(
		n.materialiser.ImportedRoot(), n.skillSigningPolicy, n.skillVerifier) {
		n.log.Warn("skills: imported skill failed to register", "err", err)
	}
	// Last, so a dev override is registered after the thing it
	// overrides. Order does not decide the winner — tier does — but a
	// scan that ran first would log its override before the skill it
	// overrides existed, which reads as a warning about nothing.
	n.loadDevSource()
	return nil
}

// storedSkills resolves each record's blobs into content.
//
// A record whose blobs are missing is dropped whole rather than
// materialised with holes. A skill missing its handler is not a
// degraded skill, it is one that fails at invoke — and the failure
// would name the interpreter rather than the missing blob.
func (n *Node) storedSkills(records []*lobslawv1.SkillRecord) ([]skills.StoredSkill, map[string]string) {
	out := make([]skills.StoredSkill, 0, len(records))
	missing := map[string]string{}

	for _, rec := range records {
		files := make(map[string][]byte, len(rec.GetFiles()))
		var failed string
		for rel, digest := range rec.GetFiles() {
			content, err := n.skillStore.Blob(digest)
			if err != nil {
				failed = fmt.Sprintf("%s: %v", rel, err)
				break
			}
			files[rel] = content
		}
		if failed != "" {
			missing[rec.GetName()] = failed
			continue
		}
		out = append(out, skills.StoredSkill{
			Name:         rec.GetName(),
			Version:      rec.GetVersion(),
			ManifestYAML: rec.GetManifestYaml(),
			ManifestSig:  rec.GetManifestSig(),
			Files:        files,
		})
	}
	return out, missing
}

// wireSkillStore constructs the store. Raft-gated like everything else
// that writes to the log.
func (n *Node) wireSkillStore() error {
	if n.raft == nil || n.store == nil {
		return nil
	}
	store, err := memory.NewSkillStore(n.raft, n.store)
	if err != nil {
		return fmt.Errorf("skill store: %w", err)
	}
	n.skillStore = store

	// Resolved and registered HERE, during wiring, because Start()
	// calls Serve immediately and gRPC makes RegisterService after
	// Serve a FATAL — it kills the process, it does not return an
	// error.
	//
	// This used to live in startSkillStoreLoader, which runs after
	// Serve and is gated on the materialiser. The materialiser
	// refused a relative data_dir, so on every documented config it
	// never started, the loader returned early, and the registration
	// never ran. Fixing the path made the registration reachable and
	// the node died on boot — the crash had been latent behind a
	// failure, which is the worst place for one to hide.
	//
	// The policy still gates the service, as it must: a zero-valued
	// policy is SigningOff, the most permissive, on a node that may
	// be configured to require signatures.
	policy, verifier, err := n.skillSigning()
	if err != nil {
		return err
	}
	n.skillSigningPolicy, n.skillVerifier = policy, verifier
	if n.server != nil {
		lobslawv1.RegisterSkillServiceServer(n.server, &skillService{
			store: n.skillStore, policy: policy, verifier: verifier,
		})
	}
	return nil
}
