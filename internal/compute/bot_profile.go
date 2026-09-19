package compute

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/identity"
)

// BotProfile is everything a turn needs to know about which bot it is.
//
// Resolved once at the start of a turn and never re-read, so a bot
// re-instructed mid-turn finishes under the brief it started with.
type BotProfile struct {
	ID           string
	DisplayName  string
	Instructions string
	// Owner is the human principal this bot serves — "user:alice".
	Owner         string
	IsCoordinator bool
	// Tools is the registry filter. Empty means the node's full set.
	Tools []string
	// Denied is subtracted after Tools, whatever Tools says.
	// Empty allowlist means everything; Denied still removes ask_bot
	// from a child so stripping one name cannot re-grant it.
	Denied     []string
	MayMessage []string
	ModelRole  string
	Caps       BudgetCaps
}

// Principal is the identity this bot's turns run as.
func (p *BotProfile) Principal() identity.Principal {
	if p == nil {
		return ""
	}
	return identity.Bot(p.ID)
}

// MayMessageBot reports whether this bot has a declared edge to another.
func (p *BotProfile) MayMessageBot(target string) bool {
	if p == nil {
		return false
	}
	return slices.Contains(p.MayMessage, strings.TrimSpace(target))
}

// FilterTools narrows a tool list to what this bot may be shown.
//
// An empty allowlist means the node's full set, NOT the empty set.
// Denied is subtracted last and unconditionally.
func (p *BotProfile) FilterTools(all []Tool) []Tool {
	if p == nil {
		return all
	}
	out := all
	if len(p.Tools) > 0 {
		allowed := make(map[string]struct{}, len(p.Tools))
		for _, name := range p.Tools {
			allowed[strings.TrimSpace(name)] = struct{}{}
		}
		kept := make([]Tool, 0, len(all))
		for _, t := range all {
			if _, ok := allowed[t.Name]; ok {
				kept = append(kept, t)
			}
		}
		out = kept
	}
	if len(p.Denied) == 0 {
		return out
	}
	kept := make([]Tool, 0, len(out))
	for _, t := range out {
		if !slices.Contains(p.Denied, t.Name) {
			kept = append(kept, t)
		}
	}
	return kept
}

// Without returns a copy that will never be shown the named tools.
func (p *BotProfile) Without(names ...string) *BotProfile {
	if p == nil {
		return nil
	}
	clone := *p
	clone.Denied = append(append([]string(nil), p.Denied...), names...)
	return &clone
}

// BotResolver answers "which bot is this".
type BotResolver interface {
	ResolveBot(ctx context.Context, botID string) (*BotProfile, error)
}

// ErrBotDisabled is returned for a bot that exists but is switched off.
var ErrBotDisabled = errors.New("bot is disabled")
