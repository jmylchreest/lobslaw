package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// The peer protocol is a closed set of typed operations. These adapters keep
// the existing owner checks, revision checks and audit hooks shared with REST;
// a peer never chooses an HTTP handler, method, path, status or JSON body.
type consoleOperation struct {
	field, result, method, route string
	handler                      func(*Server, http.ResponseWriter, *http.Request)
	created                      bool
}

var consoleQueries = []consoleOperation{
	{"capabilities", "capabilities", http.MethodGet, "/v1/capabilities", (*Server).handleCapabilities, false},
	{"tools", "tools", http.MethodGet, "/v1/tools", (*Server).handleTools, false},
	{"bots", "bots", http.MethodGet, "/v1/bots", (*Server).handleBots, false},
	{"bot", "bot", http.MethodGet, "/v1/bots/{id}", (*Server).handleBots, false},
	{"groups", "groups", http.MethodGet, "/v1/groups", (*Server).handleGroups, false},
	{"group", "group", http.MethodGet, "/v1/groups/{id}", (*Server).handleGroups, false},
	{"inbox", "inbox", http.MethodGet, "/v1/bots/{id}/inbox", (*Server).handleBots, false},
	{"inbox_item", "inbox_item", http.MethodGet, "/v1/inbox/{bot}/{id}", (*Server).handleInboxItem, false},
	{"activity", "inbox", http.MethodGet, "/v1/activity", (*Server).handleActivity, false},
	{"sessions", "sessions", http.MethodGet, "/v1/bots/{id}/sessions", (*Server).handleBots, false},
	{"transcript", "transcript", http.MethodGet, "/v1/sessions/{id}", (*Server).handleSessionTranscript, false},
	{"routines", "routines", http.MethodGet, "/v1/bots/{id}/routines", (*Server).handleBots, false},
	{"memory", "memory", http.MethodGet, "/v1/bots/{id}/memory", (*Server).handleBots, false},
	{"prompt", "prompt", http.MethodGet, "/v1/prompts/{id}", (*Server).handlePrompt, false},
	{"plan_window", "plan", http.MethodGet, "/v1/plan", (*Server).handlePlan, false},
}

var consoleMutations = []consoleOperation{
	{"create_bot", "bot", http.MethodPost, "/v1/bots", (*Server).handleBots, true},
	{"update_bot", "bot", http.MethodPatch, "/v1/bots/{id}", (*Server).handleBots, false},
	{"delete_bot", "deleted", http.MethodDelete, "/v1/bots/{id}", (*Server).handleBots, false},
	{"create_group", "group", http.MethodPost, "/v1/groups", (*Server).handleGroups, true},
	{"update_group", "group", http.MethodPatch, "/v1/groups/{id}", (*Server).handleGroups, false},
	{"delete_group", "deleted", http.MethodDelete, "/v1/groups/{id}", (*Server).handleGroups, false},
	{"post_inbox", "inbox_item", http.MethodPost, "/v1/bots/{bot}/inbox", (*Server).handleBots, true},
	{"retry_inbox", "inbox_item", http.MethodPatch, "/v1/inbox/{bot}/{id}", (*Server).handleInboxItem, false},
	{"cancel_inbox", "inbox_item", http.MethodPatch, "/v1/inbox/{bot}/{id}", (*Server).handleInboxItem, false},
	{"resolve_prompt", "decision", http.MethodPost, "/v1/prompts/{id}/resolve", (*Server).handlePrompt, false},
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
	if in.GetLearnedReviews() != nil || in.GetLearnedReview() != nil {
		return s.queryConsoleLearned(ctx, in)
	}
	out := new(pb.QueryConsoleResponse)
	err = s.consoleOperation(ctx, in, out, consoleQueries, "query")
	return out, err
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
	out := new(pb.MutateConsoleResponse)
	err = s.consoleOperation(ctx, in, out, consoleMutations, "operation")
	return out, err
}

