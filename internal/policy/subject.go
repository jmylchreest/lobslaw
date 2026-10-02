package policy

import (
	"fmt"
	"slices"
	"strings"
)

// matchableSubjectKinds are the subject kinds subjectMatches actually
// implements. ValidateSubject and ApprovalRules.Mint both check a
// subject against this same list before it is written anywhere, so a
// kind the engine cannot evaluate is refused rather than left as a
// rule that reads as a grant (or a deny) and matches nothing. See
// subjectMatches, which is what this list must stay in step with.
var matchableSubjectKinds = []string{"user", "role", "scope"}

// ValidateSubject reports whether subject is one subjectMatches can
// evaluate.
//
// "" and "*" both match every principal, which subjectMatches treats
// as a deliberate wildcard rather than a malformed subject, so both
// are valid here too. Anything else must be "kind:value" with kind
// in matchableSubjectKinds and a non-empty value. An empty value can
// never equal a real claim, so a rule carrying one would be exactly
// as inert as a kind the engine has never heard of.
func ValidateSubject(subject string) error {
	if subject == "" || subject == "*" {
		return nil
	}
	kind, value, ok := strings.Cut(subject, ":")
	if ok && value != "" && slices.Contains(matchableSubjectKinds, kind) {
		return nil
	}
	return fmt.Errorf("subject %q is not one the policy engine can match; supported kinds are %s, or \"*\" for everyone",
		subject, subjectKindList())
}

func subjectKindList() string {
	parts := make([]string, len(matchableSubjectKinds))
	for i, k := range matchableSubjectKinds {
		parts[i] = k + ":"
	}
	return strings.Join(parts, ", ")
}
