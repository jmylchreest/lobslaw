package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// The tools for the always-on blocks.
//
// The cap is deliberately a hard error rather than a truncation — the
// pressure is what forces curation. Which makes this a livelock
// waiting to happen: a model told "you are full, consolidate" can
// spend a whole turn failing to, and hermes shipped
// _MAX_CONSOLIDATION_FAILURES_PER_TURN = 3 after exactly that,
// because a fragile replace/add loop
//
//	can't loop the turn to budget exhaustion and suppress the user's
//	reply
//
// A memory side effect must never cost somebody their answer. After N
// failures in one turn these tools stop trying and say so in terms
// that tell the model to move on.

// maxPinnedFailuresPerTurn is that cap. Three attempts is enough for a
// typo and a rethink; a fourth is a loop.
const maxPinnedFailuresPerTurn = 3

// PinnedMemoryStore is the subset of memory.PinnedStore this needs,
// as an interface so compute does not depend on the memory package.
type PinnedMemoryStore interface {
	Entries(kind, userID string) ([]string, error)
	Add(ctx context.Context, kind, userID, entry string) error
	Replace(ctx context.Context, kind, userID, match, replacement string) error
	Remove(ctx context.Context, kind, userID, match string) error
	Usage(kind, userID string) (used, capacity int, err error)
}

// pinnedFailures counts consecutive failures per turn.
//
// Keyed by turn id and cleared when a turn's first call succeeds. A
// process-lifetime map would leak, so it is bounded the same way the
// snapshot cache is: entries are small and turns are short.
type pinnedFailures struct {
	mu sync.Mutex
	n  map[string]int
}

func (f *pinnedFailures) record(turnID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n == nil {
		f.n = map[string]int{}
	}
	f.n[turnID]++
	if len(f.n) > 512 {
		// A turn id never repeats, so anything still here is from a
		// turn that ended. Cheapest correct eviction is to drop the
		// lot: the only thing lost is a count for turns in flight,
		// which restarts them at zero rather than wedging them.
		clear(f.n)
		f.n[turnID] = maxPinnedFailuresPerTurn
	}
	return f.n[turnID]
}

func (f *pinnedFailures) clear(turnID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.n, turnID)
}

func (f *pinnedFailures) exhausted(turnID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n[turnID] >= maxPinnedFailuresPerTurn
}

// RegisterPinnedBuiltins installs remember / forget over the always-on
// blocks.
func RegisterPinnedBuiltins(b *Builtins, store PinnedMemoryStore) error {
	if store == nil {
		return errors.New("pinned builtins: store required")
	}
	failures := &pinnedFailures{}

	if err := b.Register("pinned_remember", pinnedWriteHandler(store, failures)); err != nil {
		return err
	}
	return b.Register("pinned_forget", pinnedForgetHandler(store, failures))
}

func pinnedWriteHandler(store PinnedMemoryStore, failures *pinnedFailures) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		turnID, userID, kind, err := pinnedContext(ctx, args)
		if err != nil {
			return nil, 2, err
		}
		if failures.exhausted(turnID) {
			return pinnedGiveUp(kind)
		}

		entry := strings.TrimSpace(args["entry"])
		replaces := strings.TrimSpace(args["replaces"])

		if replaces != "" {
			err = store.Replace(ctx, kind, userID, replaces, entry)
		} else {
			err = store.Add(ctx, kind, userID, entry)
		}
		if err != nil {
			return pinnedFailure(store, failures, turnID, kind, userID, err)
		}
		failures.clear(turnID)
		return pinnedOK(store, kind, userID, "stored")
	}
}

func pinnedForgetHandler(store PinnedMemoryStore, failures *pinnedFailures) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		turnID, userID, kind, err := pinnedContext(ctx, args)
		if err != nil {
			return nil, 2, err
		}
		if failures.exhausted(turnID) {
			return pinnedGiveUp(kind)
		}
		if err := store.Remove(ctx, kind, userID, strings.TrimSpace(args["match"])); err != nil {
			return pinnedFailure(store, failures, turnID, kind, userID, err)
		}
		failures.clear(turnID)
		return pinnedOK(store, kind, userID, "removed")
	}
}

