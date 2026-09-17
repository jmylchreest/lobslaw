package memory

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// ErrGroupNotFound is returned for an unknown group id.
var ErrGroupNotFound = errors.New("groups: not found")

const (
	groupApplyTimeout = 5 * time.Second
	// MaxGroupName is generous but bounded: a name is a label in a
	// switcher, and an unbounded one is a replicated record somebody
	// can paste a document into.
	MaxGroupName = 120
)

// groupIDPattern is the same shape as a bot id — lowercase, dashed —
// because both end up in principals, keys and URLs.
var groupIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// GroupService is the teams registry.
//
// A group is a coordinator plus the specialists reporting to it. It
// exists because "your company" was a name the console picked, and
// because one person runs more than one thing: a group owns its own
// coordinator, so "who answers when I message on Telegram" has an
// answer per team rather than one answer globally.
//
// Writes go through raft with revision-checked CAS, the same shape as
// BotService — deliberately, so there is one concurrency story for the
// registry and not two.
type GroupService struct {
	raft  *RaftNode
	store *Store
}

// NewGroupService constructs the service.
func NewGroupService(raft *RaftNode, store *Store) *GroupService {
	return &GroupService{raft: raft, store: store}
}

// Get reads one group.
func (s *GroupService) Get(_ context.Context, id string) (*lobslawv1.GroupRecord, error) {
	if s.store == nil {
		return nil, errors.New("groups: store not wired")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, ErrGroupNotFound
	}
	raw, err := s.store.Get(BucketGroups, id)
	if err != nil {
		// The store signals a missing key with types.ErrNotFound, not
		// a nil slice. Propagating it verbatim meant Put's "does this
		// already exist" check never saw ErrGroupNotFound and treated
		// a first write as a hard failure — the default group could
		// never be created, and the error said only "not found".
		if errors.Is(err, types.ErrNotFound) {
			return nil, fmt.Errorf("%w: %q", ErrGroupNotFound, id)
		}
		return nil, err
	}
	var rec lobslawv1.GroupRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("groups: unmarshal %q: %w", id, err)
	}
	return &rec, nil
}

// List returns every group, default first and then by name.
//
// Default first because it is the one a channel reaches and the one
// the console opens on; sorting the rest by name rather than by id
// means renaming a team moves it where you would look for it.
func (s *GroupService) List(_ context.Context) ([]*lobslawv1.GroupRecord, error) {
	if s.store == nil {
		return nil, errors.New("groups: store not wired")
	}
	var out []*lobslawv1.GroupRecord
	err := s.store.ForEach(BucketGroups, func(_ string, raw []byte) error {
		var rec lobslawv1.GroupRecord
		if err := proto.Unmarshal(raw, &rec); err != nil {
			return err
		}
		out = append(out, &rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GetIsDefault() != out[j].GetIsDefault() {
			return out[i].GetIsDefault()
		}
		return out[i].GetName() < out[j].GetName()
	})
	return out, nil
}

// Put creates or updates a group under revision-checked CAS.
func (s *GroupService) Put(ctx context.Context, rec *lobslawv1.GroupRecord, expectedRevision uint64) (*lobslawv1.GroupRecord, error) {
	if rec == nil {
		return nil, errors.New("groups: record required")
	}
	if s.raft == nil {
		return nil, errors.New("groups: raft not wired")
	}
	rec = proto.Clone(rec).(*lobslawv1.GroupRecord)
	if err := validateGroup(rec); err != nil {
		return nil, err
	}

	prev, err := s.Get(ctx, rec.GetId())
	switch {
	case err == nil:
		if prev.GetRevision() != expectedRevision {
			return nil, fmt.Errorf("%w: group %q changed; read it again and retry", ErrClaimConflict, rec.GetId())
		}
		// Properties of the record's history, not of whatever the
		// caller sent. Letting an update carry is_default would let a
		// rename quietly move which team answers Telegram.
		rec.IsDefault = prev.GetIsDefault()
		rec.CreatedAt = prev.GetCreatedAt()
		rec.CreatedBy = prev.GetCreatedBy()
		// Ownership is set once, at creation. An update that could
		// carry it would be a way to take somebody's team by editing
		// its name.
		rec.Owner = prev.GetOwner()
	case errors.Is(err, ErrGroupNotFound):
		if expectedRevision != 0 {
			return nil, fmt.Errorf("%w: group %q does not exist", ErrClaimConflict, rec.GetId())
		}
		if rec.GetCreatedAt() == nil {
			rec.CreatedAt = timestamppb.Now()
		}
	default:
		return nil, err
	}

	rec.UpdatedAt = timestamppb.Now()
	rec.Revision = expectedRevision + 1
	data, err := proto.Marshal(&lobslawv1.LogEntry{
		Op:               lobslawv1.LogOp_LOG_OP_CLAIM,
		Id:               rec.GetId(),
		ExpectedRevision: &expectedRevision,
		Payload:          &lobslawv1.LogEntry_Group{Group: rec},
	})
	if err != nil {
		return nil, err
	}
	res, err := s.raft.ApplyOrForward(ctx, data, groupApplyTimeout)
	if err != nil {
		return nil, fmt.Errorf("groups: raft apply: %w", err)
	}
	if applyErr, ok := res.(error); ok && applyErr != nil {
		return nil, applyErr
	}
	return rec, nil
}

// Delete removes a group.
//
// The default is refused for the same reason the coordinator is: it is
// what an inbound message reaches, and deleting it would leave a
// Telegram message with nobody to answer — a failure that looks like
// an outage rather than like a thing somebody did.
func (s *GroupService) Delete(ctx context.Context, id string) error {
	rec, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if rec.GetIsDefault() {
		return errors.New("groups: the default group cannot be deleted — make another group the default first")
	}
	if s.raft == nil {
		return errors.New("groups: raft not wired")
	}
	data, err := proto.Marshal(&lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_DELETE,
		Id: id,
		// The bucket has to be named on a delete: the payload that
		// would otherwise identify it is absent by definition.
		Payload: &lobslawv1.LogEntry_Group{Group: &lobslawv1.GroupRecord{Id: id}},
	})
	if err != nil {
		return err
	}
	res, err := s.raft.ApplyOrForward(ctx, data, groupApplyTimeout)
	if err != nil {
		return fmt.Errorf("groups: raft apply: %w", err)
	}
	if applyErr, ok := res.(error); ok && applyErr != nil {
		return applyErr
	}
	return nil
}

