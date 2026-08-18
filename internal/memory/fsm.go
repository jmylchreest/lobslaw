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
	// A session append is several writes across two buckets that must
	// land together — handled before the single-record path below.
	if p, ok := entry.Payload.(*lobslawv1.LogEntry_SessionAppend); ok {
		return f.applySessionAppend(p.SessionAppend)
	}
	bucket, payload, err := bucketAndPayload(entry)
	if err != nil {
		return err
	}
	if entry.Id == "" {
		return fmt.Errorf("PUT %s: empty id", bucket)
	}
	// Derived state is computed here rather than at each producer, so a
	// new write path can't forget it. Deterministic: same embedding, same
	// float ops in the same order, same result on every replica.
	if p, ok := payload.(*lobslawv1.VectorRecord); ok && p.Norm == 0 {
		p.Norm = norm(p.Embedding)
	}
	// Revision is FSM-assigned for the same reason: a producer that
	// forgets to bump it would silently disarm every conditional write
	// against that record. Deterministic — it is a function of what is
	// already in the store, so every replica computes the same value.
	if err := f.bumpRevision(bucket, entry.Id, payload); err != nil {
		return err
	}
	bytes, err := proto.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", bucket, err)
	}
	return f.store.Put(bucket, entry.Id, bytes)
}

// bumpRevision sets payload's revision to one past what is stored,
// for the record types that carry one. Everything else is untouched.
func (f *FSM) bumpRevision(bucket, id string, payload proto.Message) error {
	if _, carries := revisionOf(payload); !carries {
		return nil
	}
	current, err := f.currentRevision(bucket, id)
	if err != nil {
		return err
	}
	setRevision(payload, current+1)
	return nil
}

// revisionOf and setRevision are a type switch rather than an
// interface because protoc-gen-go emits getters but no setters.
func revisionOf(m proto.Message) (uint64, bool) {
	switch p := m.(type) {
	case *lobslawv1.ScheduledTaskRecord:
		return p.Revision, true
	case *lobslawv1.AgentCommitment:
		return p.Revision, true
	case *lobslawv1.SessionLease:
		return p.Revision, true
	case *lobslawv1.PromptRecord:
		return p.Revision, true
	case *lobslawv1.PinnedMemory:
		return p.Revision, true
	case *lobslawv1.SelfTaughtRecord:
		return p.Revision, true
	case *lobslawv1.SessionGrant:
		return p.Revision, true
	case *lobslawv1.SkillRecord:
		return p.Revision, true
	case *lobslawv1.SkillBlob:
		return p.Revision, true
	default:
		return 0, false
	}
}

func setRevision(m proto.Message, rev uint64) {
	switch p := m.(type) {
	case *lobslawv1.ScheduledTaskRecord:
		p.Revision = rev
	case *lobslawv1.AgentCommitment:
		p.Revision = rev
	case *lobslawv1.SessionLease:
		p.Revision = rev
	case *lobslawv1.PromptRecord:
		p.Revision = rev
	case *lobslawv1.PinnedMemory:
		p.Revision = rev
	case *lobslawv1.SelfTaughtRecord:
		p.Revision = rev
	case *lobslawv1.SessionGrant:
		p.Revision = rev
	case *lobslawv1.SkillRecord:
		p.Revision = rev
	case *lobslawv1.SkillBlob:
		p.Revision = rev
	}
}

// currentRevision reads the stored revision for a key. A missing
// record is revision 0, so the first write lands on 1 — and a caller
// can never legitimately claim to have read revision 0, which keeps
// "I read nothing" from passing as "I read the current state".
func (f *FSM) currentRevision(bucket, id string) (uint64, error) {
	raw, err := f.store.Get(bucket, id)
	if err != nil {
		return 0, nil //nolint:nilerr // absent record = revision 0
	}
	existing, err := decodeClaimable(bucket, raw)
	if err != nil {
		return 0, fmt.Errorf("read current revision of %s/%s: %w", bucket, id, err)
	}
	return existing.GetRevision(), nil
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
	// Deleting a session must also drop its transcript, else the
	// message records are orphaned in their bucket forever — nothing
	// else knows the key range. The prefix scan reads only committed
	// store state, so it replays identically.
	if bucket == BucketSessions {
		return f.purgeSession(entry.Id)
	}
	return f.store.Delete(bucket, entry.Id)
}