// pinnedContext resolves who and which block, from the turn rather
// than from the arguments.
//
// The user comes from the turn identity, never from a parameter: a
// user id the model can supply is a user id a prompt injection can
// supply, and writing into somebody else's always-on block would put
// attacker text in their system prompt on every future turn.
func pinnedContext(ctx context.Context, args map[string]string) (turnID, userID, kind string, err error) {
	id, ok := turn.IdentityFrom(ctx)
	if !ok || id.UserID == "" {
		return "", "", "", errors.New(
			"pinned memory: this turn has no identified user, so there is nobody to remember for")
	}
	kind = strings.TrimSpace(args["kind"])
	switch kind {
	case "profile", "notes":
	case "":
		kind = "notes"
	default:
		return "", "", "", fmt.Errorf("pinned memory: unknown kind %q (want profile or notes)", kind)
	}
	return id.TurnID, id.UserID, kind, nil
}

func pinnedOK(store PinnedMemoryStore, kind, userID, verb string) ([]byte, int, error) {
	used, capacity, _ := store.Usage(kind, userID)
	out, _ := json.Marshal(map[string]any{
		"status":     verb,
		"kind":       kind,
		"used_chars": used,
		"cap_chars":  capacity,
	})
	return out, 0, nil
}

// pinnedFailure counts the failure and returns it, or gives up if this
// turn has now spent its allowance.
func pinnedFailure(store PinnedMemoryStore, failures *pinnedFailures, turnID, kind, userID string, cause error) ([]byte, int, error) {
	if n := failures.record(turnID); n >= maxPinnedFailuresPerTurn {
		return pinnedGiveUp(kind)
	}
	used, capacity, _ := store.Usage(kind, userID)
	return nil, 1, fmt.Errorf("%w (%s is %d/%d characters)", cause, kind, used, capacity)
}

// pinnedGiveUp is a terminal, non-error result.
//
// Non-error on purpose: an error invites another attempt, and the
// whole point is that the model stops attempting and answers the
// person who is waiting.
func pinnedGiveUp(kind string) ([]byte, int, error) {
	out, _ := json.Marshal(map[string]any{
		"status": "gave_up",
		"kind":   kind,
		"detail": "Too many failed attempts to update pinned memory this turn. " +
			"Stop trying and answer the user — the memory can be fixed next turn.",
	})
	return out, 0, nil
}

// PinnedToolDefs are the tool definitions for the two builtins.
func PinnedToolDefs() []*types.ToolDef {
	return []*types.ToolDef{
		{
			Name:        "pinned_remember",
			Path:        compute.BuiltinScheme + "pinned_remember",
			Description: "Record a durable fact that should be in your context on EVERY future turn, not retrieved. Use sparingly: this is a small, capped block and it costs tokens on every request. kind is \"profile\" (about the user) or \"notes\" (about this environment; the default). Pass replaces with a unique fragment of an existing entry to rewrite it instead of adding. Returns current usage against the cap. If the block is full, consolidate an entry rather than retrying.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"entry": {"type": "string", "description": "The fact to store, one line where possible."},
					"kind": {"type": "string", "enum": ["profile", "notes"], "description": "profile = about the user; notes = about this environment (default)."},
					"replaces": {"type": "string", "description": "Unique fragment of an existing entry to rewrite. Omit to add a new one."}
				},
				"required": ["entry"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name:        "pinned_forget",
			Path:        compute.BuiltinScheme + "pinned_forget",
			Description: "Remove a durable fact from your always-on context. match is a unique fragment of the entry; an ambiguous fragment is refused rather than guessed at.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"match": {"type": "string", "description": "Unique fragment of the entry to remove."},
					"kind": {"type": "string", "enum": ["profile", "notes"]}
				},
				"required": ["match"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
	}
}
