package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// "Approved for the rest of this conversation", made durable and
// cluster-wide.
//
// It was an in-process map, and the argument for that was real: a
// grant that outlives what the user was looking at is one they did not
// knowingly give. But that argument only ever covered the RESTART
// axis. On the cluster axis it never held — same conversation, same
// continuity the user was reasoning about, and they were asked again
// because the next message happened to land on a different node. That
// is not the continuity ending. That is routing.
//
// So the grant replicates, and the bound the dying process used to
// provide is stated explicitly instead. A process exiting is a
// terrible TTL: it made the lifetime of a security grant a function of
// deploy cadence — weeks on a stable cluster, ninety seconds during a
// rollout — and neither of those is a decision anybody made.

// DefaultSessionGrantTTL bounds a conversation-scoped approval.
//
// A day, because the unit the user was reasoning about is a
// conversation and conversations are a day-shaped thing. Long enough
// that finishing a task tomorrow morning does not re-ask; short enough
// that a grant given last month is not still live.
const DefaultSessionGrantTTL = 24 * time.Hour

// ErrGrantNotFound is returned by Revoke for a grant that is not there.
var ErrGrantNotFound = errors.New("session grant: not found")

// grantSep separates the parts of a grant key.
//
// NUL, so a channel id containing the separator cannot forge a key
// belonging to a different conversation — the same reason the
// in-process version used it, and it matters more now that the key is
// replicated.
const grantSep = "\x00"

// SessionGrantStore holds conversation-scoped approvals.
type SessionGrantStore struct {
	raft  raftApplier
	store *Store
	ttl   time.Duration
}

// NewSessionGrantStore builds the store. A zero ttl takes the default.
func NewSessionGrantStore(raft raftApplier, store *Store, ttl time.Duration) (*SessionGrantStore, error) {
	if raft == nil || store == nil {
		return nil, errors.New("session grants: raft and store are required")
	}
	if ttl <= 0 {
		ttl = DefaultSessionGrantTTL
	}
	return &SessionGrantStore{raft: raft, store: store, ttl: ttl}, nil
}

// TTL is the configured grant lifetime.
func (s *SessionGrantStore) TTL() time.Duration { return s.ttl }

// GrantKey builds the storage key for one operation in one
// conversation.
func GrantKey(sessionID, action, resource string) string {
	return sessionID + grantSep + action + grantSep + resource
}

// GrantRequest is one approval being recorded.
type GrantRequest struct {
	SessionID string
	Action    string
	Resource  string
	GrantedBy string
	PromptID  string
}

// Grant records an approval for the rest of a conversation.
//
// Every field is required. An empty session id would produce a grant
// keyed on nothing, which matches every conversation — the opposite of
// what the button offered — and an empty action or resource is the
// same failure one level down. Refused rather than defaulted: there is
// no safe default for "what did they approve".
func (s *SessionGrantStore) Grant(ctx context.Context, req GrantRequest) (*lobslawv1.SessionGrant, error) {
	if s == nil {
		return nil, errors.New("session grants: no store configured")
	}
	sessionID := strings.TrimSpace(req.SessionID)
	action := strings.TrimSpace(req.Action)
	resource := strings.TrimSpace(req.Resource)
	switch {
	case sessionID == "":
		return nil, errors.New("session grant: session id is required; an empty one matches every conversation")
	case action == "":
		return nil, errors.New("session grant: action is required")
	case resource == "":
		return nil, errors.New("session grant: resource is required")
	}
	// A wildcard would turn "yes, this file" into "yes, every file",
	// which is more than the confirmation described. Refused for the
	// same reason an approval-minted policy rule refuses one.
	if strings.Contains(action, "*") || strings.Contains(resource, "*") {
		return nil, fmt.Errorf(
			"session grant: %q on %q contains a wildcard; a conversation grant covers "+
				"the operation that was asked about, not a class of them", action, resource)
	}

	now := time.Now()
	grant := &lobslawv1.SessionGrant{
		Id:        GrantKey(sessionID, action, resource),
		SessionId: sessionID,
		Action:    action,
		Resource:  resource,
		GrantedBy: strings.TrimSpace(req.GrantedBy),
		PromptId:  strings.TrimSpace(req.PromptID),
		GrantedAt: timestamppb.New(now),
		ExpiresAt: timestamppb.New(now.Add(s.ttl)),
	}
	if err := s.apply(ctx, lobslawv1.LogOp_LOG_OP_PUT, grant); err != nil {
		return nil, err
	}
	return grant, nil
}

// Granted reports whether this conversation has already approved this
// operation.
//
// A nil store grants nothing, so the zero value is the safe one and a
// deployment that never wires a store is not accidentally permissive.
//
// Expiry is checked on read rather than trusted to the sweeper. The
// sweeper is hygiene — it stops the bucket growing — but a grant that
// is only revoked once a background pass gets round to it is live for
// however long that pass is behind, and "how stale is the sweeper" is
// not a question a permission check should have an answer to.
func (s *SessionGrantStore) Granted(sessionID, action, resource string) bool {
	if s == nil {
		return false
	}
	if sessionID == "" || action == "" || resource == "" {
		return false
	}
	raw, err := s.store.Get(BucketSessionGrants, GrantKey(sessionID, action, resource))
	if err != nil {
		return false
	}
	var grant lobslawv1.SessionGrant
	if err := proto.Unmarshal(raw, &grant); err != nil {
		return false
	}
	return !expired(&grant, time.Now())
}

