package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/internal/turn"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// taskEvidenceEvent preserves the generated JSON contract of durable task evidence.
type taskEvidenceEvent struct{ *pb.ConsoleBotReply }

type chatEmitter func(string, proto.Message)

func (s *Server) runBotChat(ctx context.Context, claims *types.Claims, botID, message string, attachments []types.Attachment, begin func() (chatEmitter, error)) error {
	if s.runner == nil {
		return status.Error(codes.Unavailable, "this node cannot run turns")
	}
	if strings.TrimSpace(message) == "" {
		return status.Error(codes.InvalidArgument, "message is required")
	}
	if _, err := s.consoleOperations().Bot(ctx, claims, botID); err != nil {
		return consoleRPCError(err)
	}
	turnID := ids.New()
	userID := ""
	if claims != nil {
		userID = claims.UserID
	}
	sessionRef := SessionRef{Channel: botChannel, ChannelID: botID, UserID: userID}
	lease, disposition := s.gate.acquire(ctx, cacheKey(sessionRef), turnID, message, len(attachments) > 0)
	if disposition == Folded {
		return errChatFolded
	}
	if disposition == Dropped {
		return status.Error(codes.Aborted, "a turn is already running for this bot")
	}
	defer lease.Release()
	if len(lease.Batch) > 1 {
		message = strings.Join(lease.Batch, "\n")
	}

	emit, err := begin()
	if err != nil {
		return err
	}
	emit("start", &pb.ConsoleProgress{Bot: botID, TurnId: turnID})

	// A heartbeat while the turn runs. The console shows it as
	// "working", and it also keeps an idle-timeout proxy from closing a
	// connection that is legitimately quiet for ninety seconds.
	done := make(chan struct{})
	exited := make(chan struct{})
	defer func() { close(done); <-exited }()
	go func() {
		defer close(exited)
		ticker := time.NewTicker(botChatHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				emit("working", &pb.ConsoleProgress{TurnId: turnID})
			}
		}
	}()

	// The bot's conversations live on their own synthetic channel, so
	// its working transcripts never appear in somebody's chat history.
	prior := s.conv.Load(ctx, sessionRef)

	req := turn.Request{
		Attachments: attachments,
		Message:     message,
		Claims:      claims,
		// The bot's principal, so its memory belongs to it; the claims
		// stay the operator's, so policy still answers to the person
		// who asked.
		Principal:           identity.Bot(botID),
		BotID:               botID,
		TurnID:              turnID,
		Channel:             botChannel,
		ChannelID:           botID,
		Caps:                s.cfg.DefaultBudget,
		ConversationHistory: prior.Messages,
		ConversationSummary: prior.Summary,
	}

	// Chat is conversational regardless of the recipient or whether teams are
	// enabled. Only an explicit /task command bypasses the conversational runner;
	// the model can separately choose task_create or delegate via inbox_post.
	taskBody, explicit, parseErr := botTaskCommand(message, s.cfg.StartBotTask != nil)
	if explicit {
		if parseErr != nil {
			emit("error", &pb.ConsoleFailure{Message: parseErr.Error()})
			return nil
		}
		req.Message = taskBody
		task, err := s.cfg.StartBotTask(ctx, req)
		if err != nil {
			emit("error", &pb.ConsoleFailure{Message: err.Error()})
			return nil
		}
		emit("reply", taskEvidenceEvent{taskChatReply(task, turnID)})
		return nil
	}
	resp, err := s.runner.Run(ctx, req)
	if err != nil {
		// The error goes down the STREAM, not as a status: the 200 and
		// the headers are already on the wire.
		emit("error", &pb.ConsoleFailure{Message: err.Error()})
		return nil
	}
	turnStart := resp.TurnStartIndex

	// Confirmation loop, as on /v1/messages: raise a prompt, push its
	// id down the wire for the console to render buttons against, and
	// block until the person on the other end of the stream answers or
	// the TTL fires.
	for resp.NeedsConfirmation && s.cfg.Prompts != nil {
		ttl := s.cfg.ConfirmationTTL
		if ttl <= 0 {
			ttl = 5 * time.Minute
		}
		p, perr := s.cfg.Prompts.Create(NewPrompt{
			TurnID:    turnID,
			Reason:    resp.ConfirmationReason,
			Channel:   botChannel,
			SessionID: botID,
			TTL:       ttl,
			Action:    resp.ConfirmationAction,
			Resource:  resp.ConfirmationResource,
			RaisedFor: userID,
		})
		if perr != nil {
			emit("error", &pb.ConsoleFailure{
				Message: "this turn needs your approval and the prompt could not be raised: " +
					resp.ConfirmationReason,
			})
			return nil
		}
		emit("needs_confirmation", &pb.ConsoleConfirmation{
			PromptId:         p.ID,
			Reason:           resp.ConfirmationReason,
			Action:           resp.ConfirmationAction,
			Resource:         resp.ConfirmationResource,
			ExpiresInSeconds: int32(ttl.Seconds()),
		})

		decision, werr := s.cfg.Prompts.Wait(ctx, p.ID)
		if werr != nil {
			emit("error", &pb.ConsoleFailure{
				Message: "the approval request was aborted: " + werr.Error(),
			})
			return nil
		}
		if decision != PromptApproved {
			emit("reply", &pb.ConsoleBotReply{
				Text:           fmt.Sprintf("Confirmation %s: %s", decision.String(), resp.ConfirmationReason),
				ToolsUsed:      invokedToolNames(resp.ToolCalls),
				ToolsAttempted: unconfirmedToolNames(resp.ToolCalls),
			})
			return nil
		}

		req.Spent = resp.BudgetState
		resumeCtx := turn.WithTurnApproval(ctx, resp.ConfirmationAction, resp.ConfirmationResource)
		resumed, rerr := s.runner.Resume(resumeCtx, req, resp.Messages)
		if rerr != nil {
			emit("error", &pb.ConsoleFailure{Message: rerr.Error()})
			return nil
		}
		resumed.ToolCalls = append(resp.ToolCalls, resumed.ToolCalls...)
		resp = resumed
	}

	// Persisted once, complete, after the confirmation loop so an
	// approved-and-resumed turn is not recorded twice in halves.
	if newTurn := newTurnMessages(resp.Messages, turnStart); len(newTurn) > 0 {
		s.conv.Append(ctx, sessionRef, turnID, newTurn)
	}

	// Still needing confirmation here means the ask itself could not be
	// raised — no prompt registry, or the loop was skipped. Say so
	// rather than sending an empty reply the console would render as a
	// silent turn.
	if resp.NeedsConfirmation {
		emit("error", &pb.ConsoleFailure{
			Message: "this turn needs your approval and no prompt registry is wired: " +
				resp.ConfirmationReason,
		})
		return nil
	}

	emit("reply", &pb.ConsoleBotReply{
		Text:   resp.Reply,
		TurnId: turnID,
		// Names, not a count. A count tells you a turn was busy; only
		// the names tell you whether the thing it SAYS it did is among
		// them — which is the check that catches a bot reporting work
		// it never performed.
		ToolsUsed:      invokedToolNames(resp.ToolCalls),
		ToolCalls:      int32(returnedToolCount(resp.ToolCalls)),
		ToolsAttempted: unconfirmedToolNames(resp.ToolCalls),
		TokensUsed:     0,
		CostUsd:        resp.BudgetState.SpendUSD,
		SessionId:      botChannel + ":" + botID,
	})
	return nil
}
