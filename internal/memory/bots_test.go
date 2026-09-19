package memory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/identity"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func newTestBots(t *testing.T) *BotService {
	t.Helper()
	raft, fsm := newTestRaft(t)
	return NewBotService(raft, fsm.store)
}

func TestBotCreateAndGet(t *testing.T) {
	t.Parallel()
	svc := newTestBots(t)
	ctx := context.Background()

	got, err := svc.Put(ctx, &lobslawv1.BotRecord{
		Id:           "engineering",
		DisplayName:  "Engineering",
		Instructions: "You are the engineer for XYZ.",
		Owner:        identity.User("alice").String(),
		Tools:        []string{"read_file", "grep"},
		Enabled:      true,
	}, 0)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got.GetRevision() != 1 {
		t.Errorf("revision = %d, want 1 on create", got.GetRevision())
	}
	if got.GetCreatedAt() == nil {
		t.Error("created_at was not stamped")
	}
	if got.GetOwner() != "user:alice" {
		t.Errorf("owner = %q, want user:alice", got.GetOwner())
	}

	read, err := svc.Get(ctx, "engineering")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if read.GetInstructions() != "You are the engineer for XYZ." {
		t.Errorf("instructions = %q", read.GetInstructions())
	}
	if len(read.GetTools()) != 2 {
		t.Errorf("tools = %v, want the allowlist that was written", read.GetTools())
	}
}

func TestBotGetUnknownIsNotFound(t *testing.T) {
	t.Parallel()
	svc := newTestBots(t)
	if _, err := svc.Get(context.Background(), "nobody"); !errors.Is(err, ErrBotNotFound) {
		t.Errorf("err = %v, want ErrBotNotFound", err)
	}
}

