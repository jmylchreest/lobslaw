package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

var chatRequestID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,79}$`)

func (s *Server) handleChatTurns(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	authn, err := s.authenticateRequest(r)
	if err != nil || !authenticatedUploadClaims(authn.Claims) {
		s.jsonErr(w, http.StatusUnauthorized, "sign in to resume chat")
		return
	}
	if err := s.checkCookieCSRF(r, authn); err != nil {
		s.jsonErr(w, http.StatusForbidden, err.Error())
		return
	}
	owner := authn.Claims.UserID
	id := strings.TrimPrefix(r.URL.Path, "/v1/chat-turns")
	if id != "" {
		if !strings.HasPrefix(id, "/") || !chatRequestID.MatchString(id[1:]) {
			s.jsonErr(w, http.StatusNotFound, "turn not found")
			return
		}
		id = id[1:]
		turn := s.chatTurns.get(owner, id)
		if turn == nil {
			s.jsonErr(w, http.StatusNotFound, "turn not found")
			return
		}
		if !s.chatTurnVisible(r.Context(), authn.Claims, turn.Bot, w) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			respondJSON(w, http.StatusOK, turn)
		case http.MethodDelete:
			respondJSON(w, http.StatusOK, s.chatTurns.stop(owner, id))
		default:
			s.jsonErr(w, http.StatusMethodNotAllowed, "GET or DELETE required")
		}
		return
	}
	if r.Method == http.MethodGet {
		bot, session := r.URL.Query().Get("bot"), r.URL.Query().Get("session_id")
		if !s.chatTurnVisible(r.Context(), authn.Claims, bot, w) {
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"turn": s.chatTurns.latest(owner, bot, session)})
		return
	}
	if r.Method != http.MethodPost {
		s.jsonErr(w, http.StatusMethodNotAllowed, "GET or POST required")
		return
	}
	s.createRetainedChatTurn(w, r, authn)
}

func (s *Server) createRetainedChatTurn(w http.ResponseWriter, r *http.Request, authn requestAuth) {
	owner := authn.Claims.UserID
	var request chatTurnRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, restMessageBodyLimit)).Decode(&request) != nil || !chatRequestID.MatchString(request.ID) || (strings.TrimSpace(request.Message) == "" && len(request.UploadIDs) == 0) || len(request.Message) > chatTurnMaxBytes/2 || len(request.UploadIDs) > restMessageMaxUploads {
		s.jsonErr(w, http.StatusBadRequest, "request id and a bounded message are required")
		return
	}
	if request.Bot == "" {
		if request.SessionID == "" {
			request.SessionID = "console"
		}
		if err := validateSessionID(request.SessionID); err != nil {
			s.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
	} else if request.SessionID != "" {
		s.jsonErr(w, http.StatusBadRequest, "bot conversations do not take a session id")
		return
	}
	if !s.chatTurnVisible(r.Context(), authn.Claims, request.Bot, w) {
		return
	}
	if s.runner == nil && s.cfg.RemoteConsole == nil {
		s.jsonErr(w, http.StatusServiceUnavailable, "chat unavailable")
		return
	}
	// Replays can outlive temporary uploads. Recover admitted work before pinning
	// files again; never execute another copy after an upload expires.
	if existing := s.chatTurns.get(owner, request.ID); existing != nil {
		if !sameChatRequest(existing.chatTurnRequest, request) {
			s.jsonErr(w, http.StatusConflict, "request id already used for a different message")
			return
		}
		respondJSON(w, http.StatusAccepted, existing)
		return
	}
	if len(request.UploadIDs) > 0 && s.cfg.RemoteConsole != nil {
		s.jsonErr(w, http.StatusServiceUnavailable, "uploads are unavailable on remote console gateways")
		return
	}
	attachments, releaseUploads, err := s.uploads.acquire(owner, request.UploadIDs, time.Now())
	if err != nil {
		s.jsonErr(w, http.StatusNotFound, errUploadUnavailable.Error())
		return
	}
	files := make([]chatFile, 0, len(attachments))
	for _, attachment := range attachments {
		files = append(files, chatFile{Name: attachment.Filename, MIME: attachment.MimeType, Size: attachment.Size})
	}
	ctx, timeout := context.WithTimeout(s.chatTurns.ctx, chatTurnTimeout)
	expiry := authn.Claims.ExpiresAt
	if authn.LoginID != "" {
		if login := s.logins.get(authn.LoginID); login != nil {
			expiry = login.ExpiresAt
		} else {
			expiry = time.Now()
		}
	}
	if !expiry.IsZero() {
		var deadline context.CancelFunc
		ctx, deadline = context.WithDeadline(ctx, expiry)
		old := timeout
		timeout = func() { deadline(); old() }
	}
	ctx, untrack := s.bindStream(ctx, authn.LoginID)
	stop := func() { untrack(); timeout() }
	turn, created, err := s.chatTurns.create(owner, request, stop, files...)
	if err != nil {
		stop()
		releaseUploads()
		s.jsonErr(w, http.StatusConflict, err.Error())
		return
	}
	if !created {
		stop()
		releaseUploads()
	} else {
		go s.executeChatTurn(ctx, authn.Claims, request, attachments, releaseUploads, stop)
	}
	respondJSON(w, http.StatusAccepted, turn)
}

func (s *Server) chatTurnVisible(ctx context.Context, claims *types.Claims, bot string, w http.ResponseWriter) bool {
	if bot == "" {
		return true
	}
	if s.cfg.RemoteConsole != nil {
		probe, cancel := context.WithTimeout(ctx, consoleProbeTimeout)
		defer cancel()
		_, err := s.cfg.RemoteConsole.QueryConsole(probe, &pb.QueryConsoleRequest{Identity: consoleIdentity(claims), Query: &pb.QueryConsoleRequest_Bot{Bot: &pb.ConsoleTarget{Id: bot}}})
		if err != nil {
			consoleHTTPError(s, w, err)
			return false
		}
		return true
	}
	if _, err := s.consoleOperations().Bot(ctx, claims, bot); err != nil {
		consoleHTTPError(s, w, consoleRPCError(err))
		return false
	}
	return true
}

func (s *Server) executeChatTurn(ctx context.Context, claims *types.Claims, request chatTurnRequest, attachments []types.Attachment, releaseUploads func(), stop context.CancelFunc) {
	defer s.chatTurns.wg.Done()
	defer stop()
	defer releaseUploads()
	if strings.TrimSpace(request.Message) == "" {
		request.Message = "Please examine the attached files."
	}
	emit := func(event string, payload proto.Message) {
		if ctx.Err() != nil {
			return
		}
		if task, ok := payload.(taskEvidenceEvent); ok {
			payload = task.ConsoleBotReply
		}
		raw, err := protojson.Marshal(payload)
		if err == nil {
			err = s.chatTurns.event(claims.UserID, request.ID, event, raw)
		}
		if err != nil {
			_ = s.chatTurns.event(claims.UserID, request.ID, "error", chatErrorData("Cannot retain the reply. Inspect the recorded conversation before sending again."))
			stop()
		}
	}
	var err error
	switch {
	case s.cfg.RemoteConsole != nil:
		var stream pb.ConsoleService_ChatConsoleClient
		stream, err = s.cfg.RemoteConsole.ChatConsole(ctx, &pb.ChatConsoleRequest{Identity: consoleIdentity(claims), Bot: request.Bot, Message: request.Message, SessionId: request.SessionID, TurnId: request.ID})
		if err == nil {
			for {
				var event *pb.ChatConsoleResponse
				event, err = stream.Recv()
				if errors.Is(err, io.EOF) {
					err = nil
					break
				}
				if err != nil {
					break
				}
				message := event.ProtoReflect()
				field := message.WhichOneof(message.Descriptor().Oneofs().ByName("event"))
				if field != nil {
					emit(string(field.Name()), message.Get(field).Message().Interface())
				}
			}
		}
	case request.Bot != "":
		err = s.runBotChat(ctx, claims, request.Bot, request.Message, attachments, func() (chatEmitter, error) { return emit, nil })
	default:
		var result messageResponse
		result, err = s.runConsoleChat(ctx, claims, messageRequest{Message: request.Message, SessionID: request.SessionID, TurnID: request.ID}, attachments, func() chatResponder { return &retainedResponder{emit: emit} })
		if err == nil {
			emit("final", protoMessageResponse(result))
		}
	}
	if errors.Is(err, errChatFolded) {
		emit("accepted", &pb.ConsoleFailure{Message: err.Error()})
		return
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	turn := s.chatTurns.get(claims.UserID, request.ID)
	if turn != nil && activeChatTurn(turn.State) {
		message := "The reply ended without a result. Review the conversation before sending again."
		if err != nil {
			message = "The reply was interrupted: " + err.Error()
		}
		if saveErr := s.chatTurns.event(claims.UserID, request.ID, "error", chatErrorData(message)); saveErr != nil {
			s.log.Error("chat journal final write failed", "err", saveErr)
		}
	}
}

type retainedResponder struct{ emit chatEmitter }

func (r *retainedResponder) Typing(context.Context) error {
	r.emit("typing", &pb.ConsoleProgress{})
	return nil
}
func (r *retainedResponder) Interim(_ context.Context, text string) error {
	r.emit("interim", &pb.ConsoleProgress{Text: text})
	return nil
}
func (r *retainedResponder) Final(context.Context, string) error { return nil }
func (r *retainedResponder) Close()                              {}
func (r *retainedResponder) event(name string, payload any) error {
	if message, ok := payload.(proto.Message); ok {
		r.emit(name, message)
		return nil
	}
	return errors.New("unsupported retained chat event")
}
