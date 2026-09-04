package skills

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"golang.org/x/mod/semver"

	"github.com/jmylchreest/lobslaw/internal/sandbox"
)

// ErrSkillNotFound fires when Get is asked about a skill that isn't
// registered. Callers translate to gRPC / HTTP "not found" in the
// channel layer.
var ErrSkillNotFound = errors.New("skills: skill not found")

// ErrNotExecutable is returned when something asks the invoker to run
// a skill that has no code — a prose skill. Its own error rather than
// a generic failure, because a caller can act on it: read the body.
var ErrNotExecutable = errors.New("skills: skill has nothing to execute")

// Registry holds the live set of skills indexed by name. Multiple
// storage mounts can expose skills with the same name — the registry
// resolves via semver-highest-wins so a mount shipping an older
// version doesn't shadow a production one.
type Registry struct {
	mu sync.RWMutex
	// byName holds the currently winning Skill per name.
	byName map[string]*Skill
	// candidates tracks every version from every source so removal
	// can fall back to the next-highest rather than losing the name.
	candidates map[string][]*Skill
	// policySink receives any policy.d/ files shipped alongside a
	// skill's manifest. When nil the policy-loading path is skipped
	// entirely — useful for tests that don't care about the
	// sandbox layer. compute.Registry satisfies the interface.
	policySink sandbox.PolicySink
	log        *slog.Logger
	// scanned fingerprints directories already parsed, so a scan that
	// finds nothing changed does no parsing. See scancache.go.
	scanned map[string]string
}

// NewRegistry constructs an empty registry with the given logger.
// Nil logger → slog.Default(). Equivalent to NewRegistryWithPolicy
// under SigningOff — no preference between signed + unsigned.
func NewRegistry(log *slog.Logger) *Registry {
	return NewRegistryWithPolicy(log, SigningOff)
}

// SetPolicySink wires the sandbox policy receiver. When non-nil,
// every skill discovered during Scan/ScanWithPolicy is inspected
// for a policy.d/ subtree; if one exists, its tool policies are
// loaded via sandbox.LoadSkillPolicies and applied to the sink.
// Safe to call once at boot before Scan runs; re-setting mid-run
// is racy and not supported.
func (r *Registry) SetPolicySink(sink sandbox.PolicySink) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.policySink = sink
}

// NewRegistryWithPolicy exists for callers that pass a signing policy.
//
// The policy no longer affects winner-selection, and the parameter is
// kept only so existing call sites need not change. Precedence is
// tier-first now: a verified signature is a fact about provenance, so
// it outranks an unsigned skill whatever the policy says to DO about
// signatures. Under SigningOff nothing is verified, so nothing reaches
// the signed tier and the order is what it always was.
//
// The old preferSigned field is gone rather than left set-and-unread —
// a field that implies behaviour it no longer has is worse than no
// field.
func NewRegistryWithPolicy(log *slog.Logger, _ SigningPolicy) *Registry {
	if log == nil {
		log = slog.Default()
	}
	return &Registry{
		byName:     make(map[string]*Skill),
		candidates: make(map[string][]*Skill),
		log:        log,
	}
}

// Put adds or replaces a candidate skill. Recomputes the winning
// entry. Same-manifest-SHA re-puts are idempotent — they update the
// candidate list but don't change the winner.
func (r *Registry) Put(skill *Skill) {
	// The capability floor, enforced here as well as at the parse
	// entry point. Put is reachable from anywhere, and a rule that
	// only one caller applies is a rule that a second caller silently
	// does not — so an agent-tier skill asking to widen the
	// deployment's surface is refused whatever route it took.
	if skill.Tier == TierAgent {
		if err := checkAgentFloor(&skill.Manifest); err != nil {
			r.log.Error("skills: refusing an agent-authored skill",
				"skill", skill.Manifest.Name, "err", err)
			return
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	name := skill.Manifest.Name

	// Replace any prior candidate with the same ManifestDir — that's
	// "the file at this path changed" — then re-rank.
	list := r.candidates[name]
	replaced := false
	for i, c := range list {
		if c.ManifestDir == skill.ManifestDir {
			list[i] = skill
			replaced = true
			break
		}
	}
	if !replaced {
		list = append(list, skill)
	}
	r.candidates[name] = list
	r.recomputeWinnerLocked(name)
}

// Remove drops every candidate sourced from manifestDir. If that
// leaves the name with no candidates the name is unregistered;
// otherwise the winner is recomputed over what remains.
func (r *Registry) Remove(manifestDir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forgetScan(manifestDir)
	for name, list := range r.candidates {
		kept := make([]*Skill, 0, len(list))
		for _, c := range list {
			if c.ManifestDir != manifestDir {
				kept = append(kept, c)
			}
		}
		if len(kept) == len(list) {
			continue
		}
		if len(kept) == 0 {
			delete(r.candidates, name)
			delete(r.byName, name)
			continue
		}
		r.candidates[name] = kept
		r.recomputeWinnerLocked(name)
	}
}

func (r *Registry) Get(name string) (*Skill, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.byName[name]
	if !ok {
		return nil, ErrSkillNotFound
	}
	return s, nil
}

// List returns a snapshot of all registered (winning) skills,
// sorted alphabetically by name. Safe for concurrent iteration by
// the caller — the returned slice is a fresh copy.
func (r *Registry) List() []*Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Skill, 0, len(r.byName))
	for _, s := range r.byName {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.Name < out[j].Manifest.Name })
	return out
}

