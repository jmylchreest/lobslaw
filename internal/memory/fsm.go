package memory

import (
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// ErrClaimConflict is returned from FSM.Apply when a LOG_OP_CLAIM
// entry's expected_claimer didn't match the record's current claim.
// Callers (the scheduler) treat this as "another node already won
// the claim — skip."
var ErrClaimConflict = errors.New("fsm: claim conflict")

// FSM is the raft.FSM implementation backed by Store. Apply
// unmarshals each log entry as a LogEntry proto and dispatches
// to the appropriate bucket by payload type.
type FSM struct {
	mu    sync.RWMutex
	store *Store

	// CALLBACK CONTRACT (applies to every *Change field below):
	//
	// Callbacks fire SYNCHRONOUSLY under f.mu after a successful Apply.
	// They MUST NOT block AND they MUST NOT acquire any lock that the
	// caller of raft.Apply might hold — doing so deadlocks because the
	// mutator is currently waiting for raft.Apply to return.
	//
	// Concrete deadlock pattern (caught by the soul-tune callback):
	//
	//   goroutine A (mutator):
	//     a.mu.Lock()              // user-side lock
	//     raft.Apply(...)          // blocks until FSM applies + replicates
	//
	//   goroutine B (FSM Apply, fired by raft):
	//     f.mu.Lock()
	//     f.<bucket>Change()       // synchronous callback
	//       a.mu.Lock()            // ← deadlock: A holds it, never releases
	//                              //   because A is waiting on raft.Apply
	//                              //   which is blocked on this Apply.
	//
	// Callback authors should either (a) take no user-side locks at all,
	// (b) defer the work onto a goroutine, or (c) use a trylock pattern
	// and skip the update when contended (the next callback fire will
	// catch up). The soul-tune callback uses (b); the storage callback
	// is safe under (a) because Reconcile takes the storage manager's
	// lock which no caller of AddMount/RemoveMount holds during apply.

	// schedulerChange is fired (if non-nil) after every successful
	// apply that touched scheduled_tasks or commitments. Lets the
	// scheduler wake on remote-originated writes without polling.
	// Nil-safe; Scheduler wires this at construction.
	schedulerChange func()

	// storageChange is fired after every successful apply that
	// touched storage_mounts. Lets the storage Service reconcile
	// the local Manager with the replicated config.
	storageChange func()

	// soulTuneChange is fired after every successful apply that
	// touched soul_tune. Lets the Adjuster refresh its in-memory
	// view so a remote leader's mutation propagates without a
	// process restart.
	soulTuneChange func()
}

// NewFSM wraps a Store as a Raft FSM.
func NewFSM(store *Store) *FSM {
	return &FSM{store: store}
}

// SetSchedulerChangeCallback registers a callback that fires after
// each FSM.Apply that touches BucketScheduledTasks or
// BucketCommitments. Passing nil clears the callback. Safe to call
// from any goroutine; the callback itself is invoked under the
// FSM's write lock so it must not block.
func (f *FSM) SetSchedulerChangeCallback(cb func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.schedulerChange = cb
}

// SetStorageChangeCallback registers a callback that fires after
// each FSM.Apply that touches BucketStorageMounts. Used by the
// storage Service to reconcile the local Manager with the
// replicated config. Same nil-safety and non-blocking rules as
// SetSchedulerChangeCallback.
func (f *FSM) SetStorageChangeCallback(cb func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.storageChange = cb
}

// SetSoulTuneChangeCallback registers a callback that fires after
// each FSM.Apply that touches BucketSoulTune. Used by the Adjuster
// to refresh its in-memory view when a remote node's mutation lands
// here via raft replication.
func (f *FSM) SetSoulTuneChangeCallback(cb func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.soulTuneChange = cb
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

	var result any
	switch entry.Op {
	case lobslawv1.LogOp_LOG_OP_PUT:
		result = f.applyPut(&entry)
	case lobslawv1.LogOp_LOG_OP_DELETE:
		result = f.applyDelete(&entry)
	case lobslawv1.LogOp_LOG_OP_CLAIM:
		result = f.applyClaim(&entry)
	default:
		return fmt.Errorf("unknown log op: %v", entry.Op)
	}

	// Fire change hooks if the touched bucket is one a subsystem
	// watches AND the apply itself succeeded (returning an error
	// leaves the store unchanged, so there's nothing to recompute).
	if err, ok := result.(error); !ok || err == nil {
		if bucket, _, berr := bucketAndPayload(&entry); berr == nil {
			switch bucket {
			case BucketScheduledTasks, BucketCommitments:
				if f.schedulerChange != nil {
					f.schedulerChange()
				}
			case BucketStorageMounts:
				if f.storageChange != nil {
					f.storageChange()
				}
			case BucketSoulTune:
				if f.soulTuneChange != nil {
					f.soulTuneChange()
				}
			}
		}
	}
	return result
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

// applyClaim is the CAS primitive: the write goes through only when
// the record's current claimed_by field matches entry.ExpectedClaimer.
//
// **CRITICAL: this MUST be deterministic.** It runs both for live
// raft.Apply calls (during normal operation) AND during raft log
// replay at restart (which can happen hours/days/weeks after the
// original entry was written). Any time-dependent logic — like
// "an expired claim counts as unclaimed" — produces different
// results between the original apply and the replay, silently
// dropping writes during replay. This was the source of the
// "commitment fires on every restart" bug: the original mark-done
// CLAIM was applied successfully on the leader (where the prior
// claim was still fresh), but on replay the prior claim looked
// expired, so the CAS failed, the mark-done payload was dropped,
// and the FSM stayed at Status=pending after every replay.
//
// The "claim expiry" semantics (a crashed node's abandoned record
// becomes available to a new claimer) belong at the SCHEDULER
// scan layer, NOT here. The scheduler's own extractClaimer in
// internal/scheduler/scheduler.go is the right place — it runs
// only at scan time on the leader and uses time.Now() correctly.
//
// Only ScheduledTaskRecord and AgentCommitment are claimable today;
// other payload types return an error so a misrouted CLAIM can't
// silently overwrite a record that doesn't support CAS.
func (f *FSM) applyClaim(entry *lobslawv1.LogEntry) error {
	bucket, newPayload, err := bucketAndPayload(entry)
	if err != nil {
		return err
	}
	if entry.Id == "" {
		return fmt.Errorf("CLAIM %s: empty id", bucket)
	}
	if bucket != BucketScheduledTasks && bucket != BucketCommitments {
		return fmt.Errorf("CLAIM %s: bucket does not support claim semantics", bucket)
	}

	raw, getErr := f.store.Get(bucket, entry.Id)
	if getErr != nil {
		if entry.ExpectedClaimer != "" {
			return fmt.Errorf("CLAIM %s/%s: record missing, expected prior claimer %q",
				bucket, entry.Id, entry.ExpectedClaimer)
		}
	} else {
		currentClaimer, err := extractClaimerExact(bucket, raw)
		if err != nil {
			return fmt.Errorf("CLAIM %s/%s: inspect current: %w", bucket, entry.Id, err)
		}
		if currentClaimer != entry.ExpectedClaimer {
			return fmt.Errorf("%w: %s/%s expected=%q current=%q",
				ErrClaimConflict, bucket, entry.Id, entry.ExpectedClaimer, currentClaimer)
		}
	}

	bytes, err := proto.Marshal(newPayload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", bucket, err)
	}
	return f.store.Put(bucket, entry.Id, bytes)
}

// extractClaimerExact pulls the current claimed_by value out of a
// serialized ScheduledTaskRecord or AgentCommitment. Returns the
// raw value WITHOUT any expiry-based reinterpretation — see the
// applyClaim doc for why this matters (replay determinism).
func extractClaimerExact(bucket string, raw []byte) (string, error) {
	switch bucket {
	case BucketScheduledTasks:
		var r lobslawv1.ScheduledTaskRecord
		if err := proto.Unmarshal(raw, &r); err != nil {
			return "", err
		}
		return r.ClaimedBy, nil
	case BucketCommitments:
		var r lobslawv1.AgentCommitment
		if err := proto.Unmarshal(raw, &r); err != nil {
			return "", err
		}
		return r.ClaimedBy, nil
	default:
		return "", fmt.Errorf("bucket %q not claimable", bucket)
	}
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
	case *lobslawv1.LogEntry_ChannelState:
		return BucketChannelState, p.ChannelState, nil
	case *lobslawv1.LogEntry_SoulTune:
		return BucketSoulTune, p.SoulTune, nil
	case *lobslawv1.LogEntry_Credential:
		return BucketCredentials, p.Credential, nil
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
