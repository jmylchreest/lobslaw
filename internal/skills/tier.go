package skills

// Precedence is tier-first, not version-first.
//
// Version-first — higher semver wins, signing only a tie-break at
// equal version — is defensible while nothing but an operator can
// write a skill, and stops being defensible the moment the agent can
// author one. The attack is a single line of YAML: name your skill
// after a signed one, set version 99.0.0, and it wins. Anything that
// can propose an artefact could then take over any name in the
// library.
//
// So precedence is tier-first. A higher version cannot promote a skill
// past its provenance.

// SkillTier is where a skill came from, which now decides who wins a
// contested name.
type SkillTier int

const (
	// tierUnset is the ZERO VALUE on purpose, so a Skill built as a
	// struct literal derives its tier from how it arrived rather than
	// silently defaulting to the lowest one. Making TierAgent the zero
	// value looked tidier and demoted every hand-constructed skill to
	// the bottom of the order.
	tierUnset SkillTier = iota

	// TierAgent is written by the review fork. Lowest of the real
	// tiers, deliberately: it is the only one whose author is not a
	// person.
	TierAgent
	// TierOperator is on-disk, operator-authored.
	TierOperator
	// TierSigned is a bundle whose manifest verified against a trusted
	// publisher key.
	TierSigned

	// TierDev is a skill from the operator's dev source, and it
	// outranks EVERYTHING — including signed.
	//
	// That is the point and also the danger. The escape hatch for an
	// operator who needs to override a signed skill locally has to be
	// something other than bumping a version, because a rule that can
	// be beaten by editing a number is not a rule. So it is a separate
	// source, deliberately awkward: it must be configured explicitly
	// AND the process must be started with LOBSLAW_DEV set, or the
	// node refuses to boot.
	//
	// Two gates rather than one because either alone is easy to leave
	// behind. A config file gets copied to production; an environment
	// variable gets set in a shell profile and forgotten. Both at once
	// is a coincidence somebody has to arrange.
	TierDev
)

func (t SkillTier) String() string {
	switch t {
	case TierDev:
		return "dev"
	case TierSigned:
		return "signed"
	case TierOperator:
		return "operator"
	case TierAgent:
		return "agent"
	default:
		return "underived"
	}
}

// tierOf derives a skill's tier from how it arrived.
//
// Agent is never derived — it is set explicitly by whatever
// materialises the self-taught store, because provenance-by-location
// is what establishes it and a parsed manifest carries no trace of
// having been machine-written. Anything reaching Parse came off a
// disk an operator controls.
func tierOf(s *Skill) SkillTier {
	if s.Tier != tierUnset {
		return s.Tier
	}
	if s.IsSigned {
		return TierSigned
	}
	return TierOperator
}
