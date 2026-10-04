package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// ConnectorSettingsStore keeps one encrypted, CAS-protected settings record per
// canonical user and trusted connector. It is not agent-readable memory.
type ConnectorSettingsStore struct {
	raft      *RaftNode
	store     *Store
	key       crypto.Key
	connector string
}

func NewConnectorSettingsStore(raft *RaftNode, store *Store, key crypto.Key, connector string) *ConnectorSettingsStore {
	return &ConnectorSettingsStore{raft: raft, store: store, key: key, connector: connector}
}
func (s *ConnectorSettingsStore) identity(ctx context.Context) (string, string, error) {
	who, ok := turn.IdentityFrom(ctx)
	if !ok || who.Principal == "" || who.Shared || s.connector == "" {
		return "", "", errors.New("connector settings: private authenticated user required")
	}
	owner := string(who.Principal)
	sum := sha256.Sum256([]byte(s.connector + "\x00" + owner))
	return owner, hex.EncodeToString(sum[:]), nil
}
func (s *ConnectorSettingsStore) Get(ctx context.Context) (*pb.IntegrationStateRecord, error) {
	owner, id, err := s.identity(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := s.store.Get(BucketIntegrationSettings, id)
	if IsCredentialNotFound(err) {
		return &pb.IntegrationStateRecord{Id: id, Owner: owner, Kind: s.connector}, nil
	}
	if err != nil {
		return nil, err
	}
	var r pb.IntegrationStateRecord
	if err := proto.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if r.Owner != owner || r.Kind != s.connector {
		return nil, errors.New("connector settings: ownership mismatch")
	}
	r.Data, err = crypto.Open(s.key, r.Data)
	return &r, err
}
func (s *ConnectorSettingsStore) Save(ctx context.Context, r *pb.IntegrationStateRecord) error {
	owner, id, err := s.identity(ctx)
	if err != nil {
		return err
	}
	if r == nil || r.Id != id || r.Owner != owner || r.Kind != s.connector || len(r.Data) > 128<<10 || r.ExpiresAt != nil || s.key == (crypto.Key{}) {
		return errors.New("connector settings: invalid record")
	}
	after := proto.Clone(r).(*pb.IntegrationStateRecord)
	after.Data, err = crypto.Seal(s.key, r.Data)
	if err != nil {
		return err
	}
	raw, err := proto.Marshal(&pb.LogEntry{Op: pb.LogOp_LOG_OP_CLAIM, Id: id, ExpectedRevision: &r.Revision, Payload: &pb.LogEntry_IntegrationSettings{IntegrationSettings: after}})
	if err != nil {
		return err
	}
	result, err := s.raft.ApplyOrForward(ctx, raw, credentialApplyTimeout)
	if err != nil {
		return err
	}
	if failure, ok := result.(error); ok {
		return failure
	}
	return nil
}
