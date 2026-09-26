package memory

import (
	"context"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/ids"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

const taskAdmissionTimeout = 5 * time.Second

// Cancellation/completion releases admission immediately, even before a worker
// projects the task outcome back onto the inbox. No wall clock is read in Apply.
func (s *InboxService) inboxConsumesSlot(item *pb.BotInboxItem) (bool, error) {
	if !outstandingInbox(item.Status) {
		return false, nil
	}
	if item.Status != pb.InboxStatus_INBOX_STATUS_WAITING || item.TaskId == "" {
		return true, nil
	}
	raw, err := s.store.Get(BucketTaskApprovals, item.TaskId)
	if errors.Is(err, types.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return true, err
	}
	task := new(pb.TaskApprovalRecord)
	if err := proto.Unmarshal(raw, task); err != nil {
		return true, err
	}
	switch task.State {
	case pb.TaskApprovalState_TASK_APPROVAL_STATE_CANCELLED, pb.TaskApprovalState_TASK_APPROVAL_STATE_COMPLETED, pb.TaskApprovalState_TASK_APPROVAL_STATE_DENIED, pb.TaskApprovalState_TASK_APPROVAL_STATE_EXPIRED:
		return false, nil
	case pb.TaskApprovalState_TASK_APPROVAL_STATE_OUTCOME_UNKNOWN:
		return task.Continuation != nil, nil
	default:
		return true, nil
	}
}

func (s *TaskApprovalStore) admit(ctx context.Context, task *pb.TaskApprovalRecord, q *pb.CreateTaskApprovalRequest) (*pb.BotInboxItem, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	item := proto.Clone(q.Inbox).(*pb.BotInboxItem)
	if err := validateInboxItem(item); err != nil {
		return nil, err
	}
	limit := q.InboxMaxPending
	if limit == 0 {
		limit = DefaultInboxMaxPending
	}
	if limit > DefaultInboxMaxPending {
		return nil, status.Error(codes.InvalidArgument, "invalid inbox capacity")
	}
	a := &pb.TaskAdmission{Task: task, Inbox: item, MaxPending: limit, ExpectedRevision: item.Revision, ExpectedClaimer: item.ClaimedBy}
	if item.Id == "" {
		item.Id, item.CreatedAt, item.Revision = ids.New(), task.CreatedAt, 1
		a.ExpectedRevision, a.ExpectedClaimer = 0, ""
	} else {
		item.Revision++
	}
	item.Status, item.TaskId, item.RequestedBy = pb.InboxStatus_INBOX_STATUS_WAITING, task.Id, task.Owner
	item.ClaimExpiresAt, item.CompletedAt = nil, nil
	item.Result, item.Error = "", ""
	task.Revision = 1
	task.SessionId = "bot:" + item.Recipient + ".task." + task.Id
	task.CoordinatorConversation = q.CoordinatorConversation
	raw, err := proto.Marshal(&pb.LogEntry{Op: pb.LogOp_LOG_OP_PUT, Payload: &pb.LogEntry_TaskAdmission{TaskAdmission: a}})
	if err != nil {
		return nil, err
	}
	result, err := s.raft.Apply(raw, taskAdmissionTimeout)
	if err == nil {
		if applied, ok := result.(error); ok {
			err = applied
		}
	}
	if errors.Is(err, ErrInboxFull) {
		return nil, status.Error(codes.ResourceExhausted, err.Error())
	}
	if errors.Is(err, ErrClaimConflict) {
		return nil, status.Error(codes.Aborted, "task admission changed; refresh before retrying")
	}
	if err != nil {
		return nil, err
	}
	return item, nil
}

// Both records are admitted in the same encrypted Bolt transaction, serialized
// by Raft. Rejected capacity/CAS/ownership checks publish neither record.
func (f *FSM) applyTaskAdmission(a *pb.TaskAdmission) error {
	if a == nil || a.Task == nil || a.Inbox == nil || a.MaxPending == 0 || a.MaxPending > DefaultInboxMaxPending {
		return errors.New("invalid task admission")
	}
	task, item := a.Task, a.Inbox
	if task.Actor != "bot:"+item.Recipient || task.Owner == "" || item.RequestedBy != task.Owner || item.TaskId != task.Id {
		return errors.New("task admission identity mismatch")
	}
	bot, err := NewBotService(nil, f.store).Get(context.Background(), item.Recipient)
	if err != nil || !bot.GetEnabled() || bot.GetOwner() != task.Owner {
		return errors.New("task recipient unavailable or ownership changed")
	}
	if task.CoordinatorConversation && !bot.IsCoordinator {
		return errors.New("task actor is not a coordinator")
	}
	if _, err := f.store.Get(BucketTaskApprovals, task.Id); !errors.Is(err, types.ErrNotFound) {
		return ErrClaimConflict
	}
	key := inboxKey(item.Recipient, item.Id)
	raw, err := f.store.Get(BucketBotInbox, key)
	if a.ExpectedRevision == 0 {
		if !errors.Is(err, types.ErrNotFound) {
			return ErrClaimConflict
		}
	} else {
		if err != nil {
			return ErrClaimConflict
		}
		var previous pb.BotInboxItem
		if err := proto.Unmarshal(raw, &previous); err != nil {
			return err
		}
		if previous.Revision != a.ExpectedRevision || previous.ClaimedBy != a.ExpectedClaimer || previous.Status != pb.InboxStatus_INBOX_STATUS_CLAIMED || previous.TaskId != "" || previous.RequestedBy != task.Owner {
			return ErrClaimConflict
		}
	}
	if err := f.checkInboxCapacity(&pb.LogEntry{Id: key, InboxMaxPending: a.MaxPending, Payload: &pb.LogEntry_BotInbox{BotInbox: item}}); err != nil {
		return err
	}
	return f.putTaskAdmission(task, item, key)
}

func (f *FSM) putTaskAdmission(task *pb.TaskApprovalRecord, item *pb.BotInboxItem, key string) error {
	records := []struct {
		bucket, key string
		message     proto.Message
	}{{BucketTaskApprovals, task.Id, task}, {BucketBotInbox, key, item}}
	sealed := make([][]byte, len(records))
	for i, rec := range records {
		raw, err := proto.Marshal(rec.message)
		if err != nil {
			return err
		}
		sealed[i], err = f.store.cipher.Seal(raw)
		if err != nil {
			return err
		}
	}
	return f.store.loadDB().Update(func(tx *bolt.Tx) error {
		for i, rec := range records {
			b := tx.Bucket([]byte(rec.bucket))
			if b == nil {
				return fmt.Errorf("missing bucket %s", rec.bucket)
			}
			if err := b.Put([]byte(rec.key), sealed[i]); err != nil {
				return err
			}
		}
		return nil
	})
}
