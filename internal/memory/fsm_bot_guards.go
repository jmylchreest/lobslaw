package memory

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func (f *FSM) checkBotWrite(bucket, id string, payload proto.Message) error {
	if bucket != BucketBots {
		return nil
	}
	raw, err := f.store.Get(bucket, id)
	if errors.Is(err, types.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var prev lobslawv1.BotRecord
	if err := proto.Unmarshal(raw, &prev); err != nil {
		return err
	}
	next := payload.(*lobslawv1.BotRecord)
	if prev.GetDeleted() || prev.GetOwner() != "" && next.GetOwner() != prev.GetOwner() {
		return fmt.Errorf("%w: bot %q identity is reserved", ErrClaimConflict, id)
	}
	return nil
}

// Raft serializes this check with the write. A service-side count alone lets
// concurrent posts/retries both take the last slot. Active-to-active updates
// already occupy a slot, so completion retries and claim handoffs still work.
func (f *FSM) checkInboxCapacity(entry *lobslawv1.LogEntry) error {
	item := entry.GetBotInbox()
	if item == nil || entry.GetInboxMaxPending() == 0 || !outstandingInbox(item.GetStatus()) {
		return nil
	}
	raw, err := f.store.Get(BucketBotInbox, entry.GetId())
	if err == nil {
		var prev lobslawv1.BotInboxItem
		if err := proto.Unmarshal(raw, &prev); err != nil {
			return err
		}
		if outstandingInbox(prev.GetStatus()) {
			return nil
		}
	} else if !errors.Is(err, types.ErrNotFound) {
		return err
	}
	svc := NewInboxService(nil, f.store, int(entry.GetInboxMaxPending()))
	n, err := svc.countPending(item.GetRecipient())
	if err != nil {
		return err
	}
	if n >= svc.maxPending {
		return ErrInboxFull
	}
	return nil
}

func outstandingInbox(status lobslawv1.InboxStatus) bool {
	return status == lobslawv1.InboxStatus_INBOX_STATUS_PENDING || status == lobslawv1.InboxStatus_INBOX_STATUS_CLAIMED
}
