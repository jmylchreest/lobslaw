package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

const (
	// MaxInboxRecentItems bounds a timeline read independently of queue size.
	MaxInboxRecentItems int = 1000
	// MaxInboxRecentRecordBytes includes encryption overhead. Together with the
	// item cap this bounds decrypted input to at most 64 MiB per recipient.
	// Legacy oversized records fail explicitly rather than allocating on read.
	MaxInboxRecentRecordBytes int = 64 << 10
)

// Recent reads the newest arrivals, regardless of priority or execution status.
// Unlike List (the execution queue), it stops at the key window before decrypting
// or decoding older records. No secondary index or write-time migration is needed.
func (s *InboxService) Recent(ctx context.Context, recipient string, limit int) ([]*pb.BotInboxItem, error) {
	if s.store == nil {
		return nil, errors.New("inbox: store not wired")
	}
	recipient = strings.TrimSpace(recipient)
	if recipient == "" {
		return nil, errors.New("inbox: recipient required")
	}
	if limit <= 0 || limit > MaxInboxRecentItems {
		return nil, fmt.Errorf("inbox: recent limit must be between 1 and %d", MaxInboxRecentItems)
	}
	out := make([]*pb.BotInboxItem, 0, limit)
	err := s.store.loadDB().View(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		bucket := tx.Bucket([]byte(BucketBotInbox))
		if bucket == nil {
			return errors.New("inbox: bucket unavailable")
		}
		prefix := []byte(recipient + ":")
		// The final ':' has a successor, giving an exclusive prefix upper bound.
		upper := bytes.Clone(prefix)
		upper[len(upper)-1]++
		cursor := bucket.Cursor()
		key, _ := cursor.Seek(upper)
		var sealed []byte
		if key == nil {
			key, sealed = cursor.Last()
		} else {
			key, sealed = cursor.Prev()
		}
		var buf []byte
		for ; len(out) < limit && bytes.HasPrefix(key, prefix); key, sealed = cursor.Prev() {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(sealed) > MaxInboxRecentRecordBytes {
				return fmt.Errorf("inbox: recent record %q exceeds byte limit %d", key, MaxInboxRecentRecordBytes)
			}
			raw, err := s.store.cipher.OpenTo(buf, sealed)
			if err != nil {
				return fmt.Errorf("inbox: decrypt recent %q: %w", key, err)
			}
			buf = raw
			item := new(pb.BotInboxItem)
			if err := proto.Unmarshal(raw, item); err != nil {
				return fmt.Errorf("inbox: unmarshal recent %q: %w", key, err)
			}
			if item.GetRecipient() != recipient || string(key) != inboxKey(recipient, item.GetId()) {
				return fmt.Errorf("inbox: recent record %q identity mismatch", key)
			}
			out = append(out, item)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("inbox: read recent: %w", err)
	}
	return out, nil
}
