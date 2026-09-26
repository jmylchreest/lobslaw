package node

import (
	"context"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/skills"
	"github.com/jmylchreest/lobslaw/internal/turn"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Installed operator/signed skills are shared capabilities. Learned skills are
// private to their authoring principal, even after activation materialises them
// into the node-wide execution cache. A stale cache cannot confer authority.
func (n *Node) skillAllowed(ctx context.Context, name string) bool {
	if n.skillRegistry == nil {
		return false
	}
	skill, err := n.skillRegistry.Get(name)
	if err != nil {
		return false
	}
	if skill.Tier != skills.TierAgent {
		return true
	}
	id, ok := turn.IdentityFrom(ctx)
	if !ok || id.Principal.IsZero() || n.selfTaught == nil {
		return false
	}
	records, err := n.selfTaught.List(memory.SelfTaughtQuery{Owner: id.Principal.String(), Kind: pb.SelfTaughtKind_SELF_TAUGHT_KIND_SKILL, State: pb.SelfTaughtState_SELF_TAUGHT_STATE_ACTIVE})
	if err != nil {
		return false
	}
	for _, record := range records {
		if record.Name == name && reviewedSkillInstalled(skill, record) {
			return true
		}
	}
	return false
}