// A stale reader must not silently lose somebody else's edit. Same CAS
// contract as the soul overlay, and for the same reason: every write
// replaces the whole record from the writer's own read.
func TestBotStaleWriteIsRejected(t *testing.T) {
	t.Parallel()
	svc := newTestBots(t)
	ctx := context.Background()

	if _, err := svc.Put(ctx, &lobslawv1.BotRecord{
		Id: "marketing", Owner: "user:alice", Enabled: true,
	}, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Put(ctx, &lobslawv1.BotRecord{
		Id: "marketing", Owner: "user:alice", Instructions: "first",
	}, 1); err != nil {
		t.Fatalf("first update: %v", err)
	}
	_, err := svc.Put(ctx, &lobslawv1.BotRecord{
		Id: "marketing", Owner: "user:alice", Instructions: "second",
	}, 1)
	if !errors.Is(err, ErrClaimConflict) {
		t.Errorf("err = %v, want ErrClaimConflict for a write from a stale read", err)
	}

	read, err := svc.Get(ctx, "marketing")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if read.GetInstructions() != "first" {
		t.Errorf("instructions = %q, want the first update to have survived", read.GetInstructions())
	}
}

func TestBotCreateWithNonZeroRevisionIsRejected(t *testing.T) {
	t.Parallel()
	svc := newTestBots(t)
	_, err := svc.Put(context.Background(), &lobslawv1.BotRecord{Id: "ghost", Owner: "user:alice"}, 7)
	if !errors.Is(err, ErrClaimConflict) {
		t.Errorf("err = %v, want ErrClaimConflict creating against a revision that never existed", err)
	}
}

// An id becomes a principal, a soul-overlay key suffix and a policy
// subject. A colon or a space in one silently changes what a rule
// matches, so the registry is the place that refuses it.
func TestBotIDIsValidated(t *testing.T) {
	t.Parallel()
	svc := newTestBots(t)
	ctx := context.Background()
	for _, id := range []string{"", "Engineering", "eng ops", "bot:eng", "-eng", strings.Repeat("a", 64)} {
		if _, err := svc.Put(ctx, &lobslawv1.BotRecord{Id: id, Owner: "user:alice"}, 0); err == nil {
			t.Errorf("Put accepted id %q", id)
		}
	}
}

// The brief rides on every turn this bot takes, so an unbounded one is
// an unbounded per-turn cost nothing else would report.
func TestBotInstructionsAreCapped(t *testing.T) {
	t.Parallel()
	svc := newTestBots(t)
	_, err := svc.Put(context.Background(), &lobslawv1.BotRecord{
		Id:           "verbose",
		Owner:        "user:alice",
		Instructions: strings.Repeat("x", MaxBotInstructions+1),
	}, 0)
	if err == nil {
		t.Fatal("Put accepted instructions over the cap")
	}
	if !strings.Contains(err.Error(), "every turn") {
		t.Errorf("error does not say why the cap exists: %v", err)
	}
}

func TestBotOwnerMustBeAHumanPrincipal(t *testing.T) {
	t.Parallel()
	svc := newTestBots(t)
	ctx := context.Background()
	for _, owner := range []string{"alice", "bot:engineering", "chat:telegram:-100", "user:"} {
		if _, err := svc.Put(ctx, &lobslawv1.BotRecord{Id: "owned", Owner: owner}, 0); err == nil {
			t.Errorf("Put accepted owner %q", owner)
		}
	}
}

func TestBotDeleteRemovesIt(t *testing.T) {
	t.Parallel()
	svc := newTestBots(t)
	ctx := context.Background()

	if _, err := svc.Put(ctx, &lobslawv1.BotRecord{Id: "temporary", Owner: "user:alice"}, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Delete(ctx, "temporary"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Get(ctx, "temporary"); !errors.Is(err, ErrBotNotFound) {
		t.Errorf("err = %v, want ErrBotNotFound after delete", err)
	}
}

// The chief's soul-overlay key must be the pre-existing constant, or
// an upgraded cluster wakes up with a default personality — a silent
// regression that lands on the user, not the operator.
func TestChiefKeepsThePreExistingSoulKey(t *testing.T) {
	t.Parallel()
	const want = "soul:tune"
	if SoulTuneRecordID != want {
		t.Errorf("SoulTuneRecordID = %q, want the pre-existing %q", SoulTuneRecordID, want)
	}
	if got := SoulTuneRecordIDFor(ChiefBotID); got != SoulTuneRecordID {
		t.Errorf("chief overlay key = %q, want the pre-existing %q", got, SoulTuneRecordID)
	}
	if got := SoulTuneRecordIDFor(""); got != SoulTuneRecordID {
		t.Errorf("unnamed bot overlay key = %q, want the chief's %q", got, SoulTuneRecordID)
	}
	if got, want := SoulTuneRecordIDFor("engineering"), SoulTuneRecordID+":engineering"; got != want {
		t.Errorf("bot overlay key = %q, want %q", got, want)
	}
}

func TestEmptyOwnerAndEmptyPrincipalCannotModify(t *testing.T) {
	t.Parallel()
	owned := &lobslawv1.BotRecord{Id: "engineering", Owner: "user:alice"}
	unowned := &lobslawv1.BotRecord{Id: "orphan", Owner: ""}

	if MayModify(owned, "") {
		t.Error("empty principal was allowed to modify an owned bot")
	}
	if MayModify(owned, "   ") {
		t.Error("whitespace principal was allowed to modify an owned bot")
	}
	if !MayModify(owned, "user:alice") {
		t.Error("the owner was refused")
	}
	if MayModify(owned, "user:bob") {
		t.Error("a different human was allowed to modify someone else's bot")
	}
	if MayModify(unowned, "user:alice") {
		t.Error("empty owner was treated as editable — unowned is nobody, not public")
	}
	if MayModify(unowned, "") {
		t.Error("empty principal and empty owner both passed")
	}
}

func TestAdoptUnownedAssignsUniqueOperator(t *testing.T) {
	t.Parallel()
	svc := newTestBots(t)
	ctx := context.Background()

	if _, err := svc.Put(ctx, &lobslawv1.BotRecord{Id: "orphan"}, 0); err != nil {
		t.Fatalf("create unowned: %v", err)
	}
	if err := svc.AdoptUnowned(ctx, identity.UniqueOperator([]string{"alice"})); err != nil {
		t.Fatalf("AdoptUnowned: %v", err)
	}
	got, err := svc.Get(ctx, "orphan")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.GetOwner() != "user:alice" {
		t.Errorf("owner = %q, want user:alice", got.GetOwner())
	}
}

func TestAdoptUnownedLeavesAmbiguousInaccessible(t *testing.T) {
	t.Parallel()
	svc := newTestBots(t)
	ctx := context.Background()

	if _, err := svc.Put(ctx, &lobslawv1.BotRecord{Id: "orphan"}, 0); err != nil {
		t.Fatalf("create unowned: %v", err)
	}
	if err := svc.AdoptUnowned(ctx, identity.UniqueOperator([]string{"alice", "bob"})); err != nil {
		t.Fatalf("AdoptUnowned: %v", err)
	}
	got, err := svc.Get(ctx, "orphan")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.GetOwner() != "" {
		t.Errorf("owner = %q, want empty — ambiguous must not invent an owner", got.GetOwner())
	}
	if MayModify(got, "user:alice") {
		t.Error("an unowned bot after a failed adoption became editable")
	}
}

func TestBotsAreExportable(t *testing.T) {
	t.Parallel()
	for _, k := range archiveKinds {
		if k.bucket == BucketBots {
			return
		}
	}
	t.Errorf("%q is not in archiveKinds; bots would not survive a backup/restore", BucketBots)
}

func TestBotRecordRoundTripsArchive(t *testing.T) {
	t.Parallel()
	svc := newTestBots(t)
	ctx := context.Background()
	if _, err := svc.Put(ctx, &lobslawv1.BotRecord{
		Id:           "engineering",
		DisplayName:  "Engineering",
		Instructions: "You are the engineer.",
		Owner:        "user:alice",
		Tools:        []string{"read_file"},
		Enabled:      true,
	}, 0); err != nil {
		t.Fatalf("Put: %v", err)
	}

	records, err := svc.store.ArchiveRecords(ctx)
	if err != nil {
		t.Fatalf("ArchiveRecords: %v", err)
	}
	var found bool
	for _, r := range records {
		if r.Kind != "bots" || r.ID != "engineering" {
			continue
		}
		found = true
		got, err := decodeArchiveRecord(r)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		bot, ok := got.(*lobslawv1.BotRecord)
		if !ok {
			t.Fatalf("decoded %T, want BotRecord", got)
		}
		if bot.GetId() != "engineering" || bot.GetOwner() != "user:alice" {
			t.Errorf("round-trip id/owner = %q/%q", bot.GetId(), bot.GetOwner())
		}
		if bot.GetInstructions() != "You are the engineer." {
			t.Errorf("instructions = %q", bot.GetInstructions())
		}
		if len(bot.GetTools()) != 1 || bot.GetTools()[0] != "read_file" {
			t.Errorf("tools = %v", bot.GetTools())
		}
	}
	if !found {
		t.Fatal("bot record missing from archive")
	}
}

func TestArchiveStillExcludesCredentials(t *testing.T) {
	t.Parallel()
	for _, k := range archiveKinds {
		if k.bucket == BucketCredentials {
			t.Errorf("%q is in archiveKinds; credentials must never be exported", BucketCredentials)
		}
	}
}
