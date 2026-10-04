package memory

import (
	"context"
	"errors"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

const integrationStateMaxBytes = 128 << 10

// IntegrationStateStore is internal connector state, not an agent-readable
// memory surface. Callers enforce ownership before exposing or acting on data.
// OAuth callbacks can resolve opaque one-use state before a user session exists.
type IntegrationStateStore struct {
	raft  *RaftNode
	store *Store
	key   crypto.Key
}

func NewIntegrationStateStore(raft *RaftNode, store *Store, key crypto.Key) *IntegrationStateStore {
	return &IntegrationStateStore{raft: raft, store: store, key: key}
}

func (s *IntegrationStateStore) Get(ctx context.Context, id string) (*lobslawv1.IntegrationStateRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := s.store.Get(BucketIntegrationState, id)
	if err != nil {
		return nil, err
	}
	var rec lobslawv1.IntegrationStateRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, err
	}
	if rec.ExpiresAt == nil || !rec.ExpiresAt.AsTime().After(time.Now()) {
		return nil, errors.New("integration: state expired")
	}
	rec.Data, err = crypto.Open(s.key, rec.Data)
	return &rec, err
}

// Save is compare-and-swap against rec.Revision. Callers must reload after a
// successful save; retaining the old revision cannot silently overwrite state.
func (s *IntegrationStateStore) Save(ctx context.Context, rec *lobslawv1.IntegrationStateRecord) error {
	if rec == nil || rec.Id == "" || rec.Owner == "" || rec.Kind == "" || rec.ExpiresAt == nil || len(rec.Data) > integrationStateMaxBytes || s.key == (crypto.Key{}) {
		return errors.New("integration: invalid state")
	}
	after := proto.Clone(rec).(*lobslawv1.IntegrationStateRecord)
	var err error
	after.Data, err = crypto.Seal(s.key, rec.Data)
	if err != nil {
		return err
	}
	entry := &lobslawv1.LogEntry{Op: lobslawv1.LogOp_LOG_OP_CLAIM, Id: rec.Id, ExpectedRevision: &rec.Revision, ExpectedClaimer: rec.ClaimedBy, Payload: &lobslawv1.LogEntry_IntegrationState{IntegrationState: after}}
	raw, err := proto.Marshal(entry)
	if err != nil {
		return err
	}
	res, err := s.raft.ApplyOrForward(ctx, raw, credentialApplyTimeout)
	if err != nil {
		return err
	}
	if err, ok := res.(error); ok {
		return err
	}
	return nil
}

// Sweep removes expired short-lived state. IDs are random and never reused;
// callers cannot renew an expired record through Get.
func (s *IntegrationStateStore) Sweep(ctx context.Context, now time.Time) error {
	var expired []string
	if err := s.store.ForEach(BucketIntegrationState, func(id string, raw []byte) error {
		var rec lobslawv1.IntegrationStateRecord
		if err := proto.Unmarshal(raw, &rec); err != nil {
			return err
		}
		if rec.ExpiresAt == nil || !rec.ExpiresAt.AsTime().After(now) {
			expired = append(expired, id)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, id := range expired {
		entry := &lobslawv1.LogEntry{Op: lobslawv1.LogOp_LOG_OP_DELETE, Id: id, Payload: &lobslawv1.LogEntry_IntegrationState{IntegrationState: &lobslawv1.IntegrationStateRecord{Id: id}}}
		raw, err := proto.Marshal(entry)
		if err != nil {
			return err
		}
		res, err := s.raft.ApplyOrForward(ctx, raw, credentialApplyTimeout)
		if err != nil {
			return err
		}
		if err, ok := res.(error); ok {
			return err
		}
	}
	return nil
}
