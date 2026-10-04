package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

const bucketInboxNotifications = "inbox_notifications_v1"
const bucketInboxNotificationKeys = "inbox_notification_keys_v1"
const MaxNotificationPage = 256

// Candidates are indexed by current state, not inbox arrival. Active work is
// retained until resolved; terminal outcomes are ordered by their completion.
// Selection is deterministic so the index can be rebuilt from source records.
func notificationIndexKey(item *pb.ConsoleInboxItem) string {
	if item == nil {
		return ""
	}
	prefix := item.Recipient + ":"
	if item.Status == "waiting" {
		return prefix + "a:" + item.Id
	}
	candidate := item.Status == "failed" || (item.Status == "done" && (strings.HasPrefix(item.Sender, "notify:") || (strings.HasPrefix(item.Sender, "schedule:") && strings.HasSuffix(item.Sender, ":always"))))
	if !candidate {
		return ""
	}
	at, err := time.Parse(time.RFC3339Nano, item.CompletedAt)
	if err != nil {
		at, err = time.Parse(time.RFC3339Nano, item.CreatedAt)
	}
	if err != nil {
		return ""
	}
	return prefix + "z:" + at.UTC().Format("20060102T150405.000000000Z") + ":" + item.Id
}

func (s *Store) updateInboxNotifications(tx *bolt.Tx, itemKey string, next *pb.ConsoleInboxItem, sealed []byte) error {
	index := tx.Bucket([]byte(bucketInboxNotifications))
	keys := tx.Bucket([]byte(bucketInboxNotificationKeys))
	if index == nil || keys == nil {
		return errors.New("notification outbox unavailable")
	}
	if old := keys.Get([]byte(itemKey)); old != nil {
		if err := index.Delete(old); err != nil {
			return err
		}
	}
	if key := notificationIndexKey(next); key != "" {
		if err := index.Put([]byte(key), sealed); err != nil {
			return err
		}
		return keys.Put([]byte(itemKey), []byte(key))
	}
	return keys.Delete([]byte(itemKey))
}

// NotificationPage reads the durable candidate index in bounded pages. This is
// deliberately separate from Recent, whose arrival window is a UI concern.
func (s *InboxService) NotificationPage(ctx context.Context, recipient, after string, limit int) ([]*pb.ConsoleInboxItem, string, error) {
	if s.store == nil || !botIDPattern.MatchString(recipient) {
		return nil, "", errors.New("invalid notification recipient/store")
	}
	if limit < 1 || limit > MaxNotificationPage || len(after) > 512 {
		return nil, "", errors.New("invalid notification page")
	}
	if after != "" && !strings.HasPrefix(after, "a:") && !strings.HasPrefix(after, "z:") {
		return nil, "", errors.New("invalid notification cursor")
	}
	prefix := []byte(recipient + ":")
	cutoff := []byte(recipient + ":z:" + time.Now().Add(-24*time.Hour).UTC().Format("20060102T150405.000000000Z"))
	var items []*pb.ConsoleInboxItem
	var next string
	err := s.store.loadDB().View(func(tx *bolt.Tx) error {
		index := tx.Bucket([]byte(bucketInboxNotifications))
		if index == nil {
			return errors.New("notification outbox unavailable; reopen writable store")
		}
		cursor := index.Cursor()
		key, value := cursor.Seek(append(bytes.Clone(prefix), []byte(after)...))
		if after != "" && bytes.Equal(key, append(bytes.Clone(prefix), []byte(after)...)) {
			key, value = cursor.Next()
		}
		for bytes.HasPrefix(key, prefix) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if bytes.HasPrefix(key, []byte(recipient+":z:")) && bytes.Compare(key, cutoff) < 0 {
				key, value = cursor.Seek(cutoff)
				continue
			}
			if len(items) == limit {
				return nil
			}
			if len(value) > MaxInboxRecentRecordBytes {
				return fmt.Errorf("notification record exceeds byte limit")
			}
			raw, err := s.store.cipher.OpenTo(nil, value)
			if err != nil {
				return err
			}
			item := new(pb.ConsoleInboxItem)
			if err := proto.Unmarshal(raw, item); err != nil {
				return err
			}
			if item.Recipient != recipient || notificationIndexKey(item) != string(key) {
				return errors.New("notification index identity mismatch")
			}
			items = append(items, item)
			next = string(key[len(prefix):])
			key, value = cursor.Next()
		}
		next = ""
		return nil
	})
	return items, next, err
}
