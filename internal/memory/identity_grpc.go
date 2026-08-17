package memory

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Repointing a principal on a running cluster.
//
// `lobslaw identity rebind` wrote straight to bbolt, so it needed the
// node stopped — and if somebody had pointed it at a follower's file
// while the cluster ran, it would have written ownership no other
// replica has. That is worse than requiring the outage, and it is why
// this path replicates.

// IdentityRPC serves IdentityService.
type IdentityRPC struct {
	lobslawv1.UnimplementedIdentityServiceServer

	raft  *RaftNode
	store *Store
}

// NewIdentityRPC wires the rebind path over raft.
func NewIdentityRPC(raft *RaftNode, store *Store) *IdentityRPC {
	return &IdentityRPC{raft: raft, store: store}
}

// Rebind moves everything owned by `from` to `to`.
func (r *IdentityRPC) Rebind(ctx context.Context, req *lobslawv1.RebindRequest) (
	*lobslawv1.RebindResponse, error) {
	if r.store == nil {
		return nil, status.Error(codes.FailedPrecondition, "this node has no memory store")
	}
	from := strings.TrimSpace(req.GetFrom())
	to := strings.TrimSpace(req.GetTo())
	switch {
	case from == "" || to == "":
		return nil, status.Error(codes.InvalidArgument, "both from and to are required")
	case from == to:
		// Not a no-op worth performing quietly: somebody who typed the
		// same id twice meant something else, and running it would
		// report a confident zero.
		return nil, status.Errorf(codes.InvalidArgument,
			"from and to are the same id (%q); nothing to do", from)
	}

	plan, err := PlanRebind(r.store, from, to)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "plan rebind: %v", err)
	}

	out := &lobslawv1.RebindResponse{Conflicts: plan.Conflicts}
	for _, bucket := range plan.Buckets() {
		out.Changes = append(out.Changes, &lobslawv1.RebindBucketChange{
			Bucket: bucket, Ids: plan.Changes[bucket],
		})
	}
	if req.GetDryRun() || plan.Total() == 0 {
		return out, nil
	}

	if !r.raft.IsLeader() {
		// Refused rather than forwarded. A rebind is a long sequence of
		// writes and half of it landing through a redirect that then
		// fails is exactly the half-moved identity this reports
		// conflicts to avoid.
		return nil, status.Errorf(codes.FailedPrecondition,
			"not the raft leader; retry at %s", r.raft.LeaderAddress())
	}

	applied, err := ApplyRebindReplicated(ctx, r.raft, r.store, from, to)
	//nolint:gosec // a record count is bounded by the store, not by a caller
	out.Applied = int32(applied)
	if err != nil {
		// The count already written is on the response, but a partial
		// rebind is still an error: the caller must know it has to
		// re-run rather than assume the move completed.
		return out, status.Errorf(codes.Internal,
			"rebind wrote %d record(s) then failed: %v — re-run to finish it", applied, err)
	}
	return out, nil
}
