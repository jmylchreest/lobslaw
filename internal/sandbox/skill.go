package sandbox

import (
	"fmt"
	"slices"
)

// LoadSkillPolicies is the canonical entry point for the Phase 8
// skills system to install per-tool policies shipped alongside a
// skill. Semantics on top of LoadPolicyDir:
//
//  1. Standard LoadPolicyDir runs against skillPolicyDir (typically
//     <skill_install_dir>/policy.d) with the same integrity checks —
//     skills are installed by the operator, so operator-owned files
//     are what we expect; the UID/mode check applies uniformly.
//
//  2. **Ownership guard** — a skill may only ship policies for tools
//     IT registers. Policies for tool names outside ownedTools are
//     refused with a visible log warning; the file-rejection path
//     from LoadPolicyDir already records them in result.Rejected.
//
//  3. Every accepted policy is applied to sink via SetPolicy —
//     Registry.SetPolicy semantics: last-write-wins. Since skills
//     are loaded BEFORE the operator's /etc/lobslaw/policy.d
//     equivalent at boot, any overlap between the two resolves to
//     operator-authored policy winning, which is the intended trust
//     posture.
//
// ownedTools is typically built from the skill manifest (skill.toml)
// which declares the tool list at install time. An empty ownedTools
// slice is treated as "skill declares no tools" and every file is
// rejected.
func LoadSkillPolicies(skillPolicyDir string, ownedTools []string, sink PolicySink, opts LoadOptions) (*LoadResult, error) {
	if sink == nil {
		return nil, fmt.Errorf("LoadSkillPolicies: sink is required")
	}
	if opts.Logger == nil {
		opts = opts.withDefaults()
	}

	result, err := LoadPolicyDir(skillPolicyDir, opts)
	if err != nil {
		return nil, err
	}

	owned := make(map[string]struct{}, len(ownedTools))
	for _, name := range ownedTools {
		owned[name] = struct{}{}
	}

	accepted := make(map[string]*Policy, len(result.Policies))
	for name, policy := range result.Policies {
		if _, ok := owned[name]; !ok {
			opts.Logger.Warn("sandbox: skill ships policy for tool it doesn't own; skipping",
				"skill_policy_dir", skillPolicyDir,
				"tool", name)
			result.Rejected = append(result.Rejected, name)
			continue
		}
		sink.SetPolicy(name, policy)
		accepted[name] = policy
	}
	result.Policies = accepted

	// Preserve deterministic ordering in Rejected so logs / tests
	// can compare stably.
	slices.Sort(result.Rejected)
	return result, nil
}
