package memory

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/jmylchreest/lobslaw/internal/archive"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestInboxArchiveIdentityRoundTrip(t *testing.T) {
	t.Parallel()
	item := &lobslawv1.BotInboxItem{Id: "item", Recipient: "worker", Body: "test", Status: lobslawv1.InboxStatus_INBOX_STATUS_PENDING}
	data, err := protojson.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeArchiveRecord(archive.Record{Kind: "inbox", ID: inboxKey(item.Recipient, item.Id), Data: data}); err != nil {
		t.Fatal(err)
	}
	records := []archive.Record{{Kind: "inbox", ID: inboxKey(item.Recipient, item.Id), Data: data}}
	opts := ArchiveImportOptions{SourceID: "original-cluster"}
	plan, err := PlanArchiveImport(nil, records, opts)
	if err != nil {
		t.Fatalf("tracked import: %v", err)
	}
	for _, record := range plan.Additions {
		msg, err := decodeArchiveRecord(record)
		if err != nil {
			t.Fatalf("import produced invalid record: %v", err)
		}
		if rec, ok := msg.(*lobslawv1.BotInboxItem); ok && rec.GetStatus() != lobslawv1.InboxStatus_INBOX_STATUS_CANCELLED {
			t.Fatal("imported work was not paused")
		}
	}
	again, err := PlanArchiveImport(plan.Additions, records, opts)
	if err != nil || len(again.Additions) != 0 || len(again.Conflicts) != 0 {
		t.Fatalf("unchanged retry: %+v %v", again, err)
	}
}

func TestInboxResolutionRequiresOriginalClaim(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 0)
	ctx := context.Background()
	item := post(t, svc, "worker", "do work", 0)
	claim, err := svc.Claim(ctx, "worker", "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Resolve(ctx, "worker", item.Id, InboxOutcome{Result: "first result", ClaimRevision: claim.Revision, Claimer: claim.ClaimedBy}); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Resolve(ctx, "worker", item.Id, InboxOutcome{Result: "stale", ClaimRevision: claim.Revision, Claimer: claim.ClaimedBy})
	if !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("stale resolution: %v", err)
	}
}

func TestInboxJournalNeverBecomesWork(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 0)
	ctx := context.Background()
	_, err := svc.Journal(ctx, &lobslawv1.BotInboxItem{Recipient: "worker", Body: "already answered", Result: "answer"})
	if err != nil {
		t.Fatal(err)
	}
	item, err := svc.Claim(ctx, "worker", "node")
	if err != nil || item != nil {
		t.Fatalf("journal became work: %v %v", item, err)
	}
}
