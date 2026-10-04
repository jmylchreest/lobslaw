package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/jmylchreest/lobslaw/internal/grpcinterceptors"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

const consoleBodyLimit int64 = 1 << 20
const consoleProbeTimeout time.Duration = 2 * time.Second

type forwardedConsoleIdentity struct{}

// Browser routes map to the closed set of typed peer operations. Backend
// dispatch is independent of this HTTP route metadata.
type consoleOperation struct {
	field, result, method, route string
	created                      bool
}

var consoleQueries = []consoleOperation{
	{"capabilities", "capabilities", http.MethodGet, "/v1/capabilities", false},
	{"tools", "tools", http.MethodGet, "/v1/tools", false},
	{"bots", "bots", http.MethodGet, "/v1/bots", false},
	{"bot", "bot", http.MethodGet, "/v1/bots/{id}", false},
	{"groups", "groups", http.MethodGet, "/v1/groups", false},
	{"group", "group", http.MethodGet, "/v1/groups/{id}", false},
	{"inbox", "inbox", http.MethodGet, "/v1/bots/{id}/inbox", false},
	{"inbox_item", "inbox_item", http.MethodGet, "/v1/inbox/{bot}/{id}", false},
	{"activity", "inbox", http.MethodGet, "/v1/activity", false},
	{"sessions", "sessions", http.MethodGet, "/v1/bots/{id}/sessions", false},
	{"transcript", "transcript", http.MethodGet, "/v1/sessions/{id}", false},
	{"routines", "routines", http.MethodGet, "/v1/bots/{id}/routines", false},
	{"memory", "memory", http.MethodGet, "/v1/bots/{id}/memory", false},
	{"prompt", "prompt", http.MethodGet, "/v1/prompts/{id}", false},
	{"plan_window", "plan", http.MethodGet, "/v1/plan", false},
}

var consoleMutations = []consoleOperation{
	{"create_bot", "bot", http.MethodPost, "/v1/bots", true},
	{"update_bot", "bot", http.MethodPatch, "/v1/bots/{id}", false},
	{"delete_bot", "deleted", http.MethodDelete, "/v1/bots/{id}", false},
	{"create_group", "group", http.MethodPost, "/v1/groups", true},
	{"update_group", "group", http.MethodPatch, "/v1/groups/{id}", false},
	{"delete_group", "deleted", http.MethodDelete, "/v1/groups/{id}", false},
	{"post_inbox", "inbox_item", http.MethodPost, "/v1/bots/{bot}/inbox", true},
	{"retry_inbox", "inbox_item", http.MethodPatch, "/v1/inbox/{bot}/{id}", false},
	{"cancel_inbox", "inbox_item", http.MethodPatch, "/v1/inbox/{bot}/{id}", false},
	{"resolve_prompt", "decision", http.MethodPost, "/v1/prompts/{id}/resolve", false},
}

func (s *Server) consoleContext(ctx context.Context, identity *pb.ConsoleIdentity) (context.Context, error) {
	cert := grpcinterceptors.VerifiedPeerCert(ctx)
	if cert == nil || mtls.IsOperatorCert(cert) {
		return nil, status.Error(codes.PermissionDenied, "console operations require a node peer")
	}
	if s.cfg.RemoteConsole != nil {
		return nil, status.Error(codes.FailedPrecondition, "console backends cannot be chained")
	}
	claims := identity.GetClaims()
	if claims.GetUserId() == "" || identity.GetPrincipal() != canonicalUserPrincipal(claims.GetUserId()) {
		return nil, status.Error(codes.InvalidArgument, "verified claims and matching canonical principal are required")
	}
	return context.WithValue(ctx, forwardedConsoleIdentity{}, claimsFromProto(claims)), nil
}

func consoleIdentity(claims *types.Claims) *pb.ConsoleIdentity {
	return &pb.ConsoleIdentity{Claims: claimsToProto(claims), Principal: canonicalUserPrincipal(claims.UserID)}
}