func (s *Server) consoleOperation(ctx context.Context, in, out proto.Message, operations []consoleOperation, oneof string) error {
	m := in.ProtoReflect()
	f := m.WhichOneof(m.Descriptor().Oneofs().ByName(protoreflect.Name(oneof)))
	if f == nil {
		return status.Error(codes.InvalidArgument, "console operation required")
	}
	for _, op := range operations {
		if string(f.Name()) != op.field {
			continue
		}
		if (op.field == "prompt" || op.field == "resolve_prompt") && s.cfg.Prompts == nil {
			return status.Error(codes.Unavailable, "conversation approvals unavailable")
		}
		body := map[string]any{}
		if f.Kind() == protoreflect.StringKind {
			body["window"] = m.Get(f).String()
		} else if err := consoleConvert(m.Get(f).Message().Interface(), &body); err != nil {
			return err
		}
		if op.field == "update_bot" {
			for _, key := range []string{"tools", "may_message"} {
				if nested, ok := body[key].(map[string]any); ok {
					values := nested["values"]
					if values == nil {
						values = []string{}
					}
					body[key] = values
				}
			}
		}
		if op.field == "retry_inbox" {
			body["action"] = "retry"
		}
		if op.field == "cancel_inbox" {
			body["action"] = "cancel"
		}
		path, err := consolePath(op.route, body)
		if err != nil {
			return err
		}
		q := url.Values{}
		for _, key := range []string{"limit", "status", "window"} {
			if v, ok := body[key]; ok {
				q.Set(key, fmt.Sprint(v))
			}
		}
		if len(q) != 0 {
			path += "?" + q.Encode()
		}
		r, err := consoleLocalRequest(ctx, op.method, path, body)
		if err != nil {
			return err
		}
		w := newConsoleResult()
		op.handler(s, w, r)
		if err := w.resultError(); err != nil {
			return err
		}
		result := out.ProtoReflect()
		field := result.Descriptor().Fields().ByName(protoreflect.Name(op.result))
		value := result.Mutable(field).Message().Interface()
		if op.result == "deleted" {
			return nil
		}
		if op.result == "capabilities" {
			var caps capabilitiesResponse
			if err := json.Unmarshal(w.body.Bytes(), &caps); err != nil {
				return status.Error(codes.Internal, "invalid capabilities")
			}
			return consoleConvert(map[string]any{"compute": caps.Compute, "compute_teams": caps.ComputeTeams, "ui_web": caps.UIWeb}, value)
		}
		if err := json.Unmarshal(w.body.Bytes(), value); err != nil {
			return status.Error(codes.Internal, "invalid console result")
		}
		return nil
	}
	return status.Error(codes.InvalidArgument, "unsupported console operation")
}

func consoleConvert(in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return status.Error(codes.Internal, "encode console value")
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		return status.Error(codes.InvalidArgument, "invalid console value")
	}
	return nil
}

func consolePath(route string, fields map[string]any) (string, error) {
	for _, key := range []string{"id", "bot"} {
		if !strings.Contains(route, "{"+key+"}") {
			continue
		}
		value, _ := fields[key].(string)
		if value == "" || strings.ContainsAny(value, "/\\?#%") || value == "." || value == ".." {
			return "", status.Error(codes.InvalidArgument, "invalid console identifier")
		}
		route = strings.ReplaceAll(route, "{"+key+"}", url.PathEscape(value))
	}
	return route, nil
}

func consoleLocalRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	b, err := json.Marshal(body)
	if err != nil || int64(len(b)) > consoleBodyLimit {
		return nil, status.Error(codes.InvalidArgument, "console request too large")
	}
	r, err := http.NewRequestWithContext(ctx, method, path, bytes.NewReader(b))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid console arguments")
	}
	r.Header.Set("Content-Type", "application/json")
	return r, nil
}

type consoleResult struct {
	header http.Header
	body   bytes.Buffer
	code   int
	err    error
}

