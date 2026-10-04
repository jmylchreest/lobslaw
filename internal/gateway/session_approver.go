package gateway

import "context"

// SessionApprover records "approved for the rest of this conversation"
// grants. The compute SessionApprovals type satisfies this; the
// interface lives here so channels do not import the agent package.
type SessionApprover interface {
	Grant(ctx context.Context, action, resource string) bool
	DurableGrantErr(ctx context.Context, action, resource string) error
	Granted(ctx context.Context, action, resource string) bool
}
