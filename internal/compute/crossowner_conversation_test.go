package compute

import (
	"context"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// otherRecord is a private record belonging to somebody who is not the
// speaker, originating in a conversation that is not this one.
func otherRecord(owner, sessionRef string) *lobslawv1.EpisodicRecord {
	return &lobslawv1.EpisodicRecord{
		Owner:      owner,
		Visibility: lobslawv1.Visibility_VISIBILITY_PRIVATE,
		SessionRef: sessionRef,
	}
}

func TestReadAudienceDMIsOwnershipOnly(t *testing.T) {
	t.Parallel()

	turn := turn.Identity{
		Principal: identity.User("bob"),
		Channel:   "slack",
		ChannelID: "D0BOB",
		Shared:    false,
	}
	aud := readAudience(context.Background(), turn, nil)

	// Even a record from this very conversation is only readable
	// because bob owns it — a DM never widens by conversation.
	if aud.AllowsEpisodic(otherRecord("user:alice", "slack:D0BOB")) {
		t.Fatal("a DM audience read another principal's record from the same thread")
	}
	if !aud.AllowsEpisodic(otherRecord(identity.User("bob").String(), "slack:D0BOB")) {
		t.Fatal("a DM audience lost the speaker's own record")
	}
}

func TestReadAudienceSharedChannelScopesToConversation(t *testing.T) {
	t.Parallel()

	turn := turn.Identity{
		Principal: identity.User("bob"),
		Channel:   "slack",
		ChannelID: "C0GENERAL",
		Shared:    true,
	}
	aud := readAudience(context.Background(), turn, nil)

	if !aud.AllowsEpisodic(otherRecord("user:alice", "slack:C0GENERAL")) {
		t.Error("a record from this shared channel was not readable in it")
	}
	if aud.AllowsEpisodic(otherRecord("user:alice", "slack:D0ALICE")) {
		t.Error("alice's DM record was readable from a shared channel")
	}
	if aud.AllowsEpisodic(otherRecord("user:alice", "")) {
		t.Error("an originless record was swept into a shared channel")
	}
}

// A turn flagged shared but carrying no conversation address must not
// widen to everything. The guard exists because Shared and the address
// come from different places and can disagree.
func TestReadAudienceSharedWithoutAddressDoesNotWiden(t *testing.T) {
	t.Parallel()

	turn := turn.Identity{
		Principal: identity.User("bob"),
		Shared:    true,
	}
	aud := readAudience(context.Background(), turn, nil)

	if aud.AllowsEpisodic(otherRecord("user:alice", "")) {
		t.Fatal("a shared turn with no conversation address read another principal's record")
	}
	if aud.AllowsEpisodic(otherRecord("user:alice", "slack:C0GENERAL")) {
		t.Fatal("a shared turn with no conversation address reached an arbitrary conversation")
	}
}

// The cross-owner authorizer still wins: an operator granted the
// widening reads everything regardless of where they are speaking.
func TestReadAudienceCrossOwnerStillWidensInSharedChannel(t *testing.T) {
	t.Parallel()

	turn := turn.Identity{
		Principal: identity.User("bob"),
		Channel:   "slack",
		ChannelID: "C0GENERAL",
		Shared:    true,
	}
	aud := readAudience(context.Background(), turn, allowAllCrossOwner{})

	if !aud.AllowsEpisodic(otherRecord("user:alice", "slack:D0ALICE")) {
		t.Fatal("an authorised cross-owner read was narrowed by the conversation scope")
	}
	if aud.IsZero() {
		t.Fatal("the widened audience came back zero")
	}
}

type allowAllCrossOwner struct{}

func (allowAllCrossOwner) AllowsAny(context.Context, *types.Claims) bool { return true }
