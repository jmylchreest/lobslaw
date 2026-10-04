package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/ids"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

var errChatFolded = errors.New("message folded into an in-flight turn; its reply covers this message")

func (s *Server) chatHTTPError(w http.ResponseWriter, err error) {
	if errors.Is(err, errChatFolded) {
		s.jsonErr(w, http.StatusAccepted, err.Error())
		return
	}
	consoleHTTPError(s, w, consoleRPCError(err))
}

// ChatConsole runs the same conversation lifecycle as REST with a typed event sink.
func (s *Server) ChatConsole(in *pb.ChatConsoleRequest, stream grpc.ServerStreamingServer[pb.ChatConsoleResponse]) error {
	ctx, err := s.consoleContext(stream.Context(), in.GetIdentity())
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return consoleRPCError(err)
	}
	if strings.TrimSpace(in.GetMessage()) == "" {
		return status.Error(codes.InvalidArgument, "message is required")
	}
	if proto.Size(in) > int(consoleBodyLimit) {
		return status.Error(codes.ResourceExhausted, "console request too large")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	claims, _ := ctx.Value(forwardedConsoleIdentity{}).(*types.Claims)
	sink := &consoleEvents{stream: stream, cancel: cancel}
	if in.GetBot() != "" {
		err = s.runBotChat(ctx, claims, in.Bot, in.Message, func() (chatEmitter, error) {
			return func(name string, payload proto.Message) { _ = sink.event(name, payload) }, nil
		})
	} else {
		req := messageRequest{Message: in.Message, SessionID: in.SessionId, TurnID: in.TurnId, Model: in.Model}
		if req.TurnID == "" {
			req.TurnID = "rest-" + ids.New()
		}
		var out messageResponse
		out, err = s.runConsoleChat(ctx, claims, req, nil, func() chatResponder { return sink })
		if err == nil {
			err = sink.send("final", protoMessageResponse(out))
		}
	}
	if errors.Is(err, errChatFolded) {
		return sink.send("accepted", &pb.ConsoleFailure{Message: err.Error()})
	}
	if err != nil {
		return consoleRPCError(err)
	}
	return sink.sendErr
}

func protoMessageResponse(v messageResponse) *pb.ConsoleReply {
	out := &pb.ConsoleReply{Reply: v.Reply, TurnId: v.TurnID, NeedsConfirmation: v.NeedsConfirmation, ConfirmationReason: v.ConfirmationReason, PromptId: v.PromptID, Budget: &pb.ConsoleBudget{ToolCalls: int32(v.Budget.ToolCalls), SpendUsd: v.Budget.SpendUSD, EgressBytes: v.Budget.EgressBytes}}
	for _, c := range v.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, &pb.ConsoleToolCall{ExecutionStatus: c.ExecutionStatus, CallId: c.CallID, ToolName: c.ToolName, Args: c.Args, Output: c.Output, ExitCode: int32(c.ExitCode), Error: c.Error})
	}
	return out
}

// A single lock orders heartbeat and turn events; a failed send cancels the runner.
type consoleEvents struct {
	mu      sync.Mutex
	stream  grpc.ServerStreamingServer[pb.ChatConsoleResponse]
	sendErr error
	cancel  context.CancelFunc
	closed  bool
}

func (w *consoleEvents) Typing(context.Context) error {
	return w.event("typing", &pb.ConsoleProgress{})
}
func (w *consoleEvents) Interim(_ context.Context, text string) error {
	return w.event("interim", &pb.ConsoleProgress{Text: text})
}
func (w *consoleEvents) Final(_ context.Context, text string) error {
	return w.event("final", &pb.ConsoleReply{Reply: text})
}
func (w *consoleEvents) Close() { w.mu.Lock(); defer w.mu.Unlock(); w.closed = true }
func (w *consoleEvents) event(name string, payload any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	return w.sendLocked(name, payload)
}
func (w *consoleEvents) send(name string, payload proto.Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sendLocked(name, payload)
}
func (w *consoleEvents) sendLocked(name string, payload any) error {
	if w.sendErr != nil {
		return w.sendErr
	}
	out := new(pb.ChatConsoleResponse)
	switch p := payload.(type) {
	case *pb.ConsoleProgress:
		switch name {
		case "start":
			out.Event = &pb.ChatConsoleResponse_Start{Start: p}
		case "working":
			out.Event = &pb.ChatConsoleResponse_Working{Working: p}
		case "typing":
			out.Event = &pb.ChatConsoleResponse_Typing{Typing: p}
		case "interim":
			out.Event = &pb.ChatConsoleResponse_Interim{Interim: p}
		}
	case *pb.ConsoleReply:
		out.Event = &pb.ChatConsoleResponse_Final{Final: p}
	case taskEvidenceEvent:
		out.Event = &pb.ChatConsoleResponse_Reply{Reply: p.ConsoleBotReply}
	case *pb.ConsoleBotReply:
		out.Event = &pb.ChatConsoleResponse_Reply{Reply: p}
	case *pb.ConsoleConfirmation:
		out.Event = &pb.ChatConsoleResponse_NeedsConfirmation{NeedsConfirmation: p}
	case *pb.ConsoleFailure:
		if name == "accepted" {
			out.Event = &pb.ChatConsoleResponse_Accepted{Accepted: p}
		} else {
			out.Event = &pb.ChatConsoleResponse_Error{Error: p}
		}
	}
	if out.Event == nil {
		w.sendErr = status.Error(codes.Internal, "unsupported console event")
	} else {
		w.sendErr = w.stream.Send(out)
	}
	if w.sendErr != nil {
		w.cancel()
	}
	return w.sendErr
}
