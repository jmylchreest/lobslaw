package memory

import (
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func setTaskEvidence(task *pb.TaskApprovalRecord, messages []*pb.SessionMessage, receipts []*pb.TurnToolInvocation, start uint32) error {
	if len(messages) > 0 {
		if int(start) > len(messages) {
			return status.Error(codes.InvalidArgument, "invalid task transcript boundary")
		}
		task.Transcript = nil
		task.TranscriptStart = start
		for _, message := range messages[start:] {
			m := proto.Clone(message).(*pb.SessionMessage)
			m.PreparedToolCall, m.BudgetPending = nil, false
			m.TurnId = task.TurnId
			m.SessionId, m.Seq = task.SessionId, uint64(len(task.Transcript)+1)
			task.Transcript = append(task.Transcript, m)
		}
	}
	for _, receipt := range receipts {
		task.Receipts = append(task.Receipts, proto.Clone(receipt).(*pb.TurnToolInvocation))
	}
	if proto.Size(task) > maxTaskCheckpoint {
		return status.Error(codes.ResourceExhausted, "task evidence exceeds bounded record size")
	}
	return nil
}

// TaskSessionRecords is a read-only projection of committed task evidence.
// There is no second write to lose or replay between completion and history.
func TaskSessionRecords(store *Store, channel, user string) ([]*pb.SessionRecord, error) {
	var out []*pb.SessionRecord
	err := store.ForEach(BucketTaskApprovals, func(_ string, raw []byte) error {
		task := new(pb.TaskApprovalRecord)
		if err := proto.Unmarshal(raw, task); err != nil {
			return err
		}
		if task.SessionId == "" || len(task.Transcript) == 0 || (channel != "" && channel != "bot") || (user != "" && user != task.Owner) {
			return nil
		}
		out = append(out, &pb.SessionRecord{Id: task.SessionId, Channel: "bot", ChannelId: strings.TrimPrefix(task.SessionId, "bot:"), UserId: task.Owner, CreatedAt: task.CreatedAt, UpdatedAt: task.CreatedAt, NextSeq: uint64(len(task.Transcript) + len(task.Receipts) + 1), FirstSeq: 1})
		return nil
	})
	return out, err
}

func TaskSessionMessages(store *Store, session string) ([]*pb.SessionMessage, error) {
	_, id, ok := strings.Cut(session, ".task.")
	if !ok {
		return nil, fmt.Errorf("invalid task session")
	}
	raw, err := store.Get(BucketTaskApprovals, id)
	if err != nil {
		return nil, err
	}
	task := new(pb.TaskApprovalRecord)
	if err := proto.Unmarshal(raw, task); err != nil {
		return nil, err
	}
	if task.SessionId != session {
		return nil, fmt.Errorf("task session mismatch")
	}
	messages := task.Transcript
	// Receipt rows are a UI history projection, never fed into agent context.
	for _, receipt := range task.Receipts {
		content, err := json.Marshal(map[string]any{"tool": receipt.ToolName, "call_id": receipt.CallId, "status": receipt.ExecutionStatus, "args": receipt.Args, "output": receipt.Output, "error": receipt.Error, "exit_code": receipt.ExitCode})
		if err != nil {
			return nil, err
		}
		messages = append(messages, &pb.SessionMessage{Role: "tool", Content: "Tool receipt: " + string(content), TurnId: task.TurnId})
	}
	for i, m := range messages {
		m.SessionId = session
		m.Seq = uint64(i + 1)
	}
	return messages, nil
}
