package memory

import (
	"fmt"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func putNotificationFixture(t *testing.T, store *Store, item *pb.BotInboxItem) {
	t.Helper()
	raw, err := proto.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(BucketBotInbox, inboxKey(item.Recipient, item.Id), raw); err != nil {
		t.Fatal(err)
	}
}

func TestNotificationOutboxRetainsLateFailureBeyondActivityWindow(t *testing.T) {
	store, _ := newTestStore(t)
	for i := 0; i <= 200; i++ {
		putNotificationFixture(t, store, &pb.BotInboxItem{Id: fmt.Sprintf("%026d", i), Recipient: "worker", RequestedBy: "user:alice", Sender: "bot:worker", Status: pb.InboxStatus_INBOX_STATUS_DONE, CreatedAt: timestamppb.Now()})
	}
	old := &pb.BotInboxItem{Id: fmt.Sprintf("%026d", 0), Recipient: "worker", RequestedBy: "user:alice", Status: pb.InboxStatus_INBOX_STATUS_FAILED, Error: "Late failure", CompletedAt: timestamppb.Now()}
	putNotificationFixture(t, store, old)
	svc := NewInboxService(nil, store, 0)
	recent, err := svc.Recent(t.Context(), "worker", 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range recent {
		if item.Id == old.Id {
			t.Fatal("fixture did not leave the recent-activity window")
		}
	}
	items, next, err := svc.NotificationPage(t.Context(), "worker", "", 20)
	if err != nil || next != "" || len(items) != 1 || items[0].Id != old.Id {
		t.Fatalf("lost late event: %v %q %v", items, next, err)
	}
}

func TestNotificationOutboxPagingAndRebuild(t *testing.T) {
	store, path := newTestStore(t)
	for i := 0; i < 301; i++ {
		putNotificationFixture(t, store, &pb.BotInboxItem{Id: fmt.Sprintf("%026d", i), Recipient: "worker", RequestedBy: "user:alice", Sender: "schedule:daily:always", Status: pb.InboxStatus_INBOX_STATUS_DONE, CompletedAt: timestamppb.Now()})
	}
	// Simulate a store written by an older binary, then rebuild on reopen.
	if err := store.loadDB().Update(func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket([]byte(bucketInboxNotifications)); err != nil {
			return err
		}
		return tx.DeleteBucket([]byte(bucketInboxNotificationKeys))
	}); err != nil {
		t.Fatal(err)
	}
	key := store.key
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	svc := NewInboxService(nil, reopened, 0)
	seen := map[string]bool{}
	for cursor := ""; ; {
		page, next, err := svc.NotificationPage(t.Context(), "worker", cursor, 17)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page {
			if seen[item.Id] {
				t.Fatal("duplicate page item")
			}
			seen[item.Id] = true
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 301 {
		t.Fatalf("paged %d of 301 events", len(seen))
	}
	if items, _, err := svc.NotificationPage(t.Context(), "other", "", 20); err != nil || len(items) != 0 {
		t.Fatal("cross-recipient index read")
	}
}

func TestNotificationIndexTransitionsAndDeleteAreAtomic(t *testing.T) {
	store, _ := newTestStore(t)
	svc := NewInboxService(nil, store, 0)
	item := &pb.BotInboxItem{Id: "old-task", Recipient: "worker", RequestedBy: "user:alice", Status: pb.InboxStatus_INBOX_STATUS_WAITING, CreatedAt: timestamppb.New(time.Now().Add(-48 * time.Hour))}
	putNotificationFixture(t, store, item)
	if items, _, err := svc.NotificationPage(t.Context(), "worker", "", 10); err != nil || len(items) != 1 {
		t.Fatal("active old work was excluded")
	}
	item.Status = pb.InboxStatus_INBOX_STATUS_DONE
	item.Sender = "schedule:daily:always"
	item.CompletedAt = timestamppb.Now()
	putNotificationFixture(t, store, item)
	if items, _, err := svc.NotificationPage(t.Context(), "worker", "", 10); err != nil || len(items) != 1 || items[0].Status != "done" {
		t.Fatal("old active index key survived completion")
	}
	if err := store.Delete(BucketBotInbox, inboxKey(item.Recipient, item.Id)); err != nil {
		t.Fatal(err)
	}
	if items, _, err := svc.NotificationPage(t.Context(), "worker", "", 10); err != nil || len(items) != 0 {
		t.Fatal("outbox survived source deletion")
	}
}

func TestNotificationIndexFailureRollsBackInboxMutation(t *testing.T) {
	store, _ := newTestStore(t)
	item := &pb.BotInboxItem{Id: "task", Recipient: "worker", Status: pb.InboxStatus_INBOX_STATUS_PENDING}
	putNotificationFixture(t, store, item)
	before, err := store.Get(BucketBotInbox, "worker:task")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.loadDB().Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte(bucketInboxNotifications)) }); err != nil {
		t.Fatal(err)
	}
	item.Status = pb.InboxStatus_INBOX_STATUS_FAILED
	item.CompletedAt = timestamppb.Now()
	raw, err := proto.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(BucketBotInbox, "worker:task", raw); err == nil {
		t.Fatal("inbox mutation committed without its notification index")
	}
	after, err := store.Get(BucketBotInbox, "worker:task")
	if err != nil || string(after) != string(before) {
		t.Fatal("source changed despite failed outbox write")
	}
}
