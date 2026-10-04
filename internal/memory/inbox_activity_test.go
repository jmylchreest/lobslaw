package memory

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/raft"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func largeActivityItem() *pb.BotInboxItem {
	return &pb.BotInboxItem{Id: "item", Recipient: "worker", Sender: "bot:chief", Body: "private prompt",
		Error: strings.Repeat("e", 70<<10), Result: strings.Repeat("r", MaxInboxResult),
		TaskClaims: &pb.Claims{UserId: strings.Repeat("secret-claims", 10000)},
		ToolsUsed:  append([]string{"read_file", strings.Repeat("t", 70000)}, slices.Repeat([]string{"tool"}, 10000)...),
		TaskId:     "task-id", SessionId: "session-id", CorrelationId: "correlation-id", Revision: 42,
	}
}

func putActivitySource(t *testing.T, store *Store, item *pb.BotInboxItem) {
	t.Helper()
	raw, err := proto.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(BucketBotInbox, inboxKey(item.Recipient, item.Id), raw); err != nil {
		t.Fatal(err)
	}
}

func assertActivityProjection(t *testing.T, store *Store, item *pb.BotInboxItem) {
	t.Helper()
	svc := NewInboxService(nil, store, 0)
	got, err := svc.Recent(t.Context(), item.Recipient, 500)
	if err != nil || len(got) != 1 {
		t.Fatalf("activity: %d %v", len(got), err)
	}
	summary := got[0]
	if summary.Id != item.Id || summary.Revision != item.Revision || summary.TaskId != item.TaskId || summary.SessionId != item.SessionId || summary.CorrelationId != item.CorrelationId {
		t.Fatalf("identity/links lost: %v", summary)
	}
	if len(summary.Error) != InboxActivityTextBytes || len(summary.Result) != InboxActivityTextBytes || len(summary.ToolsUsed) > InboxActivityMaxTools || summary.Body != "" || summary.DetailPath != "/v1/inbox/worker/"+item.Id {
		t.Fatalf("unbounded/incomplete projection: %v", summary)
	}
	for _, field := range []string{"error", "result", "tools_used"} {
		if !slices.Contains(summary.TruncatedFields, field) {
			t.Fatalf("missing truncation %s", field)
		}
	}
	if proto.Size(summary) > MaxInboxRecentRecordBytes {
		t.Fatal("unbounded projection")
	}
	full, err := svc.Get(t.Context(), item.Recipient, item.Id)
	if err != nil || !proto.Equal(full, item) {
		t.Fatalf("authoritative record changed: %v", err)
	}
}

func TestActivityProjectionResolveLargeEvidenceAndReceipt(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 0)
	item := largeActivityItem()
	posted, err := svc.Post(t.Context(), item)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := svc.Resolve(t.Context(), posted.Recipient, posted.Id, InboxOutcome{
		Err: errors.New(item.Error), Result: item.Result, ToolsUsed: item.ToolsUsed, SessionID: item.SessionId,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertActivityProjection(t, svc.store, resolved)
	queue, err := svc.List(t.Context(), "worker", InboxFilter{})
	if err != nil || len(queue) != 1 || !proto.Equal(queue[0], resolved) {
		t.Fatalf("queue changed: %v", err)
	}
	receipts, err := svc.Recent(t.Context(), "chief", 500)
	if err != nil || len(receipts) != 1 || receipts[0].Status != "failed" || receipts[0].CorrelationId != posted.Id || !slices.Contains(receipts[0].TruncatedFields, "error") {
		t.Fatalf("completion receipt missing: %v %v", receipts, err)
	}
}

func TestActivityProjectionOversizedOptionalLinksAndIdentity(t *testing.T) {
	t.Parallel()
	item := largeActivityItem()
	item.TaskId, item.SessionId, item.CorrelationId = strings.Repeat("x", 70000), strings.Repeat("y", 70000), strings.Repeat("z", 70000)
	summary, err := inboxActivitySummary("worker:item", item)
	if err != nil {
		t.Fatal(err)
	}
	if summary.TaskId != "" || summary.SessionId != "" || summary.CorrelationId != "" || summary.DetailPath != "/v1/inbox/worker/item" {
		t.Fatal("truncated identifier advertised as a valid link")
	}
	for _, name := range []string{"task_id", "session_id", "correlation_id"} {
		if !slices.Contains(summary.TruncatedFields, name) {
			t.Fatalf("missing omitted link marker %s", name)
		}
	}
	for _, id := range []string{"", "a/b", "..", "x:y", strings.Repeat("x", InboxActivityMaxIDBytes+1)} {
		item.Id = id
		if _, err := inboxActivitySummary(inboxKey(item.Recipient, id), item); err == nil {
			t.Fatalf("invalid id accepted %q", id)
		}
	}
	item.Id = "item"
	if _, err := inboxActivitySummary("worker:other", item); err == nil {
		t.Fatal("identity mismatch accepted")
	}
}

func TestActivityProjectionRebuildAndRestore(t *testing.T) {
	t.Parallel()
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprint(stale), func(t *testing.T) {
			store, path := newTestStore(t)
			item := largeActivityItem()
			putActivitySource(t, store, item)
			if err := store.loadDB().Update(func(tx *bolt.Tx) error {
				if err := tx.DeleteBucket([]byte(bucketInboxActivity)); err != nil {
					return err
				}
				if stale {
					_, err := tx.CreateBucket([]byte(bucketInboxActivity))
					return err
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var snapshot bytes.Buffer
			if err := store.WriteSnapshot(&snapshot); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenStore(path, store.key)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			assertActivityProjection(t, reopened, item)
			if err := reopened.Delete(BucketBotInbox, "worker:item"); err != nil {
				t.Fatal(err)
			}
			if err := NewFSM(reopened).Restore(io.NopCloser(bytes.NewReader(snapshot.Bytes()))); err != nil {
				t.Fatal(err)
			}
			assertActivityProjection(t, reopened, item)
		})
	}
}

func TestActivityProjectionFSMCASDeleteAndAdmission(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	fsm := NewFSM(store)
	item := largeActivityItem()
	apply := func(op pb.LogOp, rev *uint64) error {
		t.Helper()
		raw, err := proto.Marshal(&pb.LogEntry{Id: "worker:item", Op: op, ExpectedRevision: rev, Payload: &pb.LogEntry_BotInbox{BotInbox: item}})
		if err != nil {
			t.Fatal(err)
		}
		result := fsm.Apply(&raft.Log{Data: raw})
		if err, ok := result.(error); ok {
			return err
		}
		return nil
	}
	if err := apply(pb.LogOp_LOG_OP_PUT, nil); err != nil {
		t.Fatal(err)
	}
	item.Revision = 1
	assertActivityProjection(t, store, item)
	item.Status = pb.InboxStatus_INBOX_STATUS_DONE
	rev := uint64(1)
	if err := apply(pb.LogOp_LOG_OP_CLAIM, &rev); err != nil {
		t.Fatal(err)
	}
	item.Revision = 2
	assertActivityProjection(t, store, item)
	if err := apply(pb.LogOp_LOG_OP_CLAIM, &rev); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("stale CAS: %v", err)
	}
	assertActivityProjection(t, store, item)
	if err := apply(pb.LogOp_LOG_OP_DELETE, nil); err != nil {
		t.Fatal(err)
	}
	got, err := NewInboxService(nil, store, 0).Recent(t.Context(), "worker", 1)
	if err != nil || len(got) != 0 {
		t.Fatalf("stale projection after delete: %v %v", got, err)
	}
	if err := fsm.putTaskAdmission(&pb.TaskApprovalRecord{Id: "task-id"}, item, "worker:item"); err != nil {
		t.Fatal(err)
	}
	assertActivityProjection(t, store, item)
	// Failure after derived writes must roll everything back, including projection.
	item.Id = "rollback"
	if err := store.loadDB().Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte(BucketBotInbox)) }); err != nil {
		t.Fatal(err)
	}
	if err := fsm.putTaskAdmission(&pb.TaskApprovalRecord{Id: "failed"}, item, "worker:rollback"); err == nil {
		t.Fatal("expected failed admission")
	}
	got, err = NewInboxService(nil, store, 0).Recent(t.Context(), "worker", 500)
	if err != nil || len(got) != 1 || got[0].Id != "item" {
		t.Fatalf("torn admission projection: %v %v", got, err)
	}
}

