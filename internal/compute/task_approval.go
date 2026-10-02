package compute

import (
	"context"
	"errors"

	"github.com/jmylchreest/lobslaw/internal/turn"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type taskExecutionKey struct{}
type taskExecution struct {
	scope turn.TaskScope
	check func(context.Context, *pb.CheckGrantTaskApprovalRequest) (*pb.CheckGrantTaskApprovalResponse, error)
}

// WithTaskExecution binds a claimed task to this execution leg. The check must
// consult authoritative durable state, not a conversation grant cache. A nil
// check deliberately blocks execution. Callers must replace the scope for each
// child task rather than pass its parent's execution context as authority.
func WithTaskExecution(ctx context.Context, scope turn.TaskScope, check func(context.Context, *pb.CheckGrantTaskApprovalRequest) (*pb.CheckGrantTaskApprovalResponse, error)) context.Context {
	return context.WithValue(ctx, taskExecutionKey{}, &taskExecution{scope: scope, check: check})
}
func taskGrant(ctx context.Context, action, resource string) (bool, bool, error) {
	t, ok := ctx.Value(taskExecutionKey{}).(*taskExecution)
	if !ok {
		return false, false, nil
	}
	if t.check == nil || t.scope.ID == "" || t.scope.Owner == "" || t.scope.Actor == "" || t.scope.ClaimToken == "" {
		return false, true, errors.New("task execution has no durable authority")
	}
	if id, exists := turn.IdentityFrom(ctx); exists && id.Principal.String() != t.scope.Actor {
		return false, true, errors.New("task execution actor changed")
	}
	r, err := t.check(ctx, &pb.CheckGrantTaskApprovalRequest{Id: t.scope.ID, Owner: t.scope.Owner, Actor: t.scope.Actor, ClaimToken: t.scope.ClaimToken, Action: action, Resource: resource})
	if err != nil {
		return false, true, err
	}
	if r == nil {
		return false, true, errors.New("task authority returned no result")
	}
	return r.Granted, true, nil
}
func checkTaskExecution(ctx context.Context) error { _, _, err := taskGrant(ctx, "", ""); return err }
func (e *Executor) approvalGranted(ctx context.Context, action, resource string) bool {
	allowed, task, err := taskGrant(ctx, action, resource)
	if task {
		return err == nil && allowed
	}
	return e.approvals.Granted(ctx, action, resource)
}
