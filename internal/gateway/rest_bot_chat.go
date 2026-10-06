package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/turn"
)

// handleBotChat serves POST /v1/bots/{id}/messages.
//
// Talking to a SPECIFIC bot, which /v1/messages cannot do — that
// endpoint is the coordinator's, shared with Telegram and Slack, and a
// console that could only reach the coordinator would make every
// specialist something you can configure but not converse with.
//
// Streamed as Server-Sent Events. A bot turn can run tools for a
// minute, and a request that returns nothing until it finishes is
// indistinguishable from one that has hung — which is how somebody
// ends up reloading and starting a second turn.
//
// The turn goes through turn.Runner, never a concrete agent: the
// gateway is the transport and must not depend on internal/compute.
const botChatHeartbeat time.Duration = 10 * time.Second

func (s *Server) handleBotChat(w http.ResponseWriter, r *http.Request, botID string) {
	// Authenticate before the 200 and the SSE headers go out, so an
	// unauthenticated caller gets a 401 rather than a streamed error
	// over a successful status line. Cookie-aware, so the web console's
	// login session reaches this route.
	authn, authErr := s.authenticateRequest(r)
	if authErr != nil {
		s.jsonErr(w, http.StatusUnauthorized, authErr.Error())
		return
	}
	if err := s.checkCookieCSRF(r, authn); err != nil {
		s.jsonErr(w, http.StatusForbidden, err.Error())
		return
	}
	if s.runner == nil {
		s.jsonErr(w, http.StatusServiceUnavailable, "this node cannot run turns")
		return
	}
	if r.Method != http.MethodPost {
		s.jsonErr(w, http.StatusMethodNotAllowed, "POST")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var body messageRequest
	if !s.decodeMessageRequest(w, r, &body) {
		return
	}
	if len(body.UploadIDs) > 0 && !authenticatedUploadClaims(authn.Claims) {
		s.jsonErr(w, http.StatusUnauthorized, "authentication required for uploads")
		return
	}
	attachments, release, err := s.uploads.acquire(authn.Claims.UserID, body.UploadIDs, time.Now())
	if err != nil {
		s.jsonErr(w, http.StatusNotFound, errUploadUnavailable.Error())
		return
	}
	defer release()
	if strings.TrimSpace(body.Message) == "" {
		body.Message = "Please examine the attached files."
	}
	ctx, stopStream := s.bindStream(r.Context(), authn.LoginID)
	defer stopStream()
	err = s.runBotChat(ctx, authn.Claims, botID, body.Message, attachments, func() (chatEmitter, error) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			// Without flushing, SSE is just a slow JSON response that
			// arrives all at once — worse than admitting it.
			return nil, status.Error(codes.Internal, "this server cannot stream")
		}
		// Clear the write deadline for THIS response only. The server's
		// WriteTimeout is an absolute deadline from the start of the
		// response, so a bot turn against a real model routinely runs past
		// it and every one would be killed mid-stream even though the turn
		// completed and was recorded. Scoped here so a stuck request
		// anywhere else still hits the server-wide bound.
		if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
			// Not fatal — an unwrapped ResponseWriter in a test has no
			// deadline to clear, and the stream is still correct.
			s.log.Debug("chat: could not clear write deadline", "err", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		// Reverse proxies buffer by default and would hold the whole
		// stream until the turn ended, which defeats the point.
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		// Three goroutines can write to this stream: the heartbeat ticker,
		// and this handler. An http.ResponseWriter is not safe for
		// concurrent use, so one guarded emitter rather than a mutex the
		// call sites take themselves.
		var sseMu sync.Mutex
		emit := func(event string, payload proto.Message) {
			sseMu.Lock()
			defer sseMu.Unlock()
			if task, ok := payload.(taskEvidenceEvent); ok {
				sendSSE(w, flusher, event, task.ConsoleBotReply)
			} else {
				sendSSE(w, flusher, event, consolePublicValue(payload.ProtoReflect()))
			}
		}

		return emit, nil
	})
	if err != nil {
		s.chatHTTPError(w, err)
	}

}

func botTaskCommand(message string, available bool) (string, bool, error) {
	fields := strings.Fields(message)
	if len(fields) == 0 || fields[0] != "/task" {
		return "", false, nil
	}
	body := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(message), "/task"))
	if body == "" {
		return "", true, errors.New("usage: /task <work to carry out>")
	}
	if !available {
		return "", true, errors.New("tasks are unavailable on this node")
	}
	return body, true, nil
}

// Only runner-recorded dispatch proves execution. A tool name, output or absent
// error alone cannot distinguish a refusal from a completed invocation.
func invokedToolNames(calls []turn.ToolInvocation) []string {
	return receiptToolNames(calls, false)
}

func unconfirmedToolNames(calls []turn.ToolInvocation) []string {
	return receiptToolNames(calls, true)
}

func returnedToolCount(calls []turn.ToolInvocation) int {
	count := 0
	for _, call := range calls {
		if call.ExecutionStatus == turn.ReceiptExecuted {
			count++
		}
	}
	return count
}

func receiptToolNames(calls []turn.ToolInvocation, unconfirmed bool) []string {
	seen := make(map[string]struct{}, len(calls))
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		if c.ToolName == "" || (c.ExecutionStatus != turn.ReceiptExecuted) != unconfirmed {
			continue
		}
		if _, ok := seen[c.ToolName]; ok {
			continue
		}
		seen[c.ToolName] = struct{}{}
		out = append(out, c.ToolName)
	}
	sort.Strings(out)
	return out
}

// sendSSE writes one event. Errors are dropped: the only failure here
// is a client that has gone away, and there is nowhere left to report
// that to.
func sendSSE(w http.ResponseWriter, flusher http.Flusher, event string, payload any) {
	var body []byte
	var err error
	if message, ok := payload.(proto.Message); ok {
		body, err = protojson.Marshal(message)
	} else {
		body, err = json.Marshal(payload)
	}
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body)
	flusher.Flush()
}
