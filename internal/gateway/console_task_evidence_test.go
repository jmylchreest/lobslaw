package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/turn"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func consoleEvidenceTask() *pb.TaskApprovalRecord {
	return &pb.TaskApprovalRecord{Id: "task", Owner: "user:alice", Actor: "bot:chief", Revision: 9, State: pb.TaskApprovalState_TASK_APPROVAL_STATE_OUTCOME_UNKNOWN, Recoverable: true, CoordinatorConversation: true, SessionId: "bot:chief.task.task", Result: "resumed task result", Transcript: []*pb.SessionMessage{
		{Seq: 9007199254740993, Role: "user", Content: "coordinator question", Timestamp: timestamppb.New(time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC))},
		{Role: "assistant", ToolCalls: []*pb.SessionToolCall{{Id: "call", Name: "write_file", Arguments: `{"path":"workspace/note"}`}}},
		{Role: "tool", ToolCallId: "call", Content: "resumed output"},
		{Role: "assistant", Content: "resumed final reply"},
	}, Receipts: []*pb.TurnToolInvocation{
		{CallId: "call", ToolName: "write_file", ExecutionStatus: turn.ReceiptApprovalRequired, Error: "needs approval"},
		{CallId: "call", ToolName: "write_file", ExecutionStatus: turn.ReceiptExecuted, ExitCode: 1, Output: "failed after dispatch"},
		{CallId: "denied", ToolName: "delete_file", ExecutionStatus: turn.ReceiptRefused},
		{CallId: "unknown", ToolName: "shell_command"},
	}}
}

func TestTaskEvidenceJSONMatchesAcrossLocalAndRemoteChat(t *testing.T) {
	t.Parallel()
	for _, remote := range []bool{false, true} {
		name := "local"
		if remote {
			name = "remote"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			task := consoleEvidenceTask()
			task.State = pb.TaskApprovalState_TASK_APPROVAL_STATE_COMPLETED
			backend := startWebREST(t, &captureRunner{}, func(c *RESTConfig) {
				c.Bots = &memBots{recs: map[string]*pb.BotRecord{"chief": {Id: "chief", Owner: "user:alice", IsCoordinator: true}}}
				c.StartBotTask = func(context.Context, turn.Request) (*pb.TaskApprovalRecord, error) { return task, nil }
			})
			front := backend
			if remote {
				client := testConsoleClient(t, backend)
				front = startWebREST(t, nil, func(c *RESTConfig) { c.RemoteConsole = client })
			}
			response := doJSON(t, http.MethodPost, webBaseURL(front)+"/v1/bots/chief/messages", `{"message":"/task question"}`, http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}})
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			var reply *pb.ConsoleBotReply
			for _, frame := range strings.Split(string(body), "\n\n") {
				if !strings.HasPrefix(frame, "event: reply\n") {
					continue
				}
				raw := strings.TrimPrefix(frame, "event: reply\ndata: ")
				if !strings.Contains(raw, `"executionStatus"`) || !strings.Contains(raw, `"9007199254740993"`) || !strings.Contains(raw, `"toolCalls"`) {
					t.Fatalf("not generated evidence JSON: %s", raw)
				}
				reply = new(pb.ConsoleBotReply)
				if err := protojson.Unmarshal([]byte(raw), reply); err != nil {
					t.Fatal(err)
				}
			}
			if reply == nil {
				t.Fatalf("no reply: %s", body)
			}
			want := taskChatReply(task, reply.TurnId)
			if !proto.Equal(reply, want) {
				t.Fatalf("evidence changed across transport:\ngot %v\nwant %v", reply, want)
			}
			if reply.ToolCalls != 1 || len(reply.ToolsUsed) != 1 || len(reply.ToolsAttempted) != 3 {
				t.Fatalf("attempt counted as execution: %v", reply)
			}
		})
	}
}

type evidenceTaskAPI struct {
	TaskApprovalAPI
	mu     sync.Mutex
	record *pb.TaskApprovalRecord
	closes int
}

func (a *evidenceTaskAPI) GetTaskApproval(_ context.Context, q *pb.GetTaskApprovalRequest) (*pb.GetTaskApprovalResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if q.Owner != a.record.Owner || q.Id != a.record.Id {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	return &pb.GetTaskApprovalResponse{Record: proto.Clone(a.record).(*pb.TaskApprovalRecord)}, nil
}
func (a *evidenceTaskAPI) CancelTaskApproval(_ context.Context, q *pb.CancelTaskApprovalRequest) (*pb.CancelTaskApprovalResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if q.Owner != a.record.Owner || q.Id != a.record.Id {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	if q.Revision != a.record.Revision {
		return nil, status.Error(codes.Aborted, "task changed")
	}
	a.closes++
	a.record.Revision++
	a.record.State = pb.TaskApprovalState_TASK_APPROVAL_STATE_CANCELLED
	a.record.Recoverable = false
	return &pb.CancelTaskApprovalResponse{Record: proto.Clone(a.record).(*pb.TaskApprovalRecord)}, nil
}

func TestTaskRecoveryAndEvidenceSurviveTypedOwnerAPI(t *testing.T) {
	t.Parallel()
	api := &evidenceTaskAPI{record: consoleEvidenceTask()}
	backend := startWebREST(t, nil, func(c *RESTConfig) { c.TaskApprovals = api })
	client := testConsoleClient(t, backend)
	front := startWebREST(t, nil, func(c *RESTConfig) { c.RemoteConsole = client })
	auth := http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}}
	for _, server := range []*Server{backend, front} {
		response := doJSON(t, http.MethodGet, webBaseURL(server)+"/v1/task-approvals/task", "", auth)
		raw, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		out := new(pb.GetTaskApprovalResponse)
		if err := protojson.Unmarshal(raw, out); err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(out.Record, consoleEvidenceTask()) {
			t.Fatalf("task evidence/recovery changed: %s", raw)
		}
	}
	response := doJSON(t, http.MethodPost, webBaseURL(front)+"/v1/task-approvals/task/cancel", `{"revision":"9"}`, auth)
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := new(pb.CancelTaskApprovalResponse)
	if err := protojson.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
	if out.Record.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_CANCELLED || out.Record.Recoverable || len(out.Record.Transcript) != 4 || len(out.Record.Receipts) != 4 {
		t.Fatalf("closure lost evidence: %s", raw)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.closes != 1 {
		t.Fatalf("closures = %d", api.closes)
	}
}
