package gateway

import (
	"context"
	"errors"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/bots"
	"github.com/jmylchreest/lobslaw/internal/console"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// These projections are deliberate public allowlists, independent of storage records.
func protoBotView(v console.BotView) *pb.ConsoleBot {
	return &pb.ConsoleBot{Id: v.ID, DisplayName: v.DisplayName, Description: v.Description, Instructions: v.Instructions, IsCoordinator: v.IsCoordinator, GroupId: v.GroupID, Enabled: v.Enabled, Tools: v.Tools, MayMessage: v.MayMessage, Revision: v.Revision, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt}
}
func protoGroupView(v console.GroupView) *pb.ConsoleGroup {
	return &pb.ConsoleGroup{Id: v.ID, Name: v.Name, Description: v.Description, CoordinatorBotId: v.Coordinator, IsDefault: v.IsDefault, Owner: v.Owner, Mine: v.Mine, Revision: v.Revision, Bots: int32(v.Bots)}
}
func protoInboxItemView(v console.InboxItemView) *pb.ConsoleInboxItem {
	return &pb.ConsoleInboxItem{Revision: v.Revision, TruncatedFields: v.TruncatedFields, DetailPath: v.DetailPath, TaskId: v.TaskID, Id: v.ID, Recipient: v.Recipient, Sender: v.Sender, Kind: v.Kind, Subject: v.Subject, Body: v.Body, Priority: v.Priority, Status: v.Status, Result: v.Result, Error: v.Error, Attempts: v.Attempts, CorrelationId: v.CorrelationID, SessionId: v.SessionID, RequestedBy: v.RequestedBy, ToolsUsed: v.ToolsUsed, TokensUsed: v.TokensUsed, CostUsd: v.CostUSD, CreatedAt: v.CreatedAt, CompletedAt: v.CompletedAt}
}
func protoSessionView(v console.SessionView) *pb.ConsoleSession {
	return &pb.ConsoleSession{Id: v.ID, Channel: v.Channel, ChannelId: v.ChannelID, Title: v.Title, UserId: v.UserID, Messages: v.Messages, UpdatedAt: v.UpdatedAt}
}
func protoMessageView(v console.MessageView) *pb.ConsoleMessage {
	return &pb.ConsoleMessage{Seq: v.Seq, Role: v.Role, Content: v.Content, ToolCalls: int32(v.ToolCalls), TurnId: v.TurnID}
}
func protoRoutineView(v console.RoutineView) *pb.ConsoleRoutine {
	return &pb.ConsoleRoutine{Id: v.ID, Name: v.Name, Schedule: v.Schedule, HandlerRef: v.Handler, Enabled: v.Enabled, LastRun: v.LastRun, NextRun: v.NextRun, Prompt: v.Prompt}
}
func protoMemoryRecordView(v console.MemoryRecordView) *pb.ConsoleMemoryRecord {
	return &pb.ConsoleMemoryRecord{Id: v.ID, Kind: v.Kind, Text: v.Text, Tags: v.Tags, Scope: v.Scope, CreatedAt: v.CreatedAt}
}
func protoPromptView(v console.PromptView) *pb.ConsolePrompt {
	return &pb.ConsolePrompt{Id: v.ID, TurnId: v.TurnID, Reason: v.Reason, Channel: v.Channel, Decision: v.Decision, CreatedAt: v.CreatedAt.Format(time.RFC3339Nano), ExpiresAt: v.ExpiresAt.Format(time.RFC3339Nano)}
}
func protoToolInfo(v console.ToolInfo) *pb.ConsoleTool {
	return &pb.ConsoleTool{Name: v.Name, Description: v.Description}
}
func protoCommitmentView(v console.CommitmentView) *pb.ConsoleCommitment {
	return &pb.ConsoleCommitment{Id: v.ID, DueAt: v.DueAt.Format(time.RFC3339Nano), Reason: v.Reason, Status: v.Status}
}
func protoScheduledTaskView(v console.ScheduledTaskView) *pb.ConsoleScheduledTask {
	return &pb.ConsoleScheduledTask{Id: v.ID, Name: v.Name, Schedule: v.Schedule, HandlerRef: v.HandlerRef, NextRun: v.NextRun.Format(time.RFC3339Nano)}
}

func protoRows[A, B any](rows []A, convert func(A) B) []B {
	out := make([]B, 0, len(rows))
	for _, row := range rows {
		out = append(out, convert(row))
	}
	return out
}

func consoleRPCError(err error) error {
	if err == nil {
		return nil
	}
	code := codes.Internal
	switch {
	case errors.Is(err, context.Canceled):
		code = codes.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		code = codes.DeadlineExceeded
	case errors.Is(err, console.ErrUnauthenticated):
		code = codes.Unauthenticated
	case errors.Is(err, console.ErrForbidden):
		code = codes.PermissionDenied
	case errors.Is(err, console.ErrUnavailable):
		code = codes.Unavailable
	case errors.Is(err, console.ErrInvalid):
		code = codes.InvalidArgument
	case errors.Is(err, console.ErrConflict), errors.Is(err, console.ErrPromptExpired), errors.Is(err, console.ErrPromptResolved):
		code = codes.Aborted
	case errors.Is(err, bots.ErrNotFound), errors.Is(err, bots.ErrInboxNotFound), errors.Is(err, console.ErrPromptNotFound), strings.HasPrefix(err.Error(), "groups: not found"):
		code = codes.NotFound
	case errors.Is(err, bots.ErrInboxFull):
		code = codes.ResourceExhausted
	case strings.HasPrefix(err.Error(), "bots:"):
		code = codes.InvalidArgument
	case strings.HasPrefix(err.Error(), "groups:") && (strings.Contains(err.Error(), "cannot be deleted") || strings.Contains(err.Error(), "is required") || strings.Contains(err.Error(), "must be lowercase")):
		code = codes.InvalidArgument
	default:
		if _, ok := status.FromError(err); ok {
			return err
		}
	}
	return status.Error(code, err.Error())
}

func (s *Server) queryConsoleOperations(ctx context.Context, in *pb.QueryConsoleRequest) (*pb.QueryConsoleResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, consoleRPCError(err)
	}
	claims, _ := ctx.Value(forwardedConsoleIdentity{}).(*types.Claims)
	if err := validateConsoleTargets(in); err != nil {
		return nil, err
	}
	ops := s.consoleOperations()
	out := new(pb.QueryConsoleResponse)
	var err error
	switch q := in.Query.(type) {
	case *pb.QueryConsoleRequest_Capabilities:
		c := s.localCapabilities(ctx)
		out.Result = &pb.QueryConsoleResponse_Capabilities{Capabilities: &pb.ConsoleCapabilities{Compute: protoCapability(c.Compute), ComputeTeams: protoCapability(c.ComputeTeams), UiWeb: protoCapability(c.UIWeb)}}
	case *pb.QueryConsoleRequest_Tools:
		v, e := ops.Tools(ctx, claims)
		err = e
		out.Result = &pb.QueryConsoleResponse_Tools{Tools: &pb.ConsoleTools{Tools: protoRows(v, protoToolInfo)}}
	case *pb.QueryConsoleRequest_Bots:
		v, e := ops.Bots(ctx, claims)
		err = e
		out.Result = &pb.QueryConsoleResponse_Bots{Bots: &pb.ConsoleBots{Bots: protoRows(v, protoBotView)}}
	case *pb.QueryConsoleRequest_Bot:
		v, e := ops.Bot(ctx, claims, q.Bot.GetId())
		err = e
		out.Result = &pb.QueryConsoleResponse_Bot{Bot: protoBotView(v)}
	case *pb.QueryConsoleRequest_Groups:
		v, e := ops.Groups(ctx, claims)
		err = e
		out.Result = &pb.QueryConsoleResponse_Groups{Groups: &pb.ConsoleGroups{Groups: protoRows(v, protoGroupView)}}
	case *pb.QueryConsoleRequest_Group:
		v, e := ops.Group(ctx, claims, q.Group.GetId())
		err = e
		out.Result = &pb.QueryConsoleResponse_Group{Group: protoGroupView(v)}
	case *pb.QueryConsoleRequest_Inbox:
		v, e := ops.Inbox(ctx, claims, q.Inbox.GetId(), q.Inbox.GetStatus(), int(q.Inbox.GetLimit()))
		err = e
		out.Result = &pb.QueryConsoleResponse_Inbox{Inbox: &pb.ConsoleInbox{Bot: q.Inbox.GetId(), Items: protoRows(v, protoInboxItemView)}}
	case *pb.QueryConsoleRequest_InboxItem:
		v, e := ops.InboxItem(ctx, claims, q.InboxItem.GetBot(), q.InboxItem.GetId())
		err = e
		out.Result = &pb.QueryConsoleResponse_InboxItem{InboxItem: protoInboxItemView(v)}
	case *pb.QueryConsoleRequest_Activity:
		v, e := ops.Activity(ctx, claims, int(q.Activity.GetLimit()))
		err = e
		out.Result = &pb.QueryConsoleResponse_Inbox{Inbox: &pb.ConsoleInbox{Items: protoRows(v, protoInboxItemView)}}
	case *pb.QueryConsoleRequest_Sessions:
		v, e := ops.Sessions(ctx, claims, q.Sessions.GetId())
		err = e
		out.Result = &pb.QueryConsoleResponse_Sessions{Sessions: &pb.ConsoleSessions{Bot: q.Sessions.GetId(), Sessions: protoRows(v, protoSessionView)}}
	case *pb.QueryConsoleRequest_Transcript:
		v, e := ops.Transcript(ctx, claims, q.Transcript.GetId())
		err = e
		out.Result = &pb.QueryConsoleResponse_Transcript{Transcript: &pb.ConsoleTranscript{Id: q.Transcript.GetId(), Messages: protoRows(v, protoMessageView)}}
	case *pb.QueryConsoleRequest_Routines:
		v, e := ops.Routines(ctx, claims, q.Routines.GetId())
		err = e
		out.Result = &pb.QueryConsoleResponse_Routines{Routines: &pb.ConsoleRoutines{Routines: protoRows(v, protoRoutineView)}}
	case *pb.QueryConsoleRequest_Prompt:
		v, e := ops.Prompt(ctx, claims, q.Prompt.GetId())
		err = e
		out.Result = &pb.QueryConsoleResponse_Prompt{Prompt: protoPromptView(v)}

	case *pb.QueryConsoleRequest_Memory:
		v, total, e := ops.Memory(ctx, claims, q.Memory.GetId())
		err = e
		out.Result = &pb.QueryConsoleResponse_Memory{Memory: &pb.ConsoleMemory{Records: protoRows(v, protoMemoryRecordView), Total: int32(total)}}
	case *pb.QueryConsoleRequest_PlanWindow:
		v, e := ops.Plan(ctx, claims, q.PlanWindow)
		err = e
		out.Result = &pb.QueryConsoleResponse_Plan{Plan: &pb.ConsolePlan{WindowSeconds: v.WindowSeconds, Commitments: protoRows(v.Commitments, protoCommitmentView), ScheduledTasks: protoRows(v.ScheduledTasks, protoScheduledTaskView)}}
	default:
		return nil, status.Error(codes.InvalidArgument, "console query required")
	}
	if err != nil {
		return nil, consoleRPCError(err)
	}
	return out, nil
}
func protoCapability(v capabilityFlags) *pb.ConsoleCapability {
	return &pb.ConsoleCapability{Enabled: v.Enabled, Authorised: v.Authorised, Configured: v.Configured, Available: v.Available}
}