// recomputeWinnerLocked picks the winning candidate for name under
// candidateBeats: tier, then version, then directory. Every stage is
// deterministic, so two replicas holding the same candidates pick the
// same winner. Caller must hold r.mu.
func (r *Registry) recomputeWinnerLocked(name string) {
	list := r.candidates[name]
	if len(list) == 0 {
		delete(r.byName, name)
		return
	}
	best := list[0]
	for _, c := range list[1:] {
		if r.candidateBeats(c, best) {
			best = c
		}
	}
	r.byName[name] = best
}

// candidateBeats returns true when c should replace best.
//
// Order of preference:
//  1. Higher TIER wins: signed > operator > agent.
//  2. Within a tier, higher semver wins.
//  3. Same tier and version: lexicographic ManifestDir.
//
// Tier is checked before version, and that ordering is the point.
// Version-first would let an unsigned v2 beat a signed v1 — defensible
// while only an operator could write a skill, and a privilege-
// escalation path the moment the agent can author one: name your skill
// after a signed one, set version 99.0.0, win.
//
// So a version bump cannot promote a skill past its provenance.
//
// The escape hatch for an operator who wants to override a signed
// skill locally is a dev source that wins outright, NOT bumping a
// version — because a rule that can be beaten by editing a number is
// not a rule.
func (r *Registry) candidateBeats(c, best *Skill) bool {
	if ct, bt := tierOf(c), tierOf(best); ct != bt {
		return ct > bt
	}
	cmp := compareVersion(c.Manifest.Version, best.Manifest.Version)
	if cmp > 0 {
		return true
	}
	if cmp < 0 {
		return false
	}
	return c.ManifestDir < best.ManifestDir
}

// compareVersion compares two semver strings, tolerating missing
// "v" prefixes. Non-semver versions sort lexicographically — not
// perfect but better than a hard error on bad manifest input.
func compareVersion(a, b string) int {
	va, vb := semverize(a), semverize(b)
	if semver.IsValid(va) && semver.IsValid(vb) {
		return semver.Compare(va, vb)
	}
	if a == b {
		return 0
	}
	if a < b {
		return -1
	}
	return 1
}

func semverize(v string) string {
	if v == "" {
		return ""
	}
	if v[0] == 'v' {
		return v
	}
	return "v" + v
}

// Scan walks root for "*/manifest.yaml" and Puts each parsed skill.
// Equivalent to ScanWithPolicy(root, SigningOff, nil) — see that
// function for the signature-aware form.
func (r *Registry) Scan(root string) []error {
	return r.ScanWithPolicy(root, SigningOff, nil)
}

// ScanWithPolicy is the production scan entry point: every skill
// subdir under root is parsed with the given policy + verifier.
// Rejections from ParseWithPolicy (missing-required-signature,
// invalid-signature) surface as errors. When a policy sink is
// configured AND the skill dir contains policy.d/, its tool
// policies are applied via sandbox.LoadSkillPolicies with
// ownership restricted to the skill's own name (the MVP model —
// manifests don't yet declare additional owned tools).
func (r *Registry) ScanWithPolicy(root string, policy SigningPolicy, verifier *Verifier) []error {
	var errs []error
	entries, err := os.ReadDir(root)
	if err != nil {
		return []error{err}
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		skill, err := ParseWithPolicy(dir, policy, verifier)
		if err != nil {
			if _, statErr := os.Stat(filepath.Join(dir, "manifest.yaml")); os.IsNotExist(statErr) {
				continue
			}
			r.log.Warn("skills: parse failed", "dir", dir, "err", err)
			errs = append(errs, err)
			continue
		}
		r.Put(skill)
		if err := r.loadSkillPolicies(skill); err != nil {
			r.log.Warn("skills: policy.d load failed",
				"skill", skill.Name(), "err", err)
			errs = append(errs, err)
		}
	}
	return errs
}

