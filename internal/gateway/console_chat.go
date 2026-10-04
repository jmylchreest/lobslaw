package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/turn"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type chatResponder interface {
	Responder
	event(string, any) error
	Close()
}

func (s *Server) runConsoleChat(reqCtx context.Context, claims *types.Claims, req messageRequest, attachments []types.Attachment, begin func() chatResponder) (messageResponse, error) {
	if err := reqCtx.Err(); err != nil {
		return messageResponse{}, consoleRPCError(err)
	}
	if s.runner == nil {
		return messageResponse{}, status.Error(codes.Unavailable, "agent not configured on this node")
	}
	userID := ""
	if claims != nil {
		userID = claims.UserID
	}
	var sessionRef SessionRef
	var prior Transcript
	if req.SessionID != "" {
		if err := validateSessionID(req.SessionID); err != nil {
			return messageResponse{}, status.Error(codes.InvalidArgument, err.Error())
		}
		sessionRef = SessionRef{
			Channel:   "rest",
			ChannelID: restSessionID(userID, req.SessionID),
			UserID:    userID,
		}
		// Serialise turns on this session for the same reason the
		// Telegram path does: Load → run → Append is not atomic, and
		// each request is its own goroutine. Only sessioned requests
		// need it — a session-less call has no transcript to corrupt.
		lease, disposition := s.gate.acquire(reqCtx, cacheKey(sessionRef), req.TurnID, req.Message, len(attachments) > 0)
		switch disposition {
		case Folded:
			// Another in-flight turn absorbed this message and will
			// answer for it. Unlike a chat channel, an HTTP caller is
			// waiting on this response, so say so rather than hanging
			// up silently.
			return messageResponse{}, errChatFolded
		case Dropped:
			return messageResponse{}, status.Error(codes.Aborted, "a turn is already running for this session")
		}
		defer lease.Release()

		if len(lease.Batch) > 1 {
			req.Message = strings.Join(lease.Batch, "\n")
		}

		prior = s.conv.Load(reqCtx, sessionRef)
		s.log.Debug("rest: conversation history loaded",
			"turn_id", req.TurnID,
			"session_id", req.SessionID,
			"prior_messages", len(prior.Messages),
			"summarised", prior.Summary != "")
	}

	agentReq := turn.Request{
		Attachments:         attachments,
		Message:             req.Message,
		Claims:              claims,
		TurnID:              req.TurnID,
		Model:               req.Model,
		Caps:                s.cfg.DefaultBudget,
		ConversationHistory: prior.Messages,
		ConversationSummary: prior.Summary,
		Channel:             sessionRef.Channel,
		ChannelID:           sessionRef.ChannelID,
		BotID:               s.resolveTeamBot(reqCtx, sessionRef.Channel, sessionRef.ChannelID, userID),
	}

	// Responsiveness, shared with Telegram. The visible half needs an
	// SSE client; the hard timeout applies either way, and REST had
	// none — a stalled provider hung the request until the client gave
	// up.
	//
	// turnCtx, not reqCtx, for the rest of this handler: the
	// confirmation resume loop below re-enters the agent, and a turn
	// that stalls after approval should hit the same cap as one that
	// stalls before it.
	responder := begin()
	turnCtx, stopGuards := startResponsiveness(reqCtx, responder, ResponsivenessConfig{
		TypingInterval: s.cfg.TypingInterval,
		InterimTimeout: s.cfg.InterimTimeout,
		HardTimeout:    s.cfg.HardTimeout,
		Soul:           s.cfg.Soul,
	})
	defer stopGuards()

	resp, err := s.runner.Run(turnCtx, agentReq)
	if err != nil {
		s.log.Error("agent error", "turn_id", req.TurnID, "err", err)
		stopGuards()
		responder.Close()
		return messageResponse{}, consoleRPCError(err)
	}

	// Captured before the resume loop replaces resp: resumption only
	// ever appends, so the original boundary still marks the start of
	// the whole turn once it finishes.
	turnStart := resp.TurnStartIndex

	// Auto-resume loop: when the agent returns NeedsConfirmation AND
	// the registry is wired, register a prompt, long-poll Wait until
	// the user resolves it (or the TTL fires), then either resume the
	// turn with a lifted budget (Approved) or stop with a deny reply
	// (Denied / TimedOut). The resumed turn may itself hit a fresh
	// confirmation; we loop until the agent returns a plain reply or
	// the user refuses. A nil registry leaves resp unchanged — older
	// clients that don't understand prompt_id still see the reason in
	// the response body.
	var lastPromptID string
	for resp.NeedsConfirmation && s.cfg.Prompts != nil {
		ttl := s.cfg.ConfirmationTTL
		if ttl <= 0 {
			ttl = 5 * time.Minute
		}
		// The REST caller holds the connection open, so this handler
		// resumes the turn itself and does not need the continuation
		// carried. Action and resource still are: they are what a
		// "session" or "always" answer records a grant against.
		p, perr := s.cfg.Prompts.Create(NewPrompt{
			TurnID:    req.TurnID,
			Reason:    resp.ConfirmationReason,
			Channel:   "rest",
			SessionID: req.SessionID,
			TTL:       ttl,
			Action:    resp.ConfirmationAction,
			Resource:  resp.ConfirmationResource,
			RaisedFor: userID,
		})
		if perr != nil {
			s.log.Warn("rest: prompt registration failed — returning confirmation as-is", "err", perr)
			break
		}
		lastPromptID = p.ID
		if err := responder.event("needs_confirmation", &pb.ConsoleConfirmation{PromptId: p.ID, Reason: resp.ConfirmationReason, Action: resp.ConfirmationAction, Resource: resp.ConfirmationResource}); err != nil {
			s.log.Warn("rest: send confirmation", "err", err)
			break
		}

		decision, werr := s.cfg.Prompts.Wait(turnCtx, p.ID)
		if werr != nil {
			s.log.Warn("rest: prompt wait aborted",
				"prompt_id", p.ID, "turn_id", req.TurnID, "err", werr)
			break
		}
		if decision != PromptApproved {
			resp.Reply = fmt.Sprintf("Confirmation %s: %s",
				decision.String(), resp.ConfirmationReason)
			resp.NeedsConfirmation = false
			resp.ConfirmationReason = ""
			break
		}

		// Approved: lift caps and re-enter. resp.Messages carries the
		// conversation at the moment the original turn stopped.
		//
		// The turn approval is the policy half of the same idea: the
		// operation the user just answered for does not get asked again
		// inside the turn they answered it in.
		agentReq.Spent = resp.BudgetState
		resumeCtx := turn.WithTurnApproval(turnCtx, resp.ConfirmationAction, resp.ConfirmationResource)
		resumed, rerr := s.runner.Resume(resumeCtx, agentReq, resp.Messages)
		if rerr != nil {
			s.log.Error("rest: resume after approval failed",
				"turn_id", req.TurnID, "err", rerr)
			stopGuards()
			responder.Close()
			return messageResponse{}, consoleRPCError(rerr)
		}
		// Preserve prior tool calls in the cumulative response.
		resumed.ToolCalls = append(resp.ToolCalls, resumed.ToolCalls...)
		resp = resumed
	}

	// Persisted after the confirmation loop so an approved-and-resumed
	// turn is recorded once, complete, rather than twice in halves.
	if req.SessionID != "" {
		if newTurn := newTurnMessages(resp.Messages, turnStart); len(newTurn) > 0 {
			s.conv.Append(reqCtx, sessionRef, req.TurnID, newTurn)
		}
	}

	out := messageResponse{
		// Appended after the transcript was persisted above, and to
		// the outbound text only. A notice recorded as an assistant
		// message is one the model reads next turn and reasons about.
		Reply: s.cfg.Notices.Append(reqCtx, "rest",
			sessionRef.ChannelID, noticeSubject(claims), resp.Reply),
		NeedsConfirmation:  resp.NeedsConfirmation,
		ConfirmationReason: resp.ConfirmationReason,
		PromptID:           lastPromptID,
		TurnID:             req.TurnID,
		Budget: budgetStateJSON{
			ToolCalls:   resp.BudgetState.ToolCalls,
			SpendUSD:    resp.BudgetState.SpendUSD,
			EgressBytes: resp.BudgetState.EgressBytes,
		},
	}
	for _, tc := range resp.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, toolCallJSON{
			ExecutionStatus: tc.ExecutionStatus,
			CallID:          tc.CallID,
			ToolName:        tc.ToolName,
			Args:            tc.Args,
			Output:          tc.Output,
			ExitCode:        tc.ExitCode,
			Error:           tc.Error,
		})
	}

	// Timers off and the responder closed BEFORE the body is written,
	// so a typing tick that fires at exactly the wrong moment cannot
	// interleave itself into the response.
	stopGuards()
	responder.Close()

	return out, nil
}
