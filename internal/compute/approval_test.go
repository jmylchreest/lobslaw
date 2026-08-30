package compute

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Two properties, and the second is worthless without the first.
//
// A require_confirmation rule has to reach the user. It used to become
// a tool error the *model* read, so an operator who wrote the rule
// expecting to be asked was never asked — the effect behaved as a deny
// with a confusing message. docs/architecture/agent-loop described the
// intended behaviour all along; only the budget path implemented it.
//
// And once the user has answered for a conversation, they must not be
// asked again for the same thing. Every approval was one-shot, which
// trains an operator to approve without reading and then to switch
// confirmations off — losing the protection entirely.

func TestConfirmationReasonStripsTheSentinel(t *testing.T) {
	t.Parallel()
	// What a channel shows the user should be the rule's own reason,
	// not the plumbing. "tool invocation requires confirmation:" tells
	// the operator nothing they did not already know.
	err := wrapRequireConfirm(`rule "shell-guard" matched (tool:exec/shell_command)`)
	if got := confirmationReason(err); got != `rule "shell-guard" matched (tool:exec/shell_command)` {
		t.Errorf("reason = %q, want the rule's own text", got)
	}

	// An error that is not the sentinel is passed through rather than
	// silently emptied.
	plain := errString("something else entirely")
	if got := confirmationReason(plain); got != "something else entirely" {
		t.Errorf("reason = %q, want the original text", got)
	}
}

func TestSessionApprovalScopedToOneConversation(t *testing.T) {
	t.Parallel()
	store := NewSessionApprovals()

	alice := turn.WithIdentity(context.Background(), turn.Identity{
		Channel: "telegram", ChannelID: "-100", UserID: "alice",
	})
	other := turn.WithIdentity(context.Background(), turn.Identity{
		Channel: "telegram", ChannelID: "-200", UserID: "alice",
	})

	if store.Granted(alice, "tool:exec", "shell_command") {
		t.Fatal("a fresh store granted something")
	}
	if !store.Grant(alice, "tool:exec", "shell_command") {
		t.Fatal("grant was refused for an identified turn")
	}
	if !store.Granted(alice, "tool:exec", "shell_command") {
		t.Error("the conversation that approved is asked again")
	}

	// The grant belongs to the conversation it was given in. In a
	// group chat the person tapping Approve is approving for that
	// chat, not for every chat they are in.
	if store.Granted(other, "tool:exec", "shell_command") {
		t.Error("an approval leaked into a different conversation")
	}
	// And it is per operation, not a blanket yes.
	if store.Granted(alice, "tool:exec", "memory_forget") {
		t.Error("approving one tool approved another")
	}
}

// A turn with no identity has no conversation to scope a grant to, and
// a grant that matches everything is the opposite of what the user was
// offered.
func TestSessionApprovalRefusesAnonymousTurns(t *testing.T) {
	t.Parallel()
	store := NewSessionApprovals()

	if store.Grant(context.Background(), "tool:exec", "shell_command") {
		t.Error("an anonymous turn was allowed to record a grant")
	}
	if store.Granted(context.Background(), "tool:exec", "shell_command") {
		t.Error("an anonymous turn matched a grant")
	}
}

// The nil store must grant nothing, so a deployment that never wires
// one is not accidentally permissive.
func TestNilApprovalStoreGrantsNothing(t *testing.T) {
	t.Parallel()
	var store *SessionApprovals
	ctx := turn.WithIdentity(context.Background(), turn.Identity{Channel: "rest", ChannelID: "s"})
	if store.Granted(ctx, "tool:exec", "anything") {
		t.Error("a nil store granted an approval")
	}
}

// The property that matters most: a require_confirmation rule must
// produce something the channel can show a user, not a tool error the
// model reads. This drives the real policy engine and the real
// executor, because the bug lived in the seam between them.
func TestRequireConfirmationReachesTheCaller(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	seedRuleForTest(t, env.store, &lobslawv1.PolicyRule{
		Id: "confirm-shell", Subject: "*", Action: "tool:exec", Resource: "shell_command",
		Effect: "require_confirmation", Priority: 100,
		// Higher priority than the permissive default seeded above.
	})

	ctx := turn.WithIdentity(context.Background(), turn.Identity{
		Channel: "telegram", ChannelID: "-100", UserID: "alice",
	})
	claims := &types.Claims{UserID: "alice", Scope: "default"}

	err := env.executor.CheckPolicy(ctx, claims, "tool:exec", "shell_command")
	if err == nil {
		t.Fatal("a require_confirmation rule allowed the call outright")
	}
	if !errors.Is(err, ErrRequireConfirm) {
		t.Fatalf("got %v, want ErrRequireConfirm — the channel cannot tell this from a denial", err)
	}

	// Once approved for this conversation, the same operation is not
	// asked again.
	approvals := NewSessionApprovals()
	env.executor.SetSessionApprovals(approvals)
	if !approvals.Grant(ctx, "tool:exec", "shell_command") {
		t.Fatal("grant refused")
	}
	if err := env.executor.CheckPolicy(ctx, claims, "tool:exec", "shell_command"); err != nil {
		t.Errorf("the conversation was asked again after approving: %v", err)
	}

	// A different conversation still gets asked.
	elsewhere := turn.WithIdentity(context.Background(), turn.Identity{
		Channel: "telegram", ChannelID: "-200", UserID: "alice",
	})
	if err := env.executor.CheckPolicy(elsewhere, claims, "tool:exec", "shell_command"); !errors.Is(err, ErrRequireConfirm) {
		t.Errorf("a grant in one conversation suppressed the prompt in another: %v", err)
	}
}

func seedRuleForTest(t *testing.T, store *memory.Store, rule *lobslawv1.PolicyRule) {
	t.Helper()
	raw, err := proto.Marshal(rule)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(memory.BucketPolicyRules, rule.Id, raw); err != nil {
		t.Fatal(err)
	}
}

// wrapRequireConfirm and errString build the two error shapes
// confirmationReason has to tell apart, without standing up a policy
// engine to produce them.
func wrapRequireConfirm(reason string) error {
	return fmt.Errorf("%w: %s", ErrRequireConfirm, reason)
}

type errString string

func (e errString) Error() string { return string(e) }
