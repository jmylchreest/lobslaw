package memory

import (
	"context"
	"fmt"
	"sort"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/identity"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Repointing a principal after a channel rebinding.
//
// This lived in cmd/lobslaw and wrote straight to bbolt, which means
// it required a stopped node — and on a follower it would have written
// state no other replica has, which is worse than requiring the
// outage. It is here now so the same plan can be applied either way:
// directly when the node is stopped, or through raft when it is not.
//
// The rewriters produce LOG ENTRIES rather than raw bytes. That is
// what makes the replicated path possible at all — the FSM dispatches
// on the payload type — and it keeps one mapping from record type to
// bucket, the one bucketAndPayload already owns.

// RebindPlan is what a rebind would change, and what it will not
// touch.
type RebindPlan struct {
	// Changes is bucket -> record ids, sorted, so a dry run prints the
	// same thing twice.
	Changes map[string][]string
	// Conflicts are records this cannot rebind, each with the reason.
	// Reported rather than skipped silently: a half-moved identity is
	// worse than one that did not move, because nothing says which
	// half.
	Conflicts []string
}

// Total is how many records the plan would rewrite.
func (p *RebindPlan) Total() int {
	n := 0
	for _, ids := range p.Changes {
		n += len(ids)
	}
	return n
}

// Buckets returns the touched buckets in a stable order.
func (p *RebindPlan) Buckets() []string {
	out := make([]string, 0, len(p.Changes))
	for k := range p.Changes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (p *RebindPlan) note(bucket, id string) {
	if p.Changes == nil {
		p.Changes = map[string][]string{}
	}
	p.Changes[bucket] = append(p.Changes[bucket], id)
}

// rebindRewriter walks one bucket, decoding each record and reporting
// whether it belongs to `from`.
//
// Spelled out one per record type rather than shared behind an
// interface. Protobuf messages carry state that must not be copied by
// value, and the generated types have no common owner accessor to
// write against — the clever version of this was five lines shorter
// and a copylocks violation waiting to happen.
//
// Each matches the principal form ("user:alice") AND the bare id,
// because both shapes have been written into owner fields over the
// project's life and a migration understanding only one would leave
// the other behind.
type rebindRewriter struct {
	bucket string
	// rewrite returns a PUT entry for the rebound record, or
	// (nil, false) when the record belongs to somebody else.
	rewrite func(id string, raw []byte, r rebindNames) (*lobslawv1.LogEntry, bool, error)
}

// rebindNames carries both spellings of both ids.
type rebindNames struct {
	fromPrincipal string
	toPrincipal   string
	fromID        string
	toID          string
}

func namesFor(from, to string) rebindNames {
	return rebindNames{
		fromPrincipal: identity.User(from).String(),
		toPrincipal:   identity.User(to).String(),
		fromID:        from,
		toID:          to,
	}
}

func rebindRewriters() []rebindRewriter {
	return []rebindRewriter{
		{BucketVectorRecords, rewriteVectorOwner},
		{BucketEpisodicRecords, rewriteEpisodicOwner},
		{BucketCommitments, rewriteCommitmentOwner},
		{BucketScheduledTasks, rewriteTaskOwner},
		{BucketPrompts, rewritePromptOwner},
		{BucketSessions, rewriteSessionUser},
		{BucketPolicyRules, rewriteRuleSubject},
	}
}

// PlanRebind resolves what moving `from` to `to` would rewrite,
// without writing anything.
func PlanRebind(store *Store, from, to string) (*RebindPlan, error) {
	plan := &RebindPlan{}
	names := namesFor(from, to)

	for _, rw := range rebindRewriters() {
		err := store.ForEach(rw.bucket, func(key string, raw []byte) error {
			_, changed, rerr := rw.rewrite(key, raw, names)
			if rerr != nil {
				plan.Conflicts = append(plan.Conflicts,
					fmt.Sprintf("%s/%s: %v", rw.bucket, key, rerr))
				return nil
			}
			if changed {
				plan.note(rw.bucket, key)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", rw.bucket, err)
		}
	}
	for _, ids := range plan.Changes {
		sort.Strings(ids)
	}

	// User prefs are keyed BY the canonical id, so a rebind would have
	// to merge two records rather than rewrite a field. Reported, not
	// attempted: preferences are small and hand-written, and silently
	// picking a winner between two timezones is worse than saying so.
	if _, err := store.Get(BucketUserPrefs, from); err == nil {
		plan.Conflicts = append(plan.Conflicts, fmt.Sprintf(
			"user_prefs/%s exists and is keyed by the id itself — copy any settings onto %q by hand, "+
				"then delete it; this command will not merge two preference records", from, to))
	}
	return plan, nil
}

// rebindEntries collects the PUT entries a rebind would apply.
//
// Collected before writing: mutating a bucket while iterating it is
// not something bbolt promises anything about.
func rebindEntries(store *Store, from, to string) ([]*lobslawv1.LogEntry, error) {
	names := namesFor(from, to)
	var out []*lobslawv1.LogEntry

	for _, rw := range rebindRewriters() {
		err := store.ForEach(rw.bucket, func(key string, raw []byte) error {
			entry, changed, rerr := rw.rewrite(key, raw, names)
			if rerr != nil || !changed {
				return nil //nolint:nilerr // conflicts were reported by PlanRebind
			}
			out = append(out, entry)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", rw.bucket, err)
		}
	}
	return out, nil
}

// ApplyRebindOffline rewrites straight against the store.
//
// The caller must hold the bbolt file lock, which means the node is
// stopped and there are no followers to diverge from. A rebind written
// this way on a RUNNING node's store would be state no other replica
// has, which is why the replicated path below exists.
func ApplyRebindOffline(store *Store, from, to string) error {
	entries, err := rebindEntries(store, from, to)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		bucket, payload, berr := BucketAndPayload(entry)
		if berr != nil {
			return berr
		}
		raw, merr := proto.Marshal(payload)
		if merr != nil {
			return fmt.Errorf("encode %s/%s: %w", bucket, entry.GetId(), merr)
		}
		if perr := store.Put(bucket, entry.GetId(), raw); perr != nil {
			return fmt.Errorf("write %s/%s: %w", bucket, entry.GetId(), perr)
		}
	}
	return nil
}

// RebindApplier is the slice of raft this needs — narrowed to an
// interface so the failure paths are reachable from a test. A rebind
// that reports success after a failed apply is the worst outcome
// available here, and it is not a path a real raft node offers on
// demand.
type RebindApplier interface {
	Apply(data []byte, timeout time.Duration) (any, error)
}

// ApplyRebindReplicated applies the same rewrites through raft, so
// every replica sees them.
//
// Not atomic across records — raft has no multi-entry transaction
// here, so a rebind interrupted midway leaves some records moved and
// some not. That is recoverable by re-running it (each rewrite is
// idempotent: a record already owned by `to` no longer matches `from`)
// and it is strictly better than the alternative, which was writing to
// one replica's file and requiring an outage to do it.
func ApplyRebindReplicated(_ context.Context, raft RebindApplier, store *Store, from, to string) (int, error) {
	if raft == nil {
		return 0, fmt.Errorf("rebind: this node does not host raft")
	}
	entries, err := rebindEntries(store, from, to)
	if err != nil {
		return 0, err
	}
	for i, entry := range entries {
		data, merr := proto.Marshal(entry)
		if merr != nil {
			return i, fmt.Errorf("encode entry %s: %w", entry.GetId(), merr)
		}
		res, aerr := raft.Apply(data, 5*time.Second)
		if aerr != nil {
			// The count is returned alongside the error so the caller can
			// say how far it got. "Rebind failed" without a number leaves
			// somebody unable to tell a no-op from a half-done move.
			return i, fmt.Errorf("replicate %s: %w", entry.GetId(), aerr)
		}
		if ferr, ok := res.(error); ok && ferr != nil {
			return i, fmt.Errorf("apply %s: %w", entry.GetId(), ferr)
		}
	}
	return len(entries), nil
}

// --- the rewriters ------------------------------------------------------

// putEntry wraps a rewritten record in a PUT.
//
// The payload type is what the FSM dispatches on, so each rewriter
// supplies its own concrete wrapper and the bucket is derived from it
// — one mapping, the one BucketAndPayload already owns.
func putEntry(id string, entry *lobslawv1.LogEntry) *lobslawv1.LogEntry {
	entry.Op = lobslawv1.LogOp_LOG_OP_PUT
	entry.Id = id
	return entry
}

func rewriteVectorOwner(id string, raw []byte, r rebindNames) (*lobslawv1.LogEntry, bool, error) {
	var rec lobslawv1.VectorRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, false, fmt.Errorf("decode: %w", err)
	}
	next, ok := rebindOwner(rec.Owner, r)
	if !ok {
		return nil, false, nil
	}
	rec.Owner = next
	return putEntry(id, &lobslawv1.LogEntry{Payload: &lobslawv1.LogEntry_VectorRecord{VectorRecord: &rec}}), true, nil
}

func rewriteEpisodicOwner(id string, raw []byte, r rebindNames) (*lobslawv1.LogEntry, bool, error) {
	var rec lobslawv1.EpisodicRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, false, fmt.Errorf("decode: %w", err)
	}
	next, ok := rebindOwner(rec.Owner, r)
	if !ok {
		return nil, false, nil
	}
	rec.Owner = next
	return putEntry(id, &lobslawv1.LogEntry{Payload: &lobslawv1.LogEntry_EpisodicRecord{EpisodicRecord: &rec}}), true, nil
}

func rewriteCommitmentOwner(id string, raw []byte, r rebindNames) (*lobslawv1.LogEntry, bool, error) {
	var rec lobslawv1.AgentCommitment
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, false, fmt.Errorf("decode: %w", err)
	}
	next, ok := rebindOwner(rec.Owner, r)
	if !ok {
		return nil, false, nil
	}
	rec.Owner = next
	return putEntry(id, &lobslawv1.LogEntry{Payload: &lobslawv1.LogEntry_Commitment{Commitment: &rec}}), true, nil
}

func rewriteTaskOwner(id string, raw []byte, r rebindNames) (*lobslawv1.LogEntry, bool, error) {
	var rec lobslawv1.ScheduledTaskRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, false, fmt.Errorf("decode: %w", err)
	}
	next, ok := rebindOwner(rec.Owner, r)
	if !ok {
		return nil, false, nil
	}
	rec.Owner = next
	return putEntry(id, &lobslawv1.LogEntry{Payload: &lobslawv1.LogEntry_ScheduledTask{ScheduledTask: &rec}}), true, nil
}

