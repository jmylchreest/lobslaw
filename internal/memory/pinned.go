package memory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/promptguard"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// The archive plus per-turn recall means a vector miss on "prefers
// terse replies" makes the turn behave as though it were never said.
// Some facts cannot be subject to a retrieval hit, so they go in the
// prompt every turn instead.
//
// Which makes them a fixed tax on every request, and that is why they
// are capped. The cap is not modesty — it is what forces curation.

// PinnedKind distinguishes the two blocks. Separate records rather
// than one blob: a profile is about a person and notes are about an
// environment, they are edited by different parties for different
// reasons, and one overflowing must not squeeze the other.
type PinnedKind string

const (
	// PinnedProfile is who the user is.
	PinnedProfile PinnedKind = "profile"
	// PinnedNotes is what the agent has learned about the environment
	// — conventions, quirks, facts about the deployment.
	PinnedNotes PinnedKind = "notes"
)

// Default caps, in characters rather than tokens: a character count is
// model-independent, and a limit that moves when the tokeniser changes
// is not a limit an operator can reason about.
//
// The starting values follow hermes's, which are the only field-tested
// ones anybody has published.
const (
	DefaultProfileCap = 1375
	DefaultNotesCap   = 2200

	// ConsolidationThreshold is the fraction of a cap at which Dream
	// is asked to propose a tidy-up. Below the cap on purpose: the
	// point is to consolidate BEFORE a write fails, so the pressure
	// produces curation rather than an error the user sees.
	ConsolidationThreshold = 0.8
)

var (
	// ErrPinnedFull is returned when a write would exceed the cap.
	// Deliberately an error rather than a truncation: the cap is the
	// pressure that forces curation, and silently dropping the tail
	// would remove the pressure and lose the content.
	ErrPinnedFull = errors.New("pinned memory: at capacity")

	// ErrEntryNotFound is returned when an edit's match string finds
	// nothing.
	ErrEntryNotFound = errors.New("pinned memory: no entry matches")

	// ErrAmbiguousMatch is returned when an edit's match string finds
	// more than one entry. Refused rather than guessing: editing the
	// wrong memory is worse than being told to be more specific.
	ErrAmbiguousMatch = errors.New("pinned memory: match is ambiguous")
)

// PinnedStore reads and writes the always-on blocks.
type PinnedStore struct {
	raft  raftApplier
	store *Store
	caps  map[PinnedKind]int
}

// PinnedConfig tunes the caps. Zero on either takes the default.
type PinnedConfig struct {
	ProfileCap int
	NotesCap   int
}

func NewPinnedStore(raft raftApplier, store *Store, cfg PinnedConfig) (*PinnedStore, error) {
	if raft == nil || store == nil {
		return nil, errors.New("pinned memory: Raft and Store are both required")
	}
	profile, notes := cfg.ProfileCap, cfg.NotesCap
	if profile <= 0 {
		profile = DefaultProfileCap
	}
	if notes <= 0 {
		notes = DefaultNotesCap
	}
	return &PinnedStore{
		raft:  raft,
		store: store,
		caps:  map[PinnedKind]int{PinnedProfile: profile, PinnedNotes: notes},
	}, nil
}

// Cap returns the character limit for a kind.
func (p *PinnedStore) Cap(kind PinnedKind) int { return p.caps[kind] }

func pinnedKey(kind PinnedKind, userID string) string {
	return string(kind) + ":" + userID
}

// Get reads a block. A missing record is an empty one rather than an
// error: a user who has never had anything pinned has an empty
// profile, not a broken one.
func (p *PinnedStore) Get(kind PinnedKind, userID string) (*lobslawv1.PinnedMemory, error) {
	if userID == "" {
		return nil, errors.New("pinned memory: user id required")
	}
	raw, err := p.store.Get(BucketPinned, pinnedKey(kind, userID))
	if err != nil {
		return &lobslawv1.PinnedMemory{
			Id: pinnedKey(kind, userID), UserId: userID, Kind: string(kind),
		}, nil
	}
	var rec lobslawv1.PinnedMemory
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("pinned memory: decode: %w", err)
	}
	return &rec, nil
}

