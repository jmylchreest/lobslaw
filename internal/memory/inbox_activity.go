package memory

import (
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"

	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

const (
	bucketInboxActivity         = "inbox_activity_v1"
	InboxActivityTextBytes  int = 2048
	InboxActivityLinkBytes  int = 256
	InboxActivityToolBytes  int = 128
	InboxActivityMaxTools   int = 32
	InboxActivityMaxIDBytes int = 128
)

// Post mints ULIDs; completion adds "-result"; logical imports mint new IDs.
// Keep room for those suffixes, but never turn malformed keys into route aliases.
var inboxActivityIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

func inboxActivitySummary(key string, item *pb.BotInboxItem) (*pb.ConsoleInboxItem, error) {
	if !botIDPattern.MatchString(item.GetRecipient()) || len(item.GetId()) > InboxActivityMaxIDBytes ||
		!inboxActivityIDPattern.MatchString(item.GetId()) || key != inboxKey(item.GetRecipient(), item.GetId()) {
		return nil, fmt.Errorf("inbox activity: invalid record identity %q", key)
	}
	out := &pb.ConsoleInboxItem{
		Id: item.Id, Recipient: item.Recipient, Kind: InboxKindName(item.Kind),
		Priority: item.Priority, Status: InboxStatusName(item.Status), Attempts: item.Attempts,
		Revision: item.Revision, TokensUsed: item.TokensUsed, CostUsd: item.CostUsd,
		DetailPath: "/v1/inbox/" + item.Recipient + "/" + item.Id,
	}
	text := func(name, value string, limit int) string {
		if len(value) <= limit {
			return value
		}
		out.TruncatedFields = append(out.TruncatedFields, name)
		for !utf8.RuneStart(value[limit]) {
			limit--
		}
		return value[:limit]
	}
	link := func(name, value string) string {
		if len(value) <= InboxActivityLinkBytes {
			return value
		}
		out.TruncatedFields = append(out.TruncatedFields, name)
		return ""
	}
	out.Subject = text("subject", item.Subject, MaxInboxSubject)
	out.Result = text("result", item.Result, InboxActivityTextBytes)
	out.Error = text("error", item.Error, InboxActivityTextBytes)
	out.Sender = link("sender", item.Sender)
	out.RequestedBy = link("requested_by", item.RequestedBy)
	out.TaskId = link("task_id", item.TaskId)
	out.SessionId = link("session_id", item.SessionId)
	out.CorrelationId = link("correlation_id", item.CorrelationId)
	toolsTruncated := len(item.ToolsUsed) > InboxActivityMaxTools
	for _, tool := range item.ToolsUsed[:min(len(item.ToolsUsed), InboxActivityMaxTools)] {
		if len(tool) > InboxActivityToolBytes {
			toolsTruncated = true
			continue
		}
		out.ToolsUsed = append(out.ToolsUsed, tool)
	}
	if toolsTruncated {
		out.TruncatedFields = append(out.TruncatedFields, "tools_used")
	}
	if item.CreatedAt != nil {
		out.CreatedAt = item.CreatedAt.AsTime().UTC().Format(time.RFC3339)
	}
	if item.CompletedAt != nil {
		out.CompletedAt = item.CompletedAt.AsTime().UTC().Format(time.RFC3339)
	}
	return out, nil
}

// The projection is encrypted and committed in the authoritative mutation's
// transaction. It is not an independently writable inbox or a second Raft entry.
func (s *Store) updateInboxActivity(tx *bolt.Tx, key string, raw []byte) error {
	index := tx.Bucket([]byte(bucketInboxActivity))
	if index == nil {
		return fmt.Errorf("inbox activity projection unavailable")
	}
	if raw == nil {
		if err := s.updateInboxNotifications(tx, key, nil, nil); err != nil {
			return err
		}
		return index.Delete([]byte(key))
	}
	var item pb.BotInboxItem
	if err := proto.Unmarshal(raw, &item); err != nil {
		return fmt.Errorf("decode inbox activity source: %w", err)
	}
	summary, err := inboxActivitySummary(key, &item)
	if err != nil {
		return err
	}
	value, err := proto.MarshalOptions{Deterministic: true}.Marshal(summary)
	if err != nil {
		return fmt.Errorf("encode inbox activity: %w", err)
	}
	sealed, err := s.cipher.Seal(value)
	if err != nil {
		return err
	}
	if len(sealed) > MaxInboxRecentRecordBytes {
		return fmt.Errorf("inbox activity projection exceeds byte bound")
	}
	if err := s.updateInboxNotifications(tx, key, summary, sealed); err != nil {
		return err
	}
	return index.Put([]byte(key), sealed)
}

func (s *Store) rebuildInboxActivity(tx *bolt.Tx) error {
	if tx.Bucket([]byte(bucketInboxNotificationKeys)) != nil {
		if err := tx.DeleteBucket([]byte(bucketInboxNotificationKeys)); err != nil {
			return err
		}
	}
	if _, err := tx.CreateBucket([]byte(bucketInboxNotificationKeys)); err != nil {
		return err
	}
	if tx.Bucket([]byte(bucketInboxNotifications)) != nil {
		if err := tx.DeleteBucket([]byte(bucketInboxNotifications)); err != nil {
			return err
		}
	}
	if _, err := tx.CreateBucket([]byte(bucketInboxNotifications)); err != nil {
		return err
	}
	if tx.Bucket([]byte(bucketInboxActivity)) != nil {
		if err := tx.DeleteBucket([]byte(bucketInboxActivity)); err != nil {
			return err
		}
	}
	if _, err := tx.CreateBucket([]byte(bucketInboxActivity)); err != nil {
		return err
	}
	var buf []byte
	return tx.Bucket([]byte(BucketBotInbox)).ForEach(func(key, sealed []byte) error {
		raw, err := s.cipher.OpenTo(buf, sealed)
		if err != nil {
			return fmt.Errorf("decrypt inbox activity source: %w", err)
		}
		buf = raw
		return s.updateInboxActivity(tx, string(key), raw)
	})
}

// Rebuild existing projections too: an older binary may have changed the source
// records without maintaining them. This runs before publication, never on reads.
func (s *Store) rebuildDerived(tx *bolt.Tx) error {
	if err := s.rebuildTaskHistory(tx); err != nil {
		return err
	}
	return s.rebuildInboxActivity(tx)
}