func rewritePromptOwner(id string, raw []byte, r rebindNames) (*lobslawv1.LogEntry, bool, error) {
	var rec lobslawv1.PromptRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, false, fmt.Errorf("decode: %w", err)
	}
	next, ok := rebindOwner(rec.Owner, r)
	if !ok {
		return nil, false, nil
	}
	rec.Owner = next
	return putEntry(id, &lobslawv1.LogEntry{Payload: &lobslawv1.LogEntry_Prompt{Prompt: &rec}}), true, nil
}

func rewriteSessionUser(id string, raw []byte, r rebindNames) (*lobslawv1.LogEntry, bool, error) {
	var rec lobslawv1.SessionRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, false, fmt.Errorf("decode: %w", err)
	}
	if rec.UserId != r.fromID {
		return nil, false, nil
	}
	rec.UserId = r.toID
	return putEntry(id, &lobslawv1.LogEntry{Payload: &lobslawv1.LogEntry_Session{Session: &rec}}), true, nil
}

// rewriteRuleSubject repoints rules written against this principal,
// including the ones an "always" approval minted.
//
// Only `user:` subjects. A role or scope subject names a group the
// person is in, not the person — rebinding those would move everybody
// who holds the role.
func rewriteRuleSubject(id string, raw []byte, r rebindNames) (*lobslawv1.LogEntry, bool, error) {
	var rec lobslawv1.PolicyRule
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, false, fmt.Errorf("decode: %w", err)
	}
	if rec.Subject != r.fromPrincipal {
		return nil, false, nil
	}
	rec.Subject = r.toPrincipal
	return putEntry(id, &lobslawv1.LogEntry{Payload: &lobslawv1.LogEntry_PolicyRule{PolicyRule: &rec}}), true, nil
}

// rebindOwner reports the replacement for an owner value, or ok=false
// when the record belongs to somebody else.
func rebindOwner(current string, r rebindNames) (string, bool) {
	switch current {
	case r.fromPrincipal:
		return r.toPrincipal, true
	case r.fromID:
		return r.toID, true
	default:
		return "", false
	}
}