func (s *Server) QueryConsole(ctx context.Context, in *pb.QueryConsoleRequest) (*pb.QueryConsoleResponse, error) {
	ctx, err := s.consoleContext(ctx, in.GetIdentity())
	if err != nil {
		return nil, err
	}
	if in.GetTaskApprovals() != nil || in.GetTaskApproval() != nil {
		return s.queryConsoleTask(ctx, in)
	}
	if in.GetNotificationCandidates() != nil {
		return s.queryConsoleNotifications(ctx, in)
	}
	if in.GetLearnedReviews() != nil || in.GetLearnedReview() != nil {
		return s.queryConsoleLearned(ctx, in)
	}
	return s.queryConsoleOperations(ctx, in)
}

func (s *Server) MutateConsole(ctx context.Context, in *pb.MutateConsoleRequest) (*pb.MutateConsoleResponse, error) {
	ctx, err := s.consoleContext(ctx, in.GetIdentity())
	if err != nil {
		return nil, err
	}
	if in.GetDecideTaskApproval() != nil || in.GetCancelTaskApproval() != nil || in.GetRecoverTaskApproval() != nil {
		return s.mutateConsoleTask(ctx, in)
	}
	if in.GetDecideLearnedReview() != nil {
		return s.decideConsoleLearned(ctx, in)
	}
	return s.mutateConsoleOperations(ctx, in)
}

func consoleConvert(in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return status.Error(codes.Internal, "encode console value")
	}
	if message, ok := out.(proto.Message); ok {
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, message); err != nil {
			return status.Error(codes.InvalidArgument, "invalid console value")
		}
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		return status.Error(codes.InvalidArgument, "invalid console value")
	}
	return nil
}

func (s *Server) consoleRoute(local http.HandlerFunc) http.HandlerFunc {
	if s.cfg.RemoteConsole != nil {
		return s.remoteConsoleRoute
	}
	return local
}

func consoleMatch(route, path string) (map[string]any, bool) {
	a, b := strings.Split(route, "/"), strings.Split(path, "/")
	if len(a) != len(b) {
		return nil, false
	}
	fields := map[string]any{}
	for i, segment := range a {
		if strings.HasPrefix(segment, "{") {
			if b[i] == "" {
				return nil, false
			}
			fields[strings.Trim(segment, "{}")] = b[i]
		} else if segment != b[i] {
			return nil, false
		}
	}
	return fields, true
}

