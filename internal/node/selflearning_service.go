package node

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Approving what the agent taught itself, on a running cluster.
//
// The offline CLI opens state.db directly, which means bbolt's
// exclusive lock, which means the node must be stopped. That is a fine
// shape for forensics and a terrible one for approval: approving a
// proposal is a routine act, and requiring a service outage to perform
// it guarantees nobody does it — after which propose mode is a queue
// that only fills, and the expiry the curator now applies is doing all
// the deciding.
//
// Deliberately not reachable from a conversation. Approval decides
// whether the agent's own suggestion becomes something it follows, and
// routing that through the channel the agent writes in puts the
// request and the approval on one wire. The in-channel path is a
// separate decision, and it belongs on the durable confirmation
// records rather than here.

// selfLearningService serves the self-taught store over gRPC.
type selfLearningService struct {
	lobslawv1.UnimplementedSelfLearningServiceServer
	store *memory.SelfTaughtStore
}

// errNoStore is what every method returns when self-learning is off.
//
// FailedPrecondition rather than Unimplemented, and it says which
// setting: an operator who disabled self-learning and then wonders why
// approval fails should be told the reason rather than left to
// conclude the build is missing the feature.
func (s *selfLearningService) errNoStore() error {
	return status.Error(codes.FailedPrecondition,
		"self-learning is off on this cluster (self_learning.mode); there is no store to read")
}