// applySessionAppend writes one turn atomically-in-effect: evictions
// first, then message bodies, then the index record last.
//
// The index record goes last on purpose. It is the only thing that
// advertises the retained range, so a crash midway through leaves
// messages that no reader will look at (harmless, reclaimed by the
// next trim) rather than an index promising messages that aren't
// there (a torn read).
func (f *FSM) applySessionAppend(rec *lobslawv1.SessionAppendRecord) error {
	if rec == nil || rec.Session == nil {
		return fmt.Errorf("PUT %s: session append missing index record", BucketSessions)
	}
	if rec.Session.Id == "" {
		return fmt.Errorf("PUT %s: empty session id", BucketSessions)
	}
	for _, key := range rec.EvictKeys {
		if err := f.store.Delete(BucketSessionMessages, key); err != nil {
			return fmt.Errorf("evict %s/%s: %w", BucketSessionMessages, key, err)
		}
	}
	for _, msg := range rec.Messages {
		if msg == nil {
			continue
		}
		key := sessionMessageKey(rec.Session.Id, msg.Seq)
		bytes, err := proto.Marshal(msg)
		if err != nil {
			return fmt.Errorf("marshal session message %s: %w", key, err)
		}
		if err := f.store.Put(BucketSessionMessages, key, bytes); err != nil {
			return fmt.Errorf("put %s/%s: %w", BucketSessionMessages, key, err)
		}
	}
	bytes, err := proto.Marshal(rec.Session)
	if err != nil {
		return fmt.Errorf("marshal session %s: %w", rec.Session.Id, err)
	}
	return f.store.Put(BucketSessions, rec.Session.Id, bytes)
}

// purgeSession drops a session's index record and every message in
// its key range.
func (f *FSM) purgeSession(sessionID string) error {
	var keys []string
	prefix := sessionMessagePrefix(sessionID)
	err := f.store.ForEachPrefix(BucketSessionMessages, prefix, func(key string, _ []byte) error {
		keys = append(keys, key)
		return nil
	})
	if err != nil {
		return fmt.Errorf("scan %s/%s: %w", BucketSessionMessages, prefix, err)
	}
	for _, key := range keys {
		if err := f.store.Delete(BucketSessionMessages, key); err != nil {
			return fmt.Errorf("delete %s/%s: %w", BucketSessionMessages, key, err)
		}
	}
	return f.store.Delete(BucketSessions, sessionID)
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
	if !claimableBucket(bucket) {
		return fmt.Errorf("CLAIM %s: bucket does not support claim semantics", bucket)
	}

	// A CLAIM without an expected revision is refused rather than
	// treated as unconditional. A uint64 whose zero value means "no
	// check" would hand the unsafe behaviour to anyone who forgot the
	// field, which is the shape of bug this codebase has already paid
	// for once (scopeFilter="" meaning "everything").
	if entry.ExpectedRevision == nil {
		return fmt.Errorf("CLAIM %s/%s: expected_revision is required; "+
			"a conditional write with no condition is not a claim", bucket, entry.Id)
	}
	expectedRev := entry.GetExpectedRevision()

	raw, getErr := f.store.Get(bucket, entry.Id)
	if getErr != nil {
		if entry.ExpectedClaimer != "" {
			return fmt.Errorf("CLAIM %s/%s: record missing, expected prior claimer %q",
				bucket, entry.Id, entry.ExpectedClaimer)
		}
		if expectedRev != 0 {
			return fmt.Errorf("%w: %s/%s record missing, expected revision %d",
				ErrClaimConflict, bucket, entry.Id, expectedRev)
		}
	} else {
		current, err := decodeClaimable(bucket, raw)
		if err != nil {
			return fmt.Errorf("CLAIM %s/%s: inspect current: %w", bucket, entry.Id, err)
		}
		// Revision first: it subsumes the claimer check and gives the
		// more useful error. claimed_by alone cannot distinguish
		// "nobody holds this" from "somebody held it, finished, and
		// released it" — so a claimer working from a read taken before
		// that whole cycle would pass the claimer check, re-fire the
		// work, and write its stale copy back over the completion.
		if current.GetRevision() != expectedRev {
			return fmt.Errorf("%w: %s/%s stale read, expected revision %d current %d",
				ErrClaimConflict, bucket, entry.Id, expectedRev, current.GetRevision())
		}
		if current.GetClaimedBy() != entry.ExpectedClaimer {
			return fmt.Errorf("%w: %s/%s expected=%q current=%q",
				ErrClaimConflict, bucket, entry.Id, entry.ExpectedClaimer, current.GetClaimedBy())
		}
	}

	// The claimer's payload is written wholesale, which is only safe
	// because the revision check above proved it was built from the
	// current record.
	setRevision(newPayload, expectedRev+1)

	bytes, err := proto.Marshal(newPayload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", bucket, err)
	}
	return f.store.Put(bucket, entry.Id, bytes)
}

// claimable is the shape shared by the records that support CLAIM.
// Getters only — see setRevision for why writes use a type switch.
type claimable interface {
	GetClaimedBy() string
	GetRevision() uint64
}