// Default returns the group a channel reaches when nothing else says
// otherwise, creating it on first call.
//
// Created here rather than in a migration because the condition that
// needs it — "there are bots but no group" — is exactly the state
// every existing deployment upgrades into, and a migration that half
// ran would leave bots orphaned from any coordinator.
func (s *GroupService) Default(ctx context.Context, name string) (*lobslawv1.GroupRecord, error) {
	groups, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if g.GetIsDefault() {
			return g, nil
		}
	}
	if strings.TrimSpace(name) == "" {
		name = "Your team"
	}
	return s.Put(ctx, &lobslawv1.GroupRecord{
		Id:               DefaultGroupID,
		Name:             name,
		IsDefault:        true,
		CoordinatorBotId: CoordinatorBotID,
		CreatedBy:        "system",
	}, 0)
}

// GroupOf reports which group a bot belongs to, treating an empty
// group_id as the default.
//
// A function rather than a field read, because "empty means default"
// has to be applied identically everywhere or bots written before
// groups existed appear in no team at all.
func GroupOf(rec *lobslawv1.BotRecord) string {
	if id := strings.TrimSpace(rec.GetGroupId()); id != "" {
		return id
	}
	return DefaultGroupID
}

func validateGroup(rec *lobslawv1.GroupRecord) error {
	id := strings.TrimSpace(rec.GetId())
	if !groupIDPattern.MatchString(id) {
		return fmt.Errorf("groups: id %q must be lowercase letters, digits and dashes", rec.GetId())
	}
	rec.Id = id
	rec.Name = strings.TrimSpace(rec.GetName())
	if rec.Name == "" {
		// A group with no name is one the console cannot label, and
		// naming teams is the entire point of the feature.
		return errors.New("groups: name is required")
	}
	if len(rec.Name) > MaxGroupName {
		return fmt.Errorf("groups: name is %d characters; the cap is %d", len(rec.Name), MaxGroupName)
	}
	rec.Description = strings.TrimSpace(rec.GetDescription())
	return nil
}

// ErrNotYours is returned when somebody tries to change a team that
// belongs to another person.
var ErrNotYours = errors.New("groups: that team belongs to somebody else")

// MayModify reports whether a principal can rename, re-point or delete
// a team.
//
// An UNOWNED team is editable by anyone who can sign in. That is
// deliberate rather than an oversight: the seeded default team has no
// owner, as does every team created before ownership existed, and
// making those un-editable would lock people out of their own default
// on upgrade. Ownership constrains teams somebody actually claimed.
func MayModify(rec *lobslawv1.GroupRecord, principal string) bool {
	owner := strings.TrimSpace(rec.GetOwner())
	if owner == "" {
		return true
	}
	return owner == strings.TrimSpace(principal)
}