func newConsoleResult() *consoleResult       { return &consoleResult{header: make(http.Header)} }
func (w *consoleResult) Header() http.Header { return w.header }
func (w *consoleResult) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}
func (w *consoleResult) Write(p []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	if int64(w.body.Len()+len(p)) > consoleBodyLimit {
		w.err = status.Error(codes.ResourceExhausted, "console result too large")
		return 0, w.err
	}
	return w.body.Write(p)
}
func (w *consoleResult) resultError() error {
	if w.err != nil {
		return w.err
	}
	if w.code < http.StatusBadRequest {
		return nil
	}
	code := codes.Internal
	switch w.code {
	case http.StatusBadRequest, http.StatusMethodNotAllowed:
		code = codes.InvalidArgument
	case http.StatusUnauthorized:
		code = codes.Unauthenticated
	case http.StatusForbidden:
		code = codes.PermissionDenied
	case http.StatusNotFound:
		code = codes.NotFound
	case http.StatusConflict:
		code = codes.Aborted
	case http.StatusRequestEntityTooLarge, http.StatusTooManyRequests:
		code = codes.ResourceExhausted
	case http.StatusServiceUnavailable:
		code = codes.Unavailable
	}
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(w.body.Bytes(), &body)
	if body.Error == "" {
		body.Error = strings.TrimSpace(w.body.String())
	}
	return status.Error(code, body.Error)
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
			out[string(f.Name())] = v.Interface()
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

func (s *Server) ChatConsole(in *pb.ChatConsoleRequest, stream grpc.ServerStreamingServer[pb.ChatConsoleResponse]) error {
	ctx, err := s.consoleContext(stream.Context(), in.GetIdentity())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	path := "/v1/messages"
	if in.Bot != "" {
		path, err = consolePath("/v1/bots/{bot}/messages", map[string]any{"bot": in.Bot})
		if err != nil {
			return err
		}
	}
	r, err := consoleLocalRequest(ctx, http.MethodPost, path, messageRequest{Message: in.Message, SessionID: in.SessionId, TurnID: in.TurnId, Model: in.Model})
	if err != nil {
		return err
	}
	r.Header.Set("Accept", "text/event-stream")
	w := &consoleEvents{consoleResult: newConsoleResult(), stream: stream, cancel: cancel}
	if in.Bot == "" {
		s.handleMessages(w, r)
	} else {
		s.handleBots(w, r)
	}
	if err := w.resultError(); err != nil {
		return err
	}
	if w.code == http.StatusAccepted {
		return w.consoleEvent("accepted", &pb.ConsoleFailure{Message: "Message accepted by the active turn."})
	}
	return w.sendErr
}

// Events cross gRPC as a typed oneof, never as SSE frames. The browser-facing
// node alone chooses SSE formatting. The mutex covers heartbeat and turn events.
type consoleEvents struct {
	*consoleResult
	mu      sync.Mutex
	stream  grpc.ServerStreamingServer[pb.ChatConsoleResponse]
	sendErr error
	cancel  context.CancelFunc
}

func (w *consoleEvents) Flush() {}
func (w *consoleEvents) consoleEvent(name string, payload any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sendErr != nil {
		return w.sendErr
	}
	out := new(pb.ChatConsoleResponse)
	m := out.ProtoReflect()
	f := m.Descriptor().Fields().ByName(protoreflect.Name(name))
	if f == nil {
		w.sendErr = status.Error(codes.Internal, "unsupported console event")
		w.cancel()
		return w.sendErr
	}
	if err := consoleConvert(payload, m.Mutable(f).Message().Interface()); err != nil {
		w.sendErr = err
		w.cancel()
		return err
	}
	w.sendErr = w.stream.Send(out)
	if w.sendErr != nil {
		w.cancel()
	}
	return w.sendErr
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
		payload := consolePublicValue(m.Get(f).Message())
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
