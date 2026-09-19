package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/grpcinterceptors"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

const consoleForwardBodyLimit int64 = 1 << 20
const consoleForwardChunkSize int = 64 << 10
const consoleProbeTimeout time.Duration = 2 * time.Second

type forwardedConsoleIdentity struct{}

// ConsoleForward authenticates the machine independently from the asserted user. No
// browser-controlled header can inject forwardedConsoleIdentity.
func (s *Server) ConsoleForward(in *lobslawv1.ConsoleForwardRequest, stream grpc.ServerStreamingServer[lobslawv1.ConsoleForwardResponse]) error {
	cert := grpcinterceptors.VerifiedPeerCert(stream.Context())
	if cert == nil || mtls.IsOperatorCert(cert) {
		return status.Error(codes.PermissionDenied, "console forwarding requires a node peer")
	}
	if s.cfg.RemoteConsole != nil {
		return status.Error(codes.FailedPrecondition, "console forwarding cannot chain backends")
	}
	if in == nil || in.Claims == nil || in.Claims.UserId == "" || int64(len(in.Body)) > consoleForwardBodyLimit {
		return status.Error(codes.InvalidArgument, "user claims and a bounded request are required")
	}
	u, err := url.ParseRequestURI(in.Path)
	if err != nil || u.IsAbs() || u.Host != "" {
		return status.Error(codes.InvalidArgument, "invalid console path")
	}
	handler := s.backendConsoleHandler(u.Path)
	if handler == nil {
		return status.Error(codes.PermissionDenied, "route cannot be forwarded")
	}
	ctx := context.WithValue(stream.Context(), forwardedConsoleIdentity{}, claimsFromProto(in.Claims))
	r, err := http.NewRequestWithContext(ctx, in.Method, in.Path, bytes.NewReader(in.Body))
	if err != nil {
		return status.Error(codes.InvalidArgument, "invalid console request")
	}
	r.Header.Set("Content-Type", "application/json")
	if in.EventStream {
		r.Header.Set("Accept", "text/event-stream")
	}
	w := &consoleStreamWriter{stream: stream, header: make(http.Header)}
	handler(w, r)
	w.Flush()
	return w.err
}

func (s *Server) backendConsoleHandler(path string) http.HandlerFunc {
	switch {
	case path == "/v1/messages":
		return s.handleMessages
	case path == "/v1/capabilities":
		return s.handleCapabilities
	case path == "/v1/tools":
		return s.handleTools
	case strings.HasPrefix(path, "/v1/computers/"):
		return s.handleComputer
	case path == "/v1/plan":
		return s.handlePlan
	case path == "/v1/bots" || strings.HasPrefix(path, "/v1/bots/"):
		return s.handleBots
	case path == "/v1/groups" || strings.HasPrefix(path, "/v1/groups/"):
		return s.handleGroups
	case strings.HasPrefix(path, "/v1/inbox/"):
		return s.handleInboxItem
	case path == "/v1/activity":
		return s.handleActivity
	case strings.HasPrefix(path, "/v1/sessions/"):
		return s.handleSessionTranscript
	case strings.HasPrefix(path, "/v1/prompts/"):
		return s.handlePrompt
	default:
		return nil
	}
}

func (s *Server) consoleRoute(local http.HandlerFunc) http.HandlerFunc {
	if s.cfg.RemoteConsole != nil {
		return s.forwardConsole
	}
	return local
}

func (s *Server) forwardConsole(w http.ResponseWriter, r *http.Request) {
	authn, err := s.authenticateRequest(r)
	if err != nil {
		s.jsonErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	if err := s.checkCookieCSRF(r, authn); err != nil {
		s.jsonErr(w, http.StatusForbidden, err.Error())
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, consoleForwardBodyLimit))
	if err != nil {
		s.jsonErr(w, http.StatusRequestEntityTooLarge, "console request too large")
		return
	}
	ctx, cancel := s.bindStream(r.Context(), authn.LoginID)
	defer cancel()
	stream, err := s.cfg.RemoteConsole.ConsoleForward(ctx, &lobslawv1.ConsoleForwardRequest{
		Method: r.Method, Path: r.URL.RequestURI(), Body: body,
		Claims: claimsToProto(authn.Claims), EventStream: acceptsEventStream(r),
	})
	if err != nil {
		s.jsonErr(w, http.StatusServiceUnavailable, "console backend unavailable")
		return
	}
	started := false
	for {
		part, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			if !started {
				s.jsonErr(w, http.StatusServiceUnavailable, "console backend unavailable")
			} else if ctx.Err() == nil && w.Header().Get("Content-Type") == "text/event-stream" {
				if flusher, ok := w.(http.Flusher); ok {
					sendSSE(w, flusher, "error", map[string]any{"message": "console backend disconnected", "error": "console backend disconnected"})
				}
			}
			return
		}
		if !started {
			w.Header().Set("Content-Type", part.ContentType)
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Accel-Buffering", "no")
			if part.ContentType == "text/event-stream" {
				_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
			}
			w.WriteHeader(int(part.Status))
			started = true
		}
		if _, err := w.Write(part.Data); err != nil {
			return
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func (s *Server) remoteCapabilities(ctx context.Context, claims *types.Claims) (capabilitiesResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, consoleProbeTimeout)
	defer cancel()
	stream, err := s.cfg.RemoteConsole.ConsoleForward(ctx, &lobslawv1.ConsoleForwardRequest{Method: http.MethodGet, Path: "/v1/capabilities", Claims: claimsToProto(claims)})
	if err != nil {
		return capabilitiesResponse{}, err
	}
	var body bytes.Buffer
	for {
		part, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return capabilitiesResponse{}, err
		}
		if part.Status != http.StatusOK || int64(body.Len()+len(part.Data)) > consoleForwardBodyLimit {
			return capabilitiesResponse{}, errors.New("invalid capability response")
		}
		body.Write(part.Data)
	}
	var out capabilitiesResponse
	err = json.Unmarshal(body.Bytes(), &out)
	return out, err
}

type consoleStreamWriter struct {
	stream grpc.ServerStreamingServer[lobslawv1.ConsoleForwardResponse]
	header http.Header
	mu     sync.Mutex
	status int
	sent   bool
	err    error
}

func (w *consoleStreamWriter) Header() http.Header { return w.header }
func (w *consoleStreamWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = code
	}
}
func (w *consoleStreamWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	for len(p) > 0 && w.err == nil {
		size := min(len(p), consoleForwardChunkSize)
		w.send(p[:size])
		p = p[size:]
	}
	return n - len(p), w.err
}
func (w *consoleStreamWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.sent {
		w.send(nil)
	}
}
func (w *consoleStreamWriter) send(p []byte) {
	if w.err != nil {
		return
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.err = w.stream.Send(&lobslawv1.ConsoleForwardResponse{Status: int32(w.status), ContentType: w.header.Get("Content-Type"), Data: p})
	w.sent = true
}
