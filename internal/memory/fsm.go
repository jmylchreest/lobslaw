package memory

import (
	"fmt"
	"io"
	"sync"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// FSM is the raft.FSM implementation backed by Store. Apply
// unmarshals each log entry as a LogEntry proto and dispatches
// to the appropriate bucket by payload type.
type FSM struct {
	mu    sync.RWMutex
	store *Store
}

// NewFSM wraps a Store as a Raft FSM.
func NewFSM(store *Store) *FSM {
	return &FSM{store: store}
}

// Store returns the underlying store. Intended for read-path code;
// writers go through raft.Apply, not through the store directly.
func (f *FSM) Store() *Store {
	return f.store
}

// Apply dispatches a replicated log entry. Errors are returned to
// the caller of raft.Apply via ApplyFuture.Response.
func (f *FSM) Apply(l *raft.Log) any {
	f.mu.Lock()
	defer f.mu.Unlock()

	var entry lobslawv1.LogEntry
	if err := proto.Unmarshal(l.Data, &entry); err != nil {
		return fmt.Errorf("unmarshal log entry: %w", err)
	}

	switch entry.Op {
	case lobslawv1.LogOp_LOG_OP_PUT:
		return f.applyPut(&entry)
	case lobslawv1.LogOp_LOG_OP_DELETE:
		return f.applyDelete(&entry)
	default:
		return fmt.Errorf("unknown log op: %v", entry.Op)
	}
}

func (f *FSM) applyPut(entry *lobslawv1.LogEntry) error {
	bucket, payload, err := bucketAndPayload(entry)
	if err != nil {
		return err
	}
	if entry.Id == "" {
		return fmt.Errorf("PUT %s: empty id", bucket)
	}
	bytes, err := proto.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", bucket, err)
	}
	return f.store.Put(bucket, entry.Id, bytes)
}

func (f *FSM) applyDelete(entry *lobslawv1.LogEntry) error {
	bucket, _, err := bucketAndPayload(entry)
	if err != nil {
		// DELETE is allowed to carry just the id + a typed discriminator
		// in payload (to know which bucket). If payload is absent, reject.
		return err
	}
	if entry.Id == "" {
		return fmt.Errorf("DELETE %s: empty id", bucket)
	}
	return f.store.Delete(bucket, entry.Id)
}

// bucketAndPayload maps a LogEntry's payload oneof to its bucket name
// and the concrete proto.Message. Adding a new record type requires
// wiring it both here and in buckets.go.
func bucketAndPayload(entry *lobslawv1.LogEntry) (string, proto.Message, error) {
	switch p := entry.Payload.(type) {
	case *lobslawv1.LogEntry_PolicyRule:
		return BucketPolicyRules, p.PolicyRule, nil
	case *lobslawv1.LogEntry_ScheduledTask:
		return BucketScheduledTasks, p.ScheduledTask, nil
	case *lobslawv1.LogEntry_Commitment:
		return BucketCommitments, p.Commitment, nil
	case *lobslawv1.LogEntry_AuditEntry:
		return BucketAuditEntries, p.AuditEntry, nil
	case *lobslawv1.LogEntry_VectorRecord:
		return BucketVectorRecords, p.VectorRecord, nil
	case *lobslawv1.LogEntry_EpisodicRecord:
		return BucketEpisodicRecords, p.EpisodicRecord, nil
	case *lobslawv1.LogEntry_StorageMount:
		return BucketStorageMounts, p.StorageMount, nil
	case nil:
		return "", nil, fmt.Errorf("log entry has no payload")
	default:
		return "", nil, fmt.Errorf("unknown log entry payload type: %T", p)
	}
}

// Snapshot returns a raft.FSMSnapshot that writes the entire state.db
// via bbolt's Tx.WriteTo. The snapshot is a self-consistent point-in-
// time dump at the transaction boundary.
func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return &snapshot{store: f.store}, nil
}

// Restore replaces state.db's contents with the bbolt dump read from
// rc. The Store is closed and re-opened with the restored contents.
func (f *FSM) Restore(rc io.ReadCloser) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	defer rc.Close()

	restored, err := f.store.RestoreFromSnapshot(rc)
	if err != nil {
		return err
	}
	f.store = restored
	return nil
}

// snapshot is the per-Snapshot() state captured for raft's async
// Persist call.
type snapshot struct {
	store *Store
}

// Persist writes the snapshot bytes to sink. Called by raft on its
// own goroutine; the underlying store must remain safe to read from
// concurrent Apply calls (bbolt handles this via Tx read isolation).
func (s *snapshot) Persist(sink raft.SnapshotSink) error {
	if err := s.store.WriteSnapshot(sink); err != nil {
		_ = sink.Cancel()
		return err
	}
	return sink.Close()
}

// Release is called by raft when it's done with the snapshot. bbolt
// doesn't need any release logic — the View transaction closes with
// Persist's return.
func (s *snapshot) Release() {}
