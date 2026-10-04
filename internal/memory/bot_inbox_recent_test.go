package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func putRecentInboxItem(t *testing.T, store *Store, recipient, id string, priority int32) {
	t.Helper()
	raw, err := proto.Marshal(&pb.BotInboxItem{Id: id, Recipient: recipient, Priority: priority, Status: pb.InboxStatus_INBOX_STATUS_PENDING})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(BucketBotInbox, inboxKey(recipient, id), raw); err != nil {
		t.Fatal(err)
	}
}

func TestInboxRecentUsesArrivalWindowNotExecutionOrder(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	svc := NewInboxService(nil, store, 0)
	for _, recipient := range []string{"a", "worker", "worker-extra", "z"} {
		putRecentInboxItem(t, store, recipient, "01", 10)
		putRecentInboxItem(t, store, recipient, "02", 10)
		putRecentInboxItem(t, store, recipient, "03", -10)
	}
	for _, recipient := range []string{"a", "worker", "worker-extra", "z", "absent", "work"} {
		for _, limit := range []int{1, 2, 3, MaxInboxRecentItems} {
			items, err := svc.Recent(t.Context(), recipient, limit)
			if err != nil {
				t.Fatal(err)
			}
			want := min(limit, 3)
			if recipient == "absent" || recipient == "work" {
				want = 0
			}
			if len(items) != want {
				t.Fatalf("%s limit %d: got %d items, want %d", recipient, limit, len(items), want)
			}
			for i, item := range items {
				if item.Id != fmt.Sprintf("%02d", 3-i) || item.Recipient != recipient {
					t.Fatalf("wrong latest window: %v", items)
				}
			}
		}
	}
	queue, err := svc.List(t.Context(), "worker", InboxFilter{Limit: 2})
	if err != nil || len(queue) != 2 || queue[0].Id != "01" || queue[1].Id != "02" {
		t.Fatalf("execution priority/FIFO order changed: %v %v", queue, err)
	}
}

func TestInboxRecentDoesNotDecryptOrDecodeOutsideWindow(t *testing.T) {
	t.Parallel()
	for _, poison := range []string{"ciphertext", "protobuf"} {
		t.Run(poison, func(t *testing.T) {
			t.Parallel()
			store, _ := newTestStore(t)
			const rows = 2000
			err := store.loadDB().Update(func(tx *bolt.Tx) error {
				bad := []byte("invalid ciphertext")
				if poison == "protobuf" {
					var err error
					bad, err = store.cipher.Seal([]byte{0xff})
					if err != nil {
						return err
					}
				}
				for i := range rows {
					if err := tx.Bucket([]byte(bucketInboxActivity)).Put([]byte(inboxKey("worker", fmt.Sprintf("%04d", i))), bad); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			putRecentInboxItem(t, store, "worker", "1998", 20)
			putRecentInboxItem(t, store, "worker", "1999", -20)
			svc := NewInboxService(nil, store, 0)
			items, err := svc.Recent(t.Context(), "worker", 2)
			if err != nil || len(items) != 2 || items[0].Id != "1999" || items[1].Id != "1998" {
				t.Fatalf("read beyond latest window: %v %v", items, err)
			}
			// The corruption is real: moving it into the window must fail.
			if _, err := svc.Recent(t.Context(), "worker", 3); err == nil {
				t.Fatal("corrupt row in requested window was ignored")
			}
		})
	}
}

func TestInboxRecentLimitsBeforeDecryption(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	svc := NewInboxService(nil, store, 0)
	err := store.loadDB().Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketInboxActivity)).Put([]byte(inboxKey("worker", "01")), bytes.Repeat([]byte("x"), MaxInboxRecentRecordBytes+1))
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{-1, 0, MaxInboxRecentItems + 1, int(^uint(0) >> 1)} {
		if _, err := svc.Recent(t.Context(), "worker", limit); err == nil || !strings.Contains(err.Error(), "recent limit") {
			t.Fatalf("invalid limit %d reached store: %v", limit, err)
		}
	}
	if _, err := svc.Recent(t.Context(), "worker", 1); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("oversized ciphertext reached decryption: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := svc.Recent(ctx, "worker", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read: %v", err)
	}
	if _, err := svc.Recent(t.Context(), " ", 1); err == nil {
		t.Fatal("empty recipient accepted")
	}
	if _, err := NewInboxService(nil, nil, 0).Recent(t.Context(), "worker", 1); err == nil {
		t.Fatal("missing store accepted")
	}
}