// ScanAgent loads the materialised self-taught cache, whose layout is
// <root>/<name>/<version>/manifest.yaml — one level deeper than the
// operator skills directory, because the cache holds a version
// directory that a rollback replaces wholesale.
//
// Separate from Scan rather than a flag on it, and it is not a
// convenience. Everything it finds is tagged TierAgent and passed
// through the capability floor: a manifest is a capability request,
// and one that arrived by this route was written by the agent whatever
// it says about itself. A shared scan with a tier parameter would put
// that decision in the caller, where getting it wrong is a one-word
// mistake that grants an agent-authored skill operator authority.
//
// No policy.d either. A skill the agent wrote does not get to install
// tool policy — the floor refuses the capabilities a policy would
// grant, so honouring the directory would be a second door to the same
// place.
func (r *Registry) ScanAgent(root string) []error {
	var errs []error
	names, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return []error{err}
	}
	for _, n := range names {
		if !n.IsDir() || strings.HasPrefix(n.Name(), ".") {
			continue
		}
		versions, err := os.ReadDir(filepath.Join(root, n.Name()))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, v := range versions {
			if !v.IsDir() || strings.HasPrefix(v.Name(), ".") {
				continue
			}
			dir := filepath.Join(root, n.Name(), v.Name())
			if r.unchanged(dir) {
				continue
			}
			skill, err := ParseAgentSkill(dir)
			if err != nil {
				if _, statErr := os.Stat(filepath.Join(dir, "manifest.yaml")); os.IsNotExist(statErr) {
					continue
				}
				r.log.Warn("skills: agent-authored skill failed to load", "dir", dir, "err", err)
				errs = append(errs, err)
				continue
			}
			r.Put(skill)
		}
	}
	return errs
}

// ScanImported loads the materialised store cache, whose layout is
// <root>/<name>/<version>/manifest.yaml — the same two-level shape as
// the agent cache, and a different subtree.
//
// Parsed WITH the signing policy, unlike ScanAgent. That is the whole
// difference between the two: a skill here came from an import, may
// carry a detached signature, and its tier is derived from whether
// that signature verified. A skill there was written by the agent and
// is capped whatever it claims.
//
// policy.d/ IS honoured here, again unlike the agent path. An operator
// who imported a skill shipping tool policy meant to install it; the
// agent-authored path refuses because the capability floor already
// refuses the capabilities such a policy would grant.
func (r *Registry) ScanImported(root string, policy SigningPolicy, verifier *Verifier) []error {
	var errs []error
	names, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return []error{err}
	}
	for _, n := range names {
		if !n.IsDir() || strings.HasPrefix(n.Name(), ".") {
			continue
		}
		versions, err := os.ReadDir(filepath.Join(root, n.Name()))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, v := range versions {
			if !v.IsDir() || strings.HasPrefix(v.Name(), ".") {
				continue
			}
			dir := filepath.Join(root, n.Name(), v.Name())
			if r.unchanged(dir) {
				continue
			}
			skill, err := ParseWithPolicy(dir, policy, verifier)
			if err != nil {
				if _, statErr := os.Stat(filepath.Join(dir, "manifest.yaml")); os.IsNotExist(statErr) {
					continue
				}
				r.log.Warn("skills: imported skill failed to load", "dir", dir, "err", err)
				errs = append(errs, err)
				continue
			}
			r.Put(skill)
			if err := r.loadSkillPolicies(skill); err != nil {
				r.log.Warn("skills: policy.d load failed", "skill", skill.Name(), "err", err)
				errs = append(errs, err)
			}
		}
	}
	return errs
}

// loadSkillPolicies calls sandbox.LoadSkillPolicies against the
// skill's policy.d/ subtree when one exists. No-op when no sink is
// configured OR the skill ships no policy.d.
func (r *Registry) loadSkillPolicies(skill *Skill) error {
	r.mu.RLock()
	sink := r.policySink
	r.mu.RUnlock()
	if sink == nil {
		return nil
	}
	policyDir := filepath.Join(skill.ManifestDir, "policy.d")
	if _, err := os.Stat(policyDir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	_, err := sandbox.LoadSkillPolicies(
		policyDir,
		[]string{skill.Name()},
		sink,
		sandbox.LoadOptions{Logger: r.log},
	)
	if err != nil {
		return err
	}
	return nil
}

// Watch is GONE.
//
// It registered mount skills directly, which was the second authority
// R18 calls self-inflicted complexity: a skill could be installed
// because a file existed on one node, and the store's answer to "what
// is installed" would differ from the registry's.
//
// The mount is now an import source — internal/node walks it, imports
// into the store, and the store is materialised and scanned. Deleting
// this rather than leaving it unused is deliberate: a method that
// quietly reintroduces two authorities is exactly the thing somebody
// calls because it looks like what they want.