// Usage reports characters used and the cap, for a caller that wants
// to warn before it fails.
func (p *PinnedStore) Usage(kind PinnedKind, userID string) (used, capacity int, err error) {
	rec, err := p.Get(kind, userID)
	if err != nil {
		return 0, 0, err
	}
	return renderedLen(rec.Entries), p.caps[kind], nil
}

// Add appends an entry.
func (p *PinnedStore) Add(ctx context.Context, kind PinnedKind, userID, entry string) error {
	return p.mutate(ctx, kind, userID, func(entries []string) ([]string, error) {
		text := strings.TrimSpace(entry)
		if text == "" {
			return nil, errors.New("pinned memory: entry is empty")
		}
		// Silently accepting a duplicate would let a model that
		// re-remembers the same fact every turn fill the block with one
		// sentence.
		if slices.Contains(entries, text) {
			return nil, fmt.Errorf("pinned memory: that entry is already stored")
		}
		return append(append([]string(nil), entries...), text), nil
	})
}

// Replace swaps the single entry containing match.
//
// Substring match rather than an id, because an id-addressed store
// means the model has to read an index before it can change one line —
// a round trip to edit a sentence.
func (p *PinnedStore) Replace(ctx context.Context, kind PinnedKind, userID, match, replacement string) error {
	return p.mutate(ctx, kind, userID, func(entries []string) ([]string, error) {
		idx, err := uniqueMatch(entries, match)
		if err != nil {
			return nil, err
		}
		text := strings.TrimSpace(replacement)
		if text == "" {
			return nil, errors.New("pinned memory: replacement is empty; use Remove to delete an entry")
		}
		out := append([]string(nil), entries...)
		out[idx] = text
		return out, nil
	})
}

// Remove deletes the single entry containing match.
func (p *PinnedStore) Remove(ctx context.Context, kind PinnedKind, userID, match string) error {
	return p.mutate(ctx, kind, userID, func(entries []string) ([]string, error) {
		idx, err := uniqueMatch(entries, match)
		if err != nil {
			return nil, err
		}
		return append(append([]string(nil), entries[:idx]...), entries[idx+1:]...), nil
	})
}

// uniqueMatch finds exactly one entry containing match.
func uniqueMatch(entries []string, match string) (int, error) {
	needle := strings.TrimSpace(match)
	if needle == "" {
		return 0, errors.New("pinned memory: match string is required")
	}
	found := -1
	for i, e := range entries {
		if !strings.Contains(e, needle) {
			continue
		}
		if found >= 0 {
			return 0, fmt.Errorf("%w: %q appears in more than one entry; use a longer, unique fragment",
				ErrAmbiguousMatch, needle)
		}
		found = i
	}
	if found < 0 {
		return 0, fmt.Errorf("%w: nothing contains %q", ErrEntryNotFound, needle)
	}
	return found, nil
}