// expired reports whether a grant has passed its TTL.
//
// A grant with NO expiry is treated as expired, not as eternal. The
// field is written on every path that creates one, so a record without
// it is a record this code did not write or could not fully decode —
// and the safe reading of "I do not know when this stops" is that it
// already has.
func expired(g *lobslawv1.SessionGrant, now time.Time) bool {
	if g.GetExpiresAt() == nil {
		return true
	}
	return !now.Before(g.GetExpiresAt().AsTime())
}

// ForSession lists a conversation's live grants, newest first.
func (s *SessionGrantStore) ForSession(sessionID string) ([]*lobslawv1.SessionGrant, error) {
	return s.list(sessionID+grantSep, false)
}

// List returns every live grant across every conversation — what
// `lobslaw policy approvals` shows for the session tier.
func (s *SessionGrantStore) List() ([]*lobslawv1.SessionGrant, error) {
	return s.list("", false)
}

func (s *SessionGrantStore) list(prefix string, includeExpired bool) ([]*lobslawv1.SessionGrant, error) {
	now := time.Now()
	var out []*lobslawv1.SessionGrant
	err := s.store.ForEachPrefix(BucketSessionGrants, prefix, func(_ string, raw []byte) error {
		var g lobslawv1.SessionGrant
		if err := proto.Unmarshal(raw, &g); err != nil {
			return nil //nolint:nilerr // one unreadable grant must not hide the rest
		}
		if !includeExpired && expired(&g, now) {
			return nil
		}
		out = append(out, &g)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("session grants: list: %w", err)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].GetGrantedAt().AsTime().After(out[j].GetGrantedAt().AsTime())
	})
	return out, nil
}

// Revoke drops one grant.
func (s *SessionGrantStore) Revoke(ctx context.Context, id string) error {
	if _, err := s.store.Get(BucketSessionGrants, id); err != nil {
		return fmt.Errorf("%w: %s", ErrGrantNotFound, id)
	}
	return s.apply(ctx, lobslawv1.LogOp_LOG_OP_DELETE, &lobslawv1.SessionGrant{Id: id})
}

// RevokeSession drops every grant belonging to one conversation and
// returns how many.
//
// This is the hook the in-process version had a NOTE asking for and no
// way to provide: a "forget this conversation" command must drop that
// conversation's grants too, or a cleared conversation keeps privileges
// the user believes they revoked. Now that grants are keyed by session
// id, it is a prefix scan.
func (s *SessionGrantStore) RevokeSession(ctx context.Context, sessionID string) (int, error) {
	if strings.TrimSpace(sessionID) == "" {
		return 0, errors.New("session grant: session id is required")
	}
	// Expired ones included: they are still bytes in the bucket, and
	// leaving them behind would make "forget this conversation" a
	// statement about what is enforceable rather than about what is
	// stored.
	grants, err := s.list(sessionID+grantSep, true)
	if err != nil {
		return 0, err
	}
	var n int
	for _, g := range grants {
		if err := s.apply(ctx, lobslawv1.LogOp_LOG_OP_DELETE,
			&lobslawv1.SessionGrant{Id: g.Id}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Sweep removes expired grants and returns how many.
//
// Hygiene, not enforcement — Granted checks expiry on every read, so
// nothing here is load-bearing for correctness. What it buys is a
// bucket that does not accumulate one dead record per confirmation
// ever answered, which over a year is the difference between a
// snapshot and a problem.
func (s *SessionGrantStore) Sweep(ctx context.Context) (int, error) {
	now := time.Now()
	var dead []string
	err := s.store.ForEach(BucketSessionGrants, func(key string, raw []byte) error {
		var g lobslawv1.SessionGrant
		if err := proto.Unmarshal(raw, &g); err != nil {
			// Unreadable, so nothing can ever honour it. Removed for
			// the same reason an expired one is.
			dead = append(dead, key)
			return nil
		}
		if expired(&g, now) {
			dead = append(dead, key)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("session grants: sweep: %w", err)
	}
	var n int
	for _, key := range dead {
		if err := s.apply(ctx, lobslawv1.LogOp_LOG_OP_DELETE,
			&lobslawv1.SessionGrant{Id: key}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// SweepLoop runs Sweep on a ticker. Leader-gated by the caller.
func (s *SessionGrantStore) SweepLoop(ctx context.Context, every time.Duration, log logger) error {
	if every <= 0 {
		every = time.Hour
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			n, err := s.Sweep(ctx)
			if err != nil {
				log.Warn("session grants: sweep failed", "err", err)
				continue
			}
			if n > 0 {
				log.Info("session grants: swept expired", "count", n)
			}
		}
	}
}

func (s *SessionGrantStore) apply(_ context.Context, op lobslawv1.LogOp, grant *lobslawv1.SessionGrant) error {
	data, err := proto.Marshal(&lobslawv1.LogEntry{
		Op:      op,
		Id:      grant.Id,
		Payload: &lobslawv1.LogEntry_SessionGrant{SessionGrant: grant},
	})
	if err != nil {
		return fmt.Errorf("session grants: marshal: %w", err)
	}
	res, err := s.raft.Apply(data, sessionGrantApplyTimeout)
	if err != nil {
		return fmt.Errorf("session grants: apply: %w", err)
	}
	if applyErr, ok := res.(error); ok && applyErr != nil {
		return fmt.Errorf("session grants: apply: %w", applyErr)
	}
	return nil
}
