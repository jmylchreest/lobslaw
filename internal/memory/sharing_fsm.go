package memory

import (
	"errors"
	"fmt"

	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/archive"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func (f *FSM) applyShareBatch(batch *lobslawv1.ShareBatch) error {
	if batch == nil || batch.ExpectedState == "" || proto.Size(batch) > 4<<20 {
		return errors.New("sharing: invalid transaction")
	}
	err := f.store.loadDB().Update(func(tx *bolt.Tx) error {
		snap, err := shareSnapshotTx(f.store, tx)
		if err != nil {
			return err
		}
		if snap.digest() != batch.ExpectedState {
			return errors.New("sharing: destination changed; preview again")
		}
		seen := make(map[string]bool)
		for _, m := range batch.Mutations {
			if m == nil {
				return errors.New("sharing: nil mutation")
			}
			bucket, ok := shareKinds[m.Kind]
			key := shareKey(m.Kind, m.Id)
			if !ok || m.Id == "" || seen[key] {
				return errors.New("sharing: invalid or duplicate mutation")
			}
			seen[key] = true
			kind, err := findArchiveKind(m.Kind)
			if err != nil {
				return err
			}
			msg := proto.Clone(kind.message)
			if err := proto.Unmarshal(m.Payload, msg); err != nil {
				return err
			}
			if len(msg.ProtoReflect().GetUnknown()) > 0 {
				return errors.New("sharing: unknown mutation fields")
			}
			if err := validateArchiveRecordID(archive.Record{Kind: m.Kind, ID: m.Id}, msg); err != nil {
				return err
			}
			if err := putArchiveValue(tx, f.store, bucket, m.Id, m.Payload); err != nil {
				return fmt.Errorf("sharing: commit: %w", err)
			}
		}
		return nil
	})
	if err == nil && len(batch.Mutations) > 0 && f.schedulerChange != nil {
		f.schedulerChange()
	}
	return err
}