// mutate applies a change under the cap, the guard, and a CAS.
func (p *PinnedStore) mutate(_ context.Context, kind PinnedKind, userID string, fn func([]string) ([]string, error)) error {
	if userID == "" {
		return errors.New("pinned memory: user id required")
	}
	current, err := p.Get(kind, userID)
	if err != nil {
		return err
	}
	next, err := fn(current.Entries)
	if err != nil {
		return err
	}

	// These blocks are agent-written and land in the most privileged
	// position in the request. That the STORE is trusted says nothing
	// about the content — a fact learned from a fetched page can carry
	// an instruction, and pinning it would put that instruction in
	// system position on every future turn.
	for _, e := range next {
		if finding, bad := promptguard.Suspicious(e); bad {
			return fmt.Errorf(
				"pinned memory: refusing to store an entry that reads as an instruction (%s); "+
					"pinned entries are rendered in system position on every turn",
				finding)
		}
	}

	if used, limit := renderedLen(next), p.caps[kind]; used > limit {
		return fmt.Errorf("%w: %s would be %d characters, limit is %d — remove or consolidate an entry first",
			ErrPinnedFull, kind, used, limit)
	}

	updated := &lobslawv1.PinnedMemory{
		Id:        pinnedKey(kind, userID),
		UserId:    userID,
		Kind:      string(kind),
		Entries:   next,
		UpdatedAt: timestamppb.Now(),
		ClaimedBy: "",
	}
	entry := &lobslawv1.LogEntry{
		Op:               lobslawv1.LogOp_LOG_OP_CLAIM,
		Id:               updated.Id,
		Payload:          &lobslawv1.LogEntry_Pinned{Pinned: updated},
		ExpectedClaimer:  "",
		ExpectedRevision: &current.Revision,
	}
	data, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("pinned memory: marshal: %w", err)
	}
	res, err := p.raft.Apply(data, 5*time.Second)
	if err != nil {
		return fmt.Errorf("pinned memory: raft apply: %w", err)
	}
	if ferr, ok := res.(error); ok && ferr != nil {
		return ferr
	}
	return nil
}

// renderedLen is the character cost of a block as it will appear in
// the prompt: the entries plus the newline each one costs.
//
// Measured on the rendered form rather than the sum of entry lengths,
// because the rendered form is what is actually paid for on every
// request and a cap that undercounts is not a cap.
func renderedLen(entries []string) int {
	n := 0
	for _, e := range entries {
		n += len([]rune(e)) + 1
	}
	return n
}

// NeedsConsolidation reports whether a block is close enough to its
// cap that Dream should propose a tidy-up.
//
// Asked before the cap is reached on purpose: the point is to
// consolidate BEFORE a write fails, so the pressure produces curation
// in the background rather than an error the user sees.
func (p *PinnedStore) NeedsConsolidation(kind PinnedKind, userID string) (bool, error) {
	used, limit, err := p.Usage(kind, userID)
	if err != nil {
		return false, err
	}
	if limit <= 0 {
		return false, nil
	}
	return float64(used) >= float64(limit)*ConsolidationThreshold, nil
}

// ReplaceAll swaps a block's entries wholesale.
//
// For Dream's consolidation pass, which produces a new set rather than
// editing one entry. It goes through the same mutate path as every
// other write — so the promptguard scan and the cap check apply
// identically. A consolidation that returned an instruction-shaped
// entry would otherwise land in system position on every future turn,
// and it arrived from a model rather than from the user.
func (p *PinnedStore) ReplaceAll(ctx context.Context, kind PinnedKind, userID string, entries []string) error {
	return p.mutate(ctx, kind, userID, func([]string) ([]string, error) {
		out := make([]string, 0, len(entries))
		for _, e := range entries {
			if e = strings.TrimSpace(e); e != "" {
				out = append(out, e)
			}
		}
		return out, nil
	})
}

// pinnedOwner identifies one block.
type pinnedOwner struct {
	kind  PinnedKind
	owner string
}

// pinnedOwners enumerates the blocks that exist.
//
// Read from the store rather than from a list of known users: a user
// who has never pinned anything has no block, and one whose account is
// gone still has theirs until it is forgotten. Either way the answer
// is what is actually stored.
func pinnedOwners(store *Store) ([]pinnedOwner, error) {
	var out []pinnedOwner
	err := store.ForEach(BucketPinned, func(_ string, value []byte) error {
		var rec lobslawv1.PinnedMemory
		if err := proto.Unmarshal(value, &rec); err != nil {
			// One unreadable record must not stop the pass: the others
			// are still over their thresholds.
			return nil
		}
		if rec.GetUserId() == "" || rec.GetKind() == "" {
			return nil
		}
		out = append(out, pinnedOwner{kind: PinnedKind(rec.GetKind()), owner: rec.GetUserId()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