func consoleBotPatch(v *pb.ConsoleBotPatch) console.BotPatch {
	p := console.BotPatch{Revision: v.Revision, DisplayName: v.DisplayName, Description: v.Description, Instructions: v.Instructions, Enabled: v.Enabled, GroupID: v.GroupId}
	if v.Tools != nil {
		p.Tools = &v.Tools.Values
	}
	if v.MayMessage != nil {
		p.MayMessage = &v.MayMessage.Values
	}
	return p
}
func consoleGroupWrite(v *pb.ConsoleGroupWrite) console.GroupInput {
	return console.GroupInput{ID: v.GetId(), Name: v.GetName(), Description: v.GetDescription(), Coordinator: v.GetCoordinatorBotId(), Revision: v.Revision}
}
func (s *Server) mutateConsoleOperations(ctx context.Context, in *pb.MutateConsoleRequest) (*pb.MutateConsoleResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, consoleRPCError(err)
	}
	claims, _ := ctx.Value(forwardedConsoleIdentity{}).(*types.Claims)
	if err := validateConsoleTargets(in); err != nil {
		return nil, err
	}
	ops := s.consoleOperations()
	out := new(pb.MutateConsoleResponse)
	var err error
	switch q := in.Operation.(type) {
	case *pb.MutateConsoleRequest_CreateBot:
		b := q.CreateBot
		v, e := ops.CreateBot(ctx, claims, console.BotView{ID: b.GetId(), DisplayName: b.GetDisplayName(), Description: b.GetDescription(), Instructions: b.GetInstructions(), Tools: b.GetTools(), MayMessage: b.GetMayMessage(), GroupID: b.GetGroupId()})
		err = e
		out.Result = &pb.MutateConsoleResponse_Bot{Bot: protoBotView(v)}
	case *pb.MutateConsoleRequest_UpdateBot:
		if q.UpdateBot == nil {
			return nil, status.Error(codes.InvalidArgument, "bot patch required")
		}
		v, e := ops.UpdateBot(ctx, claims, q.UpdateBot.GetId(), consoleBotPatch(q.UpdateBot))
		err = e
		out.Result = &pb.MutateConsoleResponse_Bot{Bot: protoBotView(v)}
	case *pb.MutateConsoleRequest_DeleteBot:
		err = ops.DeleteBot(ctx, claims, q.DeleteBot.GetId())
		out.Result = &pb.MutateConsoleResponse_Deleted{Deleted: &pb.ConsoleEmpty{}}
	case *pb.MutateConsoleRequest_CreateGroup:
		if q.CreateGroup == nil {
			return nil, status.Error(codes.InvalidArgument, "group required")
		}
		v, e := ops.CreateGroup(ctx, claims, consoleGroupWrite(q.CreateGroup))
		err = e
		out.Result = &pb.MutateConsoleResponse_Group{Group: protoGroupView(v)}
	case *pb.MutateConsoleRequest_UpdateGroup:
		if q.UpdateGroup == nil {
			return nil, status.Error(codes.InvalidArgument, "group required")
		}
		v, e := ops.UpdateGroup(ctx, claims, q.UpdateGroup.GetId(), consoleGroupWrite(q.UpdateGroup))
		err = e
		out.Result = &pb.MutateConsoleResponse_Group{Group: protoGroupView(v)}
	case *pb.MutateConsoleRequest_DeleteGroup:
		err = ops.DeleteGroup(ctx, claims, q.DeleteGroup.GetId())
		out.Result = &pb.MutateConsoleResponse_Deleted{Deleted: &pb.ConsoleEmpty{}}
	case *pb.MutateConsoleRequest_PostInbox:
		b := q.PostInbox
		v, e := ops.PostInbox(ctx, claims, b.GetBot(), console.InboxInput{Subject: b.GetSubject(), Body: b.GetBody(), Kind: b.GetKind(), Priority: b.GetPriority()})
		err = e
		out.Result = &pb.MutateConsoleResponse_InboxItem{InboxItem: protoInboxItemView(v)}
	case *pb.MutateConsoleRequest_RetryInbox:
		b := q.RetryInbox
		v, e := ops.ChangeInbox(ctx, claims, b.GetBot(), b.GetId(), "retry")
		err = e
		out.Result = &pb.MutateConsoleResponse_InboxItem{InboxItem: protoInboxItemView(v)}
	case *pb.MutateConsoleRequest_CancelInbox:
		b := q.CancelInbox
		v, e := ops.ChangeInbox(ctx, claims, b.GetBot(), b.GetId(), "cancel")
		err = e
		out.Result = &pb.MutateConsoleResponse_InboxItem{InboxItem: protoInboxItemView(v)}
	case *pb.MutateConsoleRequest_ResolvePrompt:
		b := q.ResolvePrompt
		decision, scope, e := ops.ResolvePrompt(ctx, claims, b.GetId(), b.GetApprove(), b.GetScope())
		err = e
		out.Result = &pb.MutateConsoleResponse_Decision{Decision: &pb.ConsoleDecision{Decision: decision, Scope: scope}}
	default:
		return nil, status.Error(codes.InvalidArgument, "console mutation required")
	}
	if err != nil {
		return nil, consoleRPCError(err)
	}
	return out, nil
}

// IDs remain data, never route fragments. Validate the public identifier contract
// before calling operations, even though the backend no longer builds HTTP paths.
func validateConsoleTargets(in proto.Message) error {
	if proto.Size(in) > int(consoleBodyLimit) {
		return status.Error(codes.ResourceExhausted, "console request too large")
	}
	m := in.ProtoReflect()
	var invalid bool
	m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if f.ContainingOneof() == nil || f.Kind() != protoreflect.MessageKind {
			return true
		}
		nested := v.Message()
		for _, name := range []protoreflect.Name{"id", "bot"} {
			field := nested.Descriptor().Fields().ByName(name)
			if field == nil {
				continue
			}
			value := nested.Get(field).String()
			required := f.Name() != "create_bot" && f.Name() != "create_group" && f.Name() != "activity"
			if (required && value == "") || strings.ContainsAny(value, "/\\?#%") || value == "." || value == ".." {
				invalid = true
			}
		}
		return !invalid
	})
	if invalid {
		return status.Error(codes.InvalidArgument, "invalid console identifier")
	}
	return nil
}