func (s *selfLearningService) ListArtefacts(_ context.Context, req *lobslawv1.ListArtefactsRequest) (*lobslawv1.ListArtefactsResponse, error) {
	if s.store == nil {
		return nil, s.errNoStore()
	}
	records, err := s.store.List(memory.SelfTaughtQuery{
		State:    req.GetState(),
		Archived: req.GetArchived(),
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &lobslawv1.ListArtefactsResponse{Artefacts: records}, nil
}

func (s *selfLearningService) ApproveArtefact(ctx context.Context, req *lobslawv1.ApproveArtefactRequest) (*lobslawv1.ApproveArtefactResponse, error) {
	id, by, err := validateApprove(req)
	if err != nil {
		return nil, err
	}
	if s.store == nil {
		return nil, s.errNoStore()
	}
	rec, err := s.store.Approve(ctx, id, by)
	if err != nil {
		return nil, artefactError(err)
	}
	return &lobslawv1.ApproveArtefactResponse{Artefact: rec}, nil
}

func (s *selfLearningService) DecideRevision(ctx context.Context, req *lobslawv1.DecideRevisionRequest) (*lobslawv1.DecideRevisionResponse, error) {
	id, by, err := validateDecide(req)
	if err != nil {
		return nil, err
	}
	if s.store == nil {
		return nil, s.errNoStore()
	}
	var rec *lobslawv1.SelfTaughtRecord
	if req.GetAccept() {
		rec, err = s.store.ApprovePending(ctx, id, by)
	} else {
		rec, err = s.store.RejectPending(ctx, id)
	}
	if err != nil {
		return nil, artefactError(err)
	}
	return &lobslawv1.DecideRevisionResponse{Artefact: rec}, nil
}

func (s *selfLearningService) ArchiveArtefact(ctx context.Context, req *lobslawv1.ArchiveArtefactRequest) (*lobslawv1.ArchiveArtefactResponse, error) {
	id, reason, err := validateArchive(req)
	if err != nil {
		return nil, err
	}
	if s.store == nil {
		return nil, s.errNoStore()
	}
	if err := s.store.Archive(ctx, id, reason); err != nil {
		return nil, artefactError(err)
	}
	return &lobslawv1.ArchiveArtefactResponse{}, nil
}

func (s *selfLearningService) RestoreArtefact(ctx context.Context, req *lobslawv1.RestoreArtefactRequest) (*lobslawv1.RestoreArtefactResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, errIDRequired()
	}
	if s.store == nil {
		return nil, s.errNoStore()
	}
	rec, err := s.store.Restore(ctx, id)
	if err != nil {
		return nil, artefactError(err)
	}
	return &lobslawv1.RestoreArtefactResponse{Artefact: rec}, nil
}

// Argument validation runs BEFORE the store check, in every method.
//
// A malformed request is malformed whether or not self-learning is
// enabled, and reporting "the store is off" for a request that names
// no artefact sends the operator to the wrong problem. It also makes
// these testable without a raft, which is why they are functions
// rather than inline blocks.

func errIDRequired() error {
	return status.Error(codes.InvalidArgument, "id is required")
}

func validateApprove(req *lobslawv1.ApproveArtefactRequest) (id, by string, err error) {
	id = strings.TrimSpace(req.GetId())
	by = strings.TrimSpace(req.GetApprovedBy())
	if id == "" {
		return "", "", errIDRequired()
	}
	// Required, not defaulted to the calling node. An approval nobody
	// is named on is one nobody can be asked about later, and "the
	// cluster approved it" is not an answer.
	if by == "" {
		return "", "", status.Error(codes.InvalidArgument,
			"approved_by is required; an approval nobody is named on is one nobody can be asked about")
	}
	return id, by, nil
}

func validateDecide(req *lobslawv1.DecideRevisionRequest) (id, by string, err error) {
	id = strings.TrimSpace(req.GetId())
	if id == "" {
		return "", "", errIDRequired()
	}
	// Only an acceptance needs attribution. A rejection changes nothing
	// about what the agent follows — the live artefact carries on
	// exactly as it was, and the thing discarded was never in force.
	if !req.GetAccept() {
		return id, "", nil
	}
	by = strings.TrimSpace(req.GetDecidedBy())
	if by == "" {
		return "", "", status.Error(codes.InvalidArgument,
			"decided_by is required when accepting a refinement")
	}
	return id, by, nil
}

func validateArchive(req *lobslawv1.ArchiveArtefactRequest) (id, reason string, err error) {
	id = strings.TrimSpace(req.GetId())
	if id == "" {
		return "", "", errIDRequired()
	}
	reason = strings.TrimSpace(req.GetReason())
	// The archive listing exists to say why each thing is in it. A
	// blank reason turns that into a list of things that stopped
	// mattering for reasons nobody wrote down.
	if reason == "" {
		return "", "", status.Error(codes.InvalidArgument,
			"reason is required; the archive listing says why each artefact is in it")
	}
	return id, reason, nil
}

// artefactError maps store errors onto gRPC codes.
//
// Mapped rather than passed through as Internal, because the
// difference between "no such artefact" and "that artefact is not
// awaiting approval" is the difference between a typo and a
// misunderstanding, and a CLI can only say which if the code carries
// it.
func artefactError(err error) error {
	switch {
	case errors.Is(err, memory.ErrArtefactNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, memory.ErrNotProposed), errors.Is(err, memory.ErrNoPendingRevision):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

// ListArtefactHistory returns the versions kept for rollback.
//
// Live because the offline form cannot run while the node is up:
// bbolt holds an exclusive flock on state.db, so `lobslaw learned
// history` against a running node times out on its own open. Reading
// what the agent used to think, only by stopping it, is not reading.
func (s *selfLearningService) ListArtefactHistory(_ context.Context, req *lobslawv1.ListArtefactHistoryRequest) (
	*lobslawv1.ListArtefactHistoryResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, errIDRequired()
	}
	if s.store == nil {
		return nil, s.errNoStore()
	}
	current, err := s.findArtefact(id)
	if err != nil {
		return nil, artefactError(err)
	}
	history, err := s.store.History(current.GetId())
	if err != nil {
		return nil, artefactError(err)
	}
	return &lobslawv1.ListArtefactHistoryResponse{Current: current, History: history}, nil
}

// findArtefact looks in the live set and then the archive, matching
// what the offline form does.
//
// Both, because "show me what it used to think" is asked about
// artefacts that were retired as often as ones still in force, and an
// id that resolves offline but not live would be a worse answer than
// the lock error it replaced.
func (s *selfLearningService) findArtefact(id string) (*lobslawv1.SelfTaughtRecord, error) {
	if rec, err := s.store.Get(id); err == nil {
		return rec, nil
	}
	archived, err := s.store.List(memory.SelfTaughtQuery{Archived: true})
	if err != nil {
		return nil, err
	}
	for _, rec := range archived {
		if rec.GetId() == id {
			return rec, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", memory.ErrArtefactNotFound, id)
}

// RollbackArtefact puts a prior version back in force.
//
// The dry run is served here rather than in the client, for the same
// reason the plan is: the client cannot read the history bucket while
// the node holds the store, so a preview it computed would be of a
// file it could not open.
func (s *selfLearningService) RollbackArtefact(ctx context.Context, req *lobslawv1.RollbackArtefactRequest) (
	*lobslawv1.RollbackArtefactResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, errIDRequired()
	}
	if req.GetVersion() == 0 {
		return nil, status.Error(codes.InvalidArgument, "version required")
	}
	if s.store == nil {
		return nil, s.errNoStore()
	}

	if !req.GetApply() {
		history, err := s.store.History(id)
		if err != nil {
			return nil, artefactError(err)
		}
		for _, rec := range history {
			if rec.GetVersion() == req.GetVersion() {
				return &lobslawv1.RollbackArtefactResponse{Artefact: rec}, nil
			}
		}
		return nil, status.Errorf(codes.NotFound, "no version %d kept for %s", req.GetVersion(), id)
	}

	rec, err := s.store.Rollback(ctx, id, req.GetVersion())
	if err != nil {
		return nil, artefactError(err)
	}
	return &lobslawv1.RollbackArtefactResponse{Artefact: rec, Applied: true}, nil
}