func (s *Server) remoteConsoleRoute(w http.ResponseWriter, r *http.Request) {
	authn, err := s.authenticateRequest(r)
	if err != nil {
		s.jsonErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	if err := s.checkCookieCSRF(r, authn); err != nil {
		s.jsonErr(w, http.StatusForbidden, err.Error())
		return
	}
	ctx, cancel := s.bindStream(r.Context(), authn.LoginID)
	defer cancel()
	identity := consoleIdentity(authn.Claims)
	if r.Method == http.MethodPost {
		if fields, ok := consoleMatch("/v1/bots/{bot}/messages", r.URL.Path); ok {
			s.remoteConsoleChat(w, r.WithContext(ctx), identity, fields["bot"].(string))
			return
		}
		if r.URL.Path == "/v1/messages" {
			s.remoteConsoleChat(w, r.WithContext(ctx), identity, "")
			return
		}
	}
	operations := consoleQueries
	var request proto.Message = &pb.QueryConsoleRequest{Identity: identity}
	if r.Method != http.MethodGet {
		operations = consoleMutations
		request = &pb.MutateConsoleRequest{Identity: identity}
	}
	for _, op := range operations {
		fields, match := consoleMatch(op.route, r.URL.Path)
		if !match || op.method != r.Method {
			continue
		}
		body, err := consoleRequestFields(w, r, fields)
		if err != nil {
			consoleHTTPError(s, w, err)
			return
		}
		if op.field == "retry_inbox" || op.field == "cancel_inbox" {
			action, _ := body["action"].(string)
			if action != "retry" && action != "cancel" {
				s.jsonErr(w, http.StatusBadRequest, "retry or cancel required")
				return
			}
			if op.field != action+"_inbox" {
				for _, candidate := range operations {
					if candidate.field == action+"_inbox" {
						op = candidate
						break
					}
				}
			}
		}
		if op.field == "update_bot" {
			for _, key := range []string{"tools", "may_message"} {
				if v, ok := body[key]; ok {
					body[key] = map[string]any{"values": v}
				}
			}
		}
		m := request.ProtoReflect()
		f := m.Descriptor().Fields().ByName(protoreflect.Name(op.field))
		if f.Kind() == protoreflect.StringKind {
			m.Set(f, protoreflect.ValueOfString(r.URL.Query().Get("window")))
		} else if err := consoleConvert(body, m.Mutable(f).Message().Interface()); err != nil {
			consoleHTTPError(s, w, err)
			return
		}
		var response proto.Message
		switch req := request.(type) {
		case *pb.QueryConsoleRequest:
			response, err = s.cfg.RemoteConsole.QueryConsole(ctx, req)
		case *pb.MutateConsoleRequest:
			response, err = s.cfg.RemoteConsole.MutateConsole(ctx, req)
		}
		if err != nil {
			consoleHTTPError(s, w, err)
			return
		}
		result := response.ProtoReflect()
		field := result.Descriptor().Fields().ByName(protoreflect.Name(op.result))
		if !result.Has(field) {
			s.jsonErr(w, http.StatusBadGateway, "missing console result")
			return
		}
		if op.result == "deleted" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		code := http.StatusOK
		if op.created {
			code = http.StatusCreated
		}
		respondJSON(w, code, consolePublicValue(result.Get(field).Message()))
		return
	}
	s.jsonErr(w, http.StatusMethodNotAllowed, "unsupported console operation")
}

func consoleRequestFields(w http.ResponseWriter, r *http.Request, targets map[string]any) (map[string]any, error) {
	body := map[string]any{}
	if r.Method != http.MethodGet && r.Method != http.MethodDelete {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, consoleBodyLimit))
		dec.UseNumber()
		if dec.Decode(&body) != nil || body == nil || dec.Decode(new(any)) != io.EOF {
			return nil, status.Error(codes.InvalidArgument, "one JSON object required")
		}
	}
	for key, value := range targets {
		body[key] = value
	}
	for _, key := range []string{"status", "window"} {
		if v := r.URL.Query().Get(key); v != "" {
			body[key] = v
		}
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if r.URL.Path == "/v1/activity" {
			limit, err := activityLimit(raw)
			if err != nil {
				return nil, status.Error(codes.InvalidArgument, err.Error())
			}
			body["limit"] = limit
			return body, nil
		}
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || n < 0 {
			return nil, status.Error(codes.InvalidArgument, "invalid limit")
		}
		body["limit"] = n
	}
	return body, nil
}

// Preserve the REST contract's empty arrays, false booleans and numeric counters
// rather than leaking generated Go JSON omitempty or protojson's string integers.
func consolePublicValue(m protoreflect.Message) map[string]any {
	out := map[string]any{}
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		v := m.Get(f)
		switch {
		case f.IsList():
			list := v.List()
			values := make([]any, 0, list.Len())
			for j := 0; j < list.Len(); j++ {
				item := list.Get(j)
				if f.Kind() == protoreflect.MessageKind {
					values = append(values, consolePublicValue(item.Message()))
				} else {
					values = append(values, item.Interface())
				}
			}
			out[string(f.Name())] = values
		case f.Kind() == protoreflect.MessageKind:
			if m.Has(f) {
				out[string(f.Name())] = consolePublicValue(v.Message())
			}
		default:
			if f.Name() == "revision" && f.Kind() == protoreflect.Uint64Kind && v.Uint() > 9007199254740991 {
				out[string(f.Name())] = strconv.FormatUint(v.Uint(), 10)
			} else {
				out[string(f.Name())] = v.Interface()
			}
		}
	}
	return out
}