// decodeClaimable unmarshals a stored record from a claim-bearing
// bucket. Refusing other buckets here is what keeps CAS semantics
// from being silently requested against a record that has no claim
// or revision to compare.
//
// The values it returns are exact: claimed_by is NOT reinterpreted
// against claim_expires_at. Expiry is wall-clock, and the FSM must
// produce the same result on every replica and on every replay, so
// "an expired claim counts as unclaimed" belongs at the scheduler
// scan layer. A node taking over a dead holder's claim passes that
// holder's id as expected_claimer.
func decodeClaimable(bucket string, raw []byte) (claimable, error) {
	switch bucket {
	case BucketScheduledTasks:
		var r lobslawv1.ScheduledTaskRecord
		if err := proto.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return &r, nil
	case BucketCommitments:
		var r lobslawv1.AgentCommitment
		if err := proto.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return &r, nil
	case BucketSessionLeases:
		var r lobslawv1.SessionLease
		if err := proto.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return &r, nil
	case BucketEnrolments:
		var r lobslawv1.EnrolmentRecord
		if err := proto.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return &r, nil
	case BucketPrompts:
		var r lobslawv1.PromptRecord
		if err := proto.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return &r, nil
	case BucketPinned:
		var r lobslawv1.PinnedMemory
		if err := proto.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return &r, nil
	case BucketSelfTaught:
		var r lobslawv1.SelfTaughtRecord
		if err := proto.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return &r, nil
	case BucketSessionGrants:
		var r lobslawv1.SessionGrant
		if err := proto.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return &r, nil
	case BucketSkills:
		var r lobslawv1.SkillRecord
		if err := proto.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return &r, nil
	case BucketSkillBlobs:
		var r lobslawv1.SkillBlob
		if err := proto.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return &r, nil
	default:
		return nil, fmt.Errorf("bucket %q not claimable", bucket)
	}
}

// claimableBucket reports whether CLAIM semantics apply. Kept beside
// decodeClaimable so the two lists cannot drift: a bucket accepted
// here but not decodable there would pass the op check and then fail
// mid-apply.
func claimableBucket(bucket string) bool {
	switch bucket {
	case BucketScheduledTasks, BucketCommitments, BucketSessionLeases, BucketPrompts, BucketPinned,
		BucketSelfTaught, BucketSessionGrants, BucketSkills, BucketSkillBlobs, BucketEnrolments:
		return true
	default:
		return false
	}
}

// BucketAndPayload is the exported form, for callers outside the FSM
// that need the same record-type-to-bucket mapping — the rebind path
// writes records it decoded from one bucket back into it, and a second
// copy of this switch would be a second authority for where each
// record type lives.
func BucketAndPayload(entry *lobslawv1.LogEntry) (string, proto.Message, error) {
	return bucketAndPayload(entry)
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
	case *lobslawv1.LogEntry_UserPrefs:
		return BucketUserPrefs, p.UserPrefs, nil
	case *lobslawv1.LogEntry_SessionAppend:
		// Payload spans two buckets; the index bucket is the one
		// callers key off (change hooks, delete routing). The actual
		// write is applySessionAppend, not the generic put path.
		return BucketSessions, p.SessionAppend, nil
	case *lobslawv1.LogEntry_Session:
		return BucketSessions, p.Session, nil
	case *lobslawv1.LogEntry_SessionLease:
		return BucketSessionLeases, p.SessionLease, nil
	case *lobslawv1.LogEntry_Prompt:
		return BucketPrompts, p.Prompt, nil
	case *lobslawv1.LogEntry_Consolidation:
		return BucketConsolidations, p.Consolidation, nil
	case *lobslawv1.LogEntry_Pinned:
		return BucketPinned, p.Pinned, nil
	case *lobslawv1.LogEntry_SelfTaught:
		// State decides the bucket, because "archived" has to be a
		// place things move TO rather than a filter over the live set
		// — that is what makes "show me what it stopped using" a scan
		// rather than a predicate, and what stops an archived artefact
		// being one bad filter away from loading again.
		if p.SelfTaught.GetState() == lobslawv1.SelfTaughtState_SELF_TAUGHT_STATE_ARCHIVED {
			return BucketSelfTaughtArchive, p.SelfTaught, nil
		}
		return BucketSelfTaught, p.SelfTaught, nil
	case *lobslawv1.LogEntry_SelfTaughtUsage:
		return BucketSelfTaughtUsage, p.SelfTaughtUsage, nil
	case *lobslawv1.LogEntry_SelfTaughtHistory:
		return BucketSelfTaughtHistory, p.SelfTaughtHistory, nil
	case *lobslawv1.LogEntry_SessionGrant:
		return BucketSessionGrants, p.SessionGrant, nil
	case *lobslawv1.LogEntry_Skill:
		return BucketSkills, p.Skill, nil
	case *lobslawv1.LogEntry_SkillBlob:
		return BucketSkillBlobs, p.SkillBlob, nil
	case *lobslawv1.LogEntry_Enrolment:
		return BucketEnrolments, p.Enrolment, nil
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
// rc. The Store rotates its internal *bolt.DB in place — outside
// references to *Store remain valid (policy engine, scheduler,
// services all hold the same *Store and continue to work without
// re-wiring).
func (f *FSM) Restore(rc io.ReadCloser) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	defer func() { _ = rc.Close() }()

	return f.store.RestoreFromSnapshot(rc)
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