func TestActivityReadsOnlyProjectionAndArchiveMaintainsIt(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	fsm := NewFSM(store)
	item := largeActivityItem()
	raw, err := proto.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(mutation *pb.ArchiveMutation) error {
		return store.loadDB().Update(func(tx *bolt.Tx) error {
			_, err := fsm.applyArchiveMutation(tx, mutation)
			return err
		})
	}
	if err := mutate(&pb.ArchiveMutation{Kind: "inbox", Id: "worker:item", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	item.Revision = 1
	assertActivityProjection(t, store, item)
	// Activity must never read/decrypt the full record, even inside its window.
	if err := store.loadDB().Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketBotInbox)).Put([]byte("worker:item"), []byte("not ciphertext"))
	}); err != nil {
		t.Fatal(err)
	}
	got, err := NewInboxService(nil, store, 0).Recent(t.Context(), "worker", 500)
	if err != nil || len(got) != 1 {
		t.Fatalf("activity read full evidence: %v", err)
	}
	putActivitySource(t, store, item)
	if err := mutate(&pb.ArchiveMutation{Kind: "inbox", Id: "worker:item", Delete: true}); err != nil {
		t.Fatal(err)
	}
	got, err = NewInboxService(nil, store, 0).Recent(t.Context(), "worker", 500)
	if err != nil || len(got) != 0 {
		t.Fatalf("archive deletion left projection: %v %v", got, err)
	}
}

func TestActivitySummaryWorstCaseBoundsAndUTF8(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	item := largeActivityItem()
	item.Id = strings.Repeat("i", InboxActivityMaxIDBytes)
	item.Recipient = strings.Repeat("b", 63)
	item.Error, item.Result, item.Subject = strings.Repeat("€", 30000), strings.Repeat("€", 30000), strings.Repeat("€", 1000)
	item.Sender, item.RequestedBy = strings.Repeat("s", InboxActivityLinkBytes), strings.Repeat("u", InboxActivityLinkBytes)
	item.TaskId, item.SessionId, item.CorrelationId = item.Sender, item.Sender, item.Sender
	item.ToolsUsed = slices.Repeat([]string{strings.Repeat("t", InboxActivityToolBytes)}, InboxActivityMaxTools)
	putActivitySource(t, store, item)
	got, err := NewInboxService(nil, store, 0).Recent(t.Context(), item.Recipient, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("maximum metadata rejected: %v", err)
	}
	if proto.Size(got[0]) >= MaxInboxRecentRecordBytes {
		t.Fatal("worst-case summary exceeds bound")
	}
	if !strings.HasSuffix(got[0].Error, "€") || !strings.HasSuffix(got[0].Subject, "€") {
		t.Fatal("split a UTF-8 character")
	}
	if got[0].TaskId != item.TaskId || got[0].SessionId != item.SessionId || got[0].CorrelationId != item.CorrelationId {
		t.Fatal("boundary-sized link omitted")
	}
}