func consoleHTTPError(s *Server, w http.ResponseWriter, err error) {
	code := http.StatusServiceUnavailable
	switch status.Code(err) {
	case codes.InvalidArgument:
		code = http.StatusBadRequest
	case codes.PermissionDenied:
		code = http.StatusForbidden
	case codes.Unauthenticated:
		code = http.StatusUnauthorized
	case codes.NotFound:
		code = http.StatusNotFound
	case codes.Aborted, codes.FailedPrecondition:
		code = http.StatusConflict
	case codes.ResourceExhausted:
		code = http.StatusRequestEntityTooLarge
	case codes.Internal:
		code = http.StatusInternalServerError
	}
	s.jsonErr(w, code, status.Convert(err).Message())
}

func (s *Server) remoteCapabilities(ctx context.Context, claims *types.Claims) (capabilitiesResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, consoleProbeTimeout)
	defer cancel()
	out, err := s.cfg.RemoteConsole.QueryConsole(ctx, &pb.QueryConsoleRequest{Identity: consoleIdentity(claims), Query: &pb.QueryConsoleRequest_Capabilities{Capabilities: &pb.ConsoleEmpty{}}})
	if err != nil {
		return capabilitiesResponse{}, err
	}
	caps := out.GetCapabilities()
	if caps == nil {
		return capabilitiesResponse{}, errors.New("missing console capabilities")
	}
	var result capabilitiesResponse
	if err := consoleConvert(caps.Compute, &result.Compute); err != nil {
		return result, err
	}
	if err := consoleConvert(caps.ComputeTeams, &result.ComputeTeams); err != nil {
		return result, err
	}
	err = consoleConvert(caps.UiWeb, &result.UIWeb)
	return result, err
}

func (s *Server) remoteConsoleChat(w http.ResponseWriter, r *http.Request, identity *pb.ConsoleIdentity, bot string) {
	var body messageRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, consoleBodyLimit)).Decode(&body) != nil {
		s.jsonErr(w, http.StatusBadRequest, "invalid message")
		return
	}
	if len(body.UploadIDs) != 0 {
		s.jsonErr(w, http.StatusBadRequest, "remote console uploads are not supported")
		return
	}
	stream, err := s.cfg.RemoteConsole.ChatConsole(r.Context(), &pb.ChatConsoleRequest{Identity: identity, Bot: bot, Message: body.Message, SessionId: body.SessionID, TurnId: body.TurnID, Model: body.Model})
	if err != nil {
		consoleHTTPError(s, w, err)
		return
	}
	streaming := bot != "" || acceptsEventStream(r)
	if streaming {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	}
	started := false
	for {
		part, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			if !started {
				consoleHTTPError(s, w, err)
			} else if r.Context().Err() == nil {
				sendSSE(w, w.(http.Flusher), "error", &pb.ConsoleFailure{Error: "console backend disconnected", Message: "console backend disconnected"})
			}
			return
		}
		m := part.ProtoReflect()
		f := m.WhichOneof(m.Descriptor().Oneofs().ByName("event"))
		if f == nil {
			continue
		}
		if part.GetAccepted() != nil && !started {
			respondJSON(w, http.StatusAccepted, map[string]string{"error": part.GetAccepted().Message})
			return
		}
		var payload any
		if reply := part.GetReply(); reply != nil {
			// Task evidence uses protobuf JSON on both local and remote SSE:
			// exact uint64 strings, timestamps and nested message field names.
			payload = reply
		} else {
			payload = consolePublicValue(m.Get(f).Message())
		}
		if !streaming {
			if part.GetFinal() != nil {
				respondJSON(w, http.StatusOK, payload)
				return
			}
			if part.GetError() != nil {
				s.jsonErr(w, http.StatusInternalServerError, firstNonEmpty(part.GetError().Error, part.GetError().Message))
				return
			}
			continue
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			s.jsonErr(w, http.StatusInternalServerError, "streaming unavailable")
			return
		}
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Accel-Buffering", "no")
			started = true
		}
		sendSSE(w, flusher, string(f.Name()), payload)
	}
}
