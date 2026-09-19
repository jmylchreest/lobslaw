package memory

import (
	"context"
	"crypto/sha256"
	"fmt"

	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// DecideReviewed applies a human decision to exactly the revision shown. Owner
// and revision are checked again by the store; the gateway supplies identity,
// never the model. The FSM's conditional write closes the read/write race.
func (s *SelfTaughtStore) DecideReviewed(ctx context.Context, id string, revision uint64, digest, owner string, approve bool) (*lobslawv1.SelfTaughtRecord, error) {
	current, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if owner == "" || current.Owner != owner {
		return nil, fmt.Errorf("review is not owned by this user")
	}
	if revision == 0 || current.Revision != revision || digest != SelfTaughtReviewDigest(current) {
		return nil, fmt.Errorf("%w: proposal changed; review it again", ErrClaimConflict)
	}
	if current.Pending == nil && current.State != lobslawv1.SelfTaughtState_SELF_TAUGHT_STATE_PROPOSED {
		return nil, ErrNotProposed
	}
	updated := proto.Clone(current).(*lobslawv1.SelfTaughtRecord)
	updated.UpdatedAt = timestamppb.Now()
	switch {
	case approve:
		if current.Pending != nil {
			if err := s.recordHistory(current); err != nil {
				return nil, err
			}
			updated.Body = current.Pending.Body
			updated.Files = current.Pending.Files
			updated.Description = current.Pending.Description
			updated.Version++
			updated.Pending = nil
			if err := s.embed(ctx, updated); err != nil {
				return nil, err
			}
		}
		updated.State = lobslawv1.SelfTaughtState_SELF_TAUGHT_STATE_ACTIVE
		updated.ApprovedBy = owner
		updated.ApprovedAt = updated.UpdatedAt
	case current.State == lobslawv1.SelfTaughtState_SELF_TAUGHT_STATE_PROPOSED:
		if current.Pinned {
			return nil, fmt.Errorf("unpin this proposal before denying it")
		}
		updated.State = lobslawv1.SelfTaughtState_SELF_TAUGHT_STATE_ARCHIVED
		updated.ArchivedReason = "Denied during review by " + owner
	default:
		updated.Pending = nil
	}
	if err := s.applyEntry(&lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_CLAIM, Id: id, ExpectedRevision: &revision,
		ExpectedClaimer: current.ClaimedBy,
		Payload:         &lobslawv1.LogEntry_SelfTaught{SelfTaught: updated},
	}); err != nil {
		return nil, err
	}
	updated.Revision = revision + 1
	return updated, nil
}

// applyReviewedArchive moves a proposal out of the live set atomically. A
// conditional archive must compare the LIVE revision, not an older archive.
func (f *FSM) applyReviewedArchive(entry *lobslawv1.LogEntry, rec *lobslawv1.SelfTaughtRecord) error {
	if entry.Id == "" || rec.Id != entry.Id || entry.ExpectedRevision == nil || entry.GetExpectedRevision() == 0 {
		return fmt.Errorf("review archive requires id and revision")
	}
	return f.store.loadDB().Update(func(tx *bolt.Tx) error {
		live := tx.Bucket([]byte(BucketSelfTaught))
		sealed := live.Get([]byte(entry.Id))
		if sealed == nil {
			return ErrClaimConflict
		}
		raw, err := f.store.cipher.OpenTo(nil, sealed)
		if err != nil {
			return err
		}
		var current lobslawv1.SelfTaughtRecord
		if err := proto.Unmarshal(raw, &current); err != nil {
			return err
		}
		if current.Revision != entry.GetExpectedRevision() || current.ClaimedBy != entry.ExpectedClaimer {
			return ErrClaimConflict
		}
		if current.State != lobslawv1.SelfTaughtState_SELF_TAUGHT_STATE_PROPOSED || current.Pinned {
			return ErrNotProposed
		}
		rec.Revision = current.Revision + 1
		raw, err = proto.Marshal(rec)
		if err != nil {
			return err
		}
		sealed, err = f.store.cipher.Seal(raw)
		if err != nil {
			return err
		}
		if err := tx.Bucket([]byte(BucketSelfTaughtArchive)).Put([]byte(entry.Id), sealed); err != nil {
			return err
		}
		return live.Delete([]byte(entry.Id))
	})
}

// SelfTaughtReviewDigest also binds review to content across archive restores,
// where an imported record may reuse a previous revision number.
func SelfTaughtReviewDigest(rec *lobslawv1.SelfTaughtRecord) string {
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(rec)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}
