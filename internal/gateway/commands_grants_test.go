package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

type fakeGrants struct {
	held    []GrantView
	revoked string
	n       int
	err     error
}

func (f *fakeGrants) ForSession(string) ([]GrantView, error) { return f.held, f.err }
func (f *fakeGrants) RevokeSession(_ context.Context, id string) (int, error) {
	f.revoked = id
	return f.n, f.err
}

func grantsSet(t *testing.T, g SessionGrants) *CommandSet {
	t.Helper()
	cs := NewCommandSet(allowAllCommands{}, discardLogger())
	RegisterGrantCommands(cs, g)
	return cs
}

type allowAllCommands struct{}

func (allowAllCommands) AllowsCommand(context.Context, *types.Claims, string) bool { return true }

// The command exists to answer "what has this conversation already
// agreed to", so the answer has to name the operations rather than the
// storage keys — an id carries an unprintable separator and names
// nothing a person recognises.
func TestGrantsListsOperationsNotIDs(t *testing.T) {
	t.Parallel()
	g := &fakeGrants{held: []GrantView{
		{ID: "slack:C1\x00shell:run\x00git status", Action: "shell:run",
			Resource: "git status", GrantedBy: "user:alice", ExpiresIn: 3 * time.Hour},
	}}
	out := grantsSet(t, g).Dispatch(context.Background(), CommandRequest{
		Name:    "grants",
		Session: SessionRef{Channel: "slack", ChannelID: "C1"},
	})
	if !strings.Contains(out, "shell:run — git status") {
		t.Errorf("the operation should be named: %q", out)
	}
	if strings.Contains(out, "\x00") {
		t.Error("a raw storage key reached the reply")
	}
	if !strings.Contains(out, "by user:alice") {
		t.Error("an unattributable standing grant is one nobody can audit")
	}
}

// Nothing standing is a different message from a failure, and from a
// revoke that found nothing.
func TestGrantsSaysNothingStandingPlainly(t *testing.T) {
	t.Parallel()
	out := grantsSet(t, &fakeGrants{}).Dispatch(context.Background(), CommandRequest{
		Name:    "grants",
		Session: SessionRef{Channel: "slack", ChannelID: "C1"},
	})
	if !strings.Contains(out, "Nothing standing") {
		t.Errorf("unexpected reply: %q", out)
	}
}

// The whole reason this command exists: revoking approvals must not
// take the transcript with it, which is what /new does.
func TestGrantsRevokeTargetsThisConversationOnly(t *testing.T) {
	t.Parallel()
	g := &fakeGrants{n: 2}
	out := grantsSet(t, g).Dispatch(context.Background(), CommandRequest{
		Name:    "grants",
		Args:    "revoke",
		Session: SessionRef{Channel: "telegram", ChannelID: "-100"},
	})
	if g.revoked != "telegram:-100" {
		t.Errorf("revoked %q, want telegram:-100", g.revoked)
	}
	if !strings.Contains(out, "Revoked 2") {
		t.Errorf("unexpected reply: %q", out)
	}
}

// A revoke that matched nothing is not a success message. "Revoked 0"
// reads as a no-op somebody has to interpret.
func TestGrantsRevokeDistinguishesNothingToDo(t *testing.T) {
	t.Parallel()
	out := grantsSet(t, &fakeGrants{n: 0}).Dispatch(context.Background(), CommandRequest{
		Name: "grants", Args: "revoke",
		Session: SessionRef{Channel: "slack", ChannelID: "C1"},
	})
	if !strings.Contains(out, "nothing to revoke") {
		t.Errorf("unexpected reply: %q", out)
	}
}

// It reads and writes ONE conversation, so a channel that cannot say
// which conversation must refuse rather than act on the wrong one —
// the same rule /new and /status follow.
func TestGrantsIsSessionScoped(t *testing.T) {
	t.Parallel()
	if !grantsSet(t, &fakeGrants{}).IsSessionScoped("grants") {
		t.Error("grants acts on one conversation and must be marked session-scoped")
	}
}

// A node with no replicated store registers nothing, rather than a
// command that always answers "none" — which reads as "you have
// approved nothing" instead of "this cannot be answered here".
func TestGrantsUnregisteredWithoutAStore(t *testing.T) {
	t.Parallel()
	cs := NewCommandSet(allowAllCommands{}, discardLogger())
	RegisterGrantCommands(cs, nil)
	for _, n := range cs.Names() {
		if n == "grants" {
			t.Fatal("grants registered with no backing store")
		}
	}
}

func TestGrantsReportsAStoreFailure(t *testing.T) {
	t.Parallel()
	out := grantsSet(t, &fakeGrants{err: errors.New("raft unavailable")}).Dispatch(
		context.Background(), CommandRequest{
			Name:    "grants",
			Session: SessionRef{Channel: "slack", ChannelID: "C1"},
		})
	if !strings.Contains(out, "raft unavailable") {
		t.Errorf("a failure must reach the caller: %q", out)
	}
}
