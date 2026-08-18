// Package gateway wires user-facing channels (REST, Telegram) on
// top of the node's internal services. The agent loop doesn't know
// about HTTP or Telegram — each channel is a thin adapter that
// translates an inbound request into an internal
// compute.ProcessMessageRequest and translates the response back.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/pkg/auth"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// RESTConfig tunes the REST channel.
type RESTConfig struct {
	// Notices appends operator notices to outbound replies. Nil
	// disables them entirely, which is what a deployment that never
	// opted this channel in gets.
	Notices *Notices

	// QueueMode and QueueDebounce configure per-session turn
	// serialisation. Zero value is QueueSerial.
	QueueMode     QueueMode
	QueueDebounce time.Duration

	// Leaser adds cluster-wide turn ownership on top of the in-process
	// queue. Nil is correct for a single node.
	Leaser SessionLeaser

	// Responsiveness timers, shared with Telegram. HardTimeout is the
	// one that matters even for a client that cannot stream: REST had
	// no cap at all, so a stalled provider hung the request until the
	// client gave up. Zero on any field takes the default; negative
	// disables that timer.
	TypingInterval time.Duration
	InterimTimeout time.Duration
	HardTimeout    time.Duration

	// Soul gates interim progress on the personality's directness, as
	// on Telegram. Nil emits them for any client that asked to stream.
	Soul func() *types.SoulConfig

	// Addr is the host:port to bind. Empty → ":8443" by default.
	Addr string

	// TLSCert / TLSKey enable HTTPS. Both empty = plaintext HTTP
	// (fine for localhost / behind a reverse proxy with TLS
	// termination elsewhere).
	TLSCert string
	TLSKey  string

	// ReadTimeout / WriteTimeout / IdleTimeout override the Go
	// defaults. Zero means "use sensible defaults".
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration

	// DefaultScope is the security scope assigned to unauthenticated
	// requests. A valid JWT (Authorization: Bearer ...) supersedes
	// this with the token's own scope claim.
	DefaultScope string

	// DefaultBudget is the per-turn budget applied to each message.
	// Zero caps mean unlimited. Callers typically pass caps derived
	// from config.Compute.Budgets.
	DefaultBudget compute.BudgetCaps

	// JWTValidator validates inbound Authorization: Bearer tokens.
	// Nil means accept unauthenticated requests with DefaultScope
	// attribution — acceptable for localhost / reverse-proxy-
	// terminated deployments, but loud in logs so operators see it.
	JWTValidator *auth.Validator

	// RequireAuth flips "missing or bad token" from "use DefaultScope"
	// to "reject with 401". Deployments that MUST have valid JWTs
	// (anything reachable from the public internet) set this true.
	RequireAuth bool

	// Telegram, when non-nil, mounts the Telegram webhook handler
	// at /telegram on the same mux. Shares the server's TLS + port
	// so operators don't need a second listener.
	Telegram *TelegramHandler

	// Webhooks are generic inbound-webhook channels (Zapier,
	// IFTTT, n8n, GitHub Actions, etc.). Each is mounted at its
	// PathPrefix on the same mux. Empty slice = no webhooks.
	Webhooks []*WebhookHandler

	// Prompts is the confirmation-prompt registry. When set, the
	// REST server mounts /v1/prompts/{id} endpoints. Agents that
	// return NeedsConfirmation register a prompt here; UIs poll
	// and resolve. Nil = no prompt flow (NeedsConfirmation returns
	// as plain text like Phase 6e).
	Prompts Prompts

	// ConfirmationTTL is how long a pending prompt waits before
	// auto-denying on timeout. 0 → 5 minutes default.
	ConfirmationTTL time.Duration

	// Plan, when non-nil, mounts GET /v1/plan on the mux. The
	// handler wraps PlanService.GetPlan, translates the window
	// query parameter into a protobuf Duration, and returns a
	// JSON aggregate view suitable for UIs / skills.
	Plan PlanService

	// Sessions is the durable conversation transcript store, used for
	// requests that carry a session_id. Nil leaves REST on the
	// in-memory buffer — conversations then last only as long as the
	// process does.
	Sessions SessionStore

	// Compactor folds aged-out conversation into a running summary.
	// Nil disables compaction.
	Compactor SessionCompactor

	// Conversation tunes replay depth and the degraded-mode cache.
	Conversation ConversationConfig

	// Logger is used for structured log output. Nil → slog.Default().
	Logger *slog.Logger
}

// PlanService is the subset of lobslawv1.PlanServiceServer that the
// REST layer actually calls. Kept narrow so tests can pass a fake
// without constructing a real Raft-backed service.
type PlanService interface {
	GetPlan(ctx context.Context, req *lobslawv1.GetPlanRequest) (*lobslawv1.GetPlanResponse, error)
}

// Server is the REST channel handler. Stateful only for lifecycle
// bookkeeping (net.Listener, underlying http.Server).
type Server struct {
	// gate serialises turns per session. See turnqueue.go.
	gate *TurnGate

	cfg   RESTConfig
	agent *compute.Agent
	log   *slog.Logger
	conv  *conversationLog

	mu       sync.Mutex
	httpSrv  *http.Server
	listener net.Listener
	ready    bool // flipped to true when Start() completes bind; checked by /readyz
}

// NewServer constructs the REST server with explicit dependencies.
// agent may be nil — /healthz still responds, /v1/messages returns
// 503. Lets a node with Compute disabled still expose health
// endpoints for load-balancer probes.
func NewServer(cfg RESTConfig, agent *compute.Agent) *Server {
	if cfg.Addr == "" {
		cfg.Addr = ":8443"
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = 30 * time.Second
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 60 * time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 2 * time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Server{
		cfg:   cfg,
		agent: agent,
		log:   cfg.Logger,
		gate:  NewTurnGate(cfg.QueueMode, cfg.QueueDebounce, cfg.Logger).WithLeaser(cfg.Leaser, 0),
		conv:  newConversationLog(cfg.Sessions, cfg.Compactor, cfg.Conversation, cfg.Logger),
	}
}

// Start binds the listener and serves. Blocks until ctx is
// cancelled or the HTTP server returns an error. A cancelled ctx
// triggers a graceful shutdown with a bounded timeout.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", s.handleMessages)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	if s.cfg.Telegram != nil && s.cfg.Telegram.Mode() == TelegramModeWebhook {
		mux.Handle("/telegram", s.cfg.Telegram)
	}
	for _, wh := range s.cfg.Webhooks {
		mux.Handle(wh.PathPrefix(), wh)
		s.log.Info("gateway: webhook mounted",
			"name", wh.Name(), "path", wh.PathPrefix())
	}
	if s.cfg.Prompts != nil {
		mux.HandleFunc("/v1/prompts/", s.handlePrompt)
	}
	if s.cfg.Plan != nil {
		mux.HandleFunc("/v1/plan", s.handlePlan)
	}

	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("rest: listen %q: %w", s.cfg.Addr, err)
	}
	s.mu.Lock()
	s.listener = ln
	s.httpSrv = &http.Server{
		Handler:      mux,
		ReadTimeout:  s.cfg.ReadTimeout,
		WriteTimeout: s.cfg.WriteTimeout,
		IdleTimeout:  s.cfg.IdleTimeout,
	}
	s.ready = true
	s.mu.Unlock()

	s.log.Info("rest server listening", "addr", ln.Addr().String(), "tls", s.cfg.TLSCert != "")

	// Telegram long-poll runs alongside the HTTP server in poll
	// mode. The loop exits cleanly when ctx is cancelled; failures
	// inside it log + keep retrying (bounded backoff), so we don't
	// propagate them to errCh — a flaky Telegram API shouldn't
	// take down the whole gateway.
	if s.cfg.Telegram != nil && s.cfg.Telegram.Mode() == TelegramModePoll {
		go func() {
			if err := s.cfg.Telegram.RunLongPoll(ctx); err != nil {
				s.log.Warn("telegram long-poll exited", "err", err)
			}
		}()
	}

	errCh := make(chan error, 1)
	go func() {
		var serveErr error
		if s.cfg.TLSCert != "" {
			serveErr = s.httpSrv.ServeTLS(ln, s.cfg.TLSCert, s.cfg.TLSKey)
		} else {
			serveErr = s.httpSrv.Serve(ln)
		}
		if !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.httpSrv.Shutdown(shutdownCtx)
		return nil
	}
}

// Addr returns the bound listener address — useful for tests that
// let the OS pick a port (":0"). Empty before Start() binds.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// ------------------------------------------------------------------
// Handlers
// ------------------------------------------------------------------

// messageRequest is the JSON body for POST /v1/messages. Minimal
// shape — channel handlers construct the full
// compute.ProcessMessageRequest server-side from this + config +
// any auth context.
type messageRequest struct {
	Message string `json:"message"`
	TurnID  string `json:"turn_id,omitempty"`
	Model   string `json:"model,omitempty"` // optional override
	// SessionID opts this request into a durable conversation: the
	// prior transcript is replayed into the turn and this turn is
	// appended to it. Omitting it keeps the legacy stateless
	// behaviour, where every request starts from a blank thread.
	//
	// Opt-in rather than defaulted-per-user because REST has no
	// natural conversation boundary — a script firing independent
	// one-shot requests under one token must not accumulate them
	// into a single ever-growing thread.
	SessionID string `json:"session_id,omitempty"`
}

// messageResponse is what we return. Mirrors the internal
// ProcessMessageResponse with enough fidelity for UIs to render
// tool-call history + confirmation prompts, but with only the
// fields a channel client needs.
type messageResponse struct {
	Reply              string         `json:"reply"`
	ToolCalls          []toolCallJSON `json:"tool_calls,omitempty"`
	NeedsConfirmation  bool           `json:"needs_confirmation,omitempty"`
	ConfirmationReason string         `json:"confirmation_reason,omitempty"`
	// PromptID is populated when NeedsConfirmation and Prompts is
	// configured — the client polls /v1/prompts/<id> and resolves
	// via POST /v1/prompts/<id>/resolve.
	PromptID string          `json:"prompt_id,omitempty"`
	Budget   budgetStateJSON `json:"budget,omitempty"`
	// TurnID is what `lobslaw trace <turn-id>` wants. Always set,
	// including when the server minted it.
	TurnID string `json:"turn_id,omitempty"`
}

type toolCallJSON struct {
	CallID   string `json:"call_id"`
	ToolName string `json:"tool_name"`
	Args     string `json:"args,omitempty"`
	Output   string `json:"output,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
}

type budgetStateJSON struct {
	ToolCalls   int     `json:"tool_calls"`
	SpendUSD    float64 `json:"spend_usd"`
	EgressBytes int64   `json:"egress_bytes"`
}

// restSessionID composes the stored conversation id from its owner and
// the client's chosen session_id.
//
// REST session ids are arbitrary client-supplied strings in one flat
// namespace, so without the owner component any authenticated caller
// who guesses another caller's session_id would have that transcript
// replayed into their turn — and would append to it. Composing the
// owner in makes one user's thread structurally unreachable from
// another's token, rather than depending on a check somewhere further
// down remembering to compare owners.
//
// An ownership check in the store would also be the wrong shape:
// Telegram deliberately shares one session across every member of a
// group chat (the ref's UserID is the sender, the channel id is the
// chat), so "requester must equal the session's creator" is not an
// invariant the store can enforce. Whether ids are per-user is a fact
// only the channel knows, and for REST they are.
//
// Unauthenticated deployments (RequireAuth=false) collapse to the
// single "anon" owner, which is the correct reading of a node that
// cannot tell its callers apart.
func restSessionID(userID, sessionID string) string {
	return escapeSessionComponent(userID) + "/" + sessionID
}

// escapeSessionComponent makes an identity safe to embed in a session
// id. ':' is the store's own separator and rejected outright there, so
// a JWT subject containing one would silently cost that caller durable
// history. '%' is escaped first so the mapping stays injective —
// without it the user ids "a%3Ab" and "a:b" would address the same
// conversation, reintroducing the leak this exists to close.
func escapeSessionComponent(s string) string {
	s = strings.ReplaceAll(s, "%", "%25")
	return strings.ReplaceAll(s, ":", "%3A")
}

// validateSessionID rejects the separators the composed id is built
// from. Failing the request beats the silent alternative: a ':' would
// reach the store, be rejected by its own key validation, and leave
// the caller with a stateless turn while believing they were holding a
// conversation.
func validateSessionID(id string) error {
	if strings.ContainsAny(id, ":/") {
		return errors.New("session_id must not contain ':' or '/'")
	}
	return nil
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.agent == nil {
		http.Error(w, "agent not configured on this node", http.StatusServiceUnavailable)
		return
	}

	// Cap body size to avoid clients streaming megabytes. The actual
	// useful message is usually under a few KB; 1MB covers rare long
	// copy-paste scenarios.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var req messageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.jsonErr(w, http.StatusBadRequest, "bad JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		s.jsonErr(w, http.StatusBadRequest, "message is required")
		return
	}
	// A turn with no id is a turn with no trace. The Telegram path
	// always mints one; this one passed through whatever the caller
	// sent, so a REST turn from a client that supplied none recorded
	// its spans under the empty id and `trace list` came back empty —
	// tracing looked switched OFF on a node where it was switched on.
	// Returned in the response too: an id the caller cannot see is an
	// id they cannot look the turn up by.
	if req.TurnID == "" {
		req.TurnID = "rest-" + ids.New()
	}

	budget, err := compute.NewTurnBudget(s.cfg.DefaultBudget)
	if err != nil {
		s.jsonErr(w, http.StatusInternalServerError, "budget construction: "+err.Error())
		return
	}

	claims, authErr := s.authenticate(r)
	if authErr != nil {
		s.jsonErr(w, http.StatusUnauthorized, authErr.Error())
		return
	}

	var sessionRef SessionRef
	var prior Transcript
	if req.SessionID != "" {
		if err := validateSessionID(req.SessionID); err != nil {
			s.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		sessionRef = SessionRef{
			Channel:   "rest",
			ChannelID: restSessionID(claims.UserID, req.SessionID),
			UserID:    claims.UserID,
		}
		// Serialise turns on this session for the same reason the
		// Telegram path does: Load → run → Append is not atomic, and
		// each request is its own goroutine. Only sessioned requests
		// need it — a session-less call has no transcript to corrupt.
		lease, disposition := s.gate.Acquire(r.Context(), cacheKey(sessionRef), req.TurnID, req.Message)
		switch disposition {
		case Folded:
			// Another in-flight turn absorbed this message and will
			// answer for it. Unlike a chat channel, an HTTP caller is
			// waiting on this response, so say so rather than hanging
			// up silently.
			s.jsonErr(w, http.StatusAccepted,
				"message folded into an in-flight turn on this session; its reply covers this message")
			return
		case Dropped:
			s.jsonErr(w, http.StatusConflict,
				"a turn is already running for this session")
			return
		}
		defer lease.Release()

		if len(lease.Batch) > 1 {
			req.Message = strings.Join(lease.Batch, "\n")
		}

		prior = s.conv.Load(r.Context(), sessionRef)
		s.log.Debug("rest: conversation history loaded",
			"turn_id", req.TurnID,
			"session_id", req.SessionID,
			"prior_messages", len(prior.Messages),
			"summarised", prior.Summary != "")
	}

	agentReq := compute.ProcessMessageRequest{
		Message:             req.Message,
		Claims:              claims,
		TurnID:              req.TurnID,
		Model:               req.Model,
		Budget:              budget,
		ConversationHistory: prior.Messages,
		ConversationSummary: prior.Summary,
		Channel:             sessionRef.Channel,
		ChannelID:           sessionRef.ChannelID,
	}

	// Responsiveness, shared with Telegram. The visible half needs an
	// SSE client; the hard timeout applies either way, and REST had
	// none — a stalled provider hung the request until the client gave
	// up.
	//
	// turnCtx, not r.Context(), for the rest of this handler: the
	// confirmation resume loop below re-enters the agent, and a turn
	// that stalls after approval should hit the same cap as one that
	// stalls before it.
	responder := newRESTResponder(w, r)
	turnCtx, stopGuards := startResponsiveness(r.Context(), responder, ResponsivenessConfig{
		TypingInterval: s.cfg.TypingInterval,
		InterimTimeout: s.cfg.InterimTimeout,
		HardTimeout:    s.cfg.HardTimeout,
		Soul:           s.cfg.Soul,
	})
	defer stopGuards()

	resp, err := s.agent.RunToolCallLoop(turnCtx, agentReq)
	if err != nil {
		s.log.Error("agent error", "turn_id", req.TurnID, "err", err)
		stopGuards()
		responder.Close()
		s.restError(w, responder, http.StatusInternalServerError, err.Error())
		return
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
		})
		if perr != nil {
			s.log.Warn("rest: prompt registration failed — returning confirmation as-is", "err", perr)
			break
		}
		lastPromptID = p.ID

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
		agentReq.Budget.Relax()
		resumed, rerr := s.agent.ResumeFromConfirmation(turnCtx, agentReq, resp.Messages)
		if rerr != nil {
			s.log.Error("rest: resume after approval failed",
				"turn_id", req.TurnID, "err", rerr)
			stopGuards()
			responder.Close()
			s.restError(w, responder, http.StatusInternalServerError, rerr.Error())
			return
		}
		// Preserve prior tool calls in the cumulative response.
		resumed.ToolCalls = append(resp.ToolCalls, resumed.ToolCalls...)
		resp = resumed
	}

	// Persisted after the confirmation loop so an approved-and-resumed
	// turn is recorded once, complete, rather than twice in halves.
	if req.SessionID != "" {
		if newTurn := newTurnMessages(resp.Messages, turnStart); len(newTurn) > 0 {
			s.conv.Append(r.Context(), sessionRef, req.TurnID, newTurn)
		}
	}

	out := messageResponse{
		// Appended after the transcript was persisted above, and to
		// the outbound text only. A notice recorded as an assistant
		// message is one the model reads next turn and reasons about.
		Reply: s.cfg.Notices.Append(r.Context(), "rest",
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
			CallID:   tc.CallID,
			ToolName: tc.ToolName,
			Args:     tc.Args,
			Output:   tc.Output,
			ExitCode: tc.ExitCode,
			Error:    tc.Error,
		})
	}

	// Timers off and the responder closed BEFORE the body is written,
	// so a typing tick that fires at exactly the wrong moment cannot
	// interleave itself into the response.
	stopGuards()
	responder.Close()

	if responder.Streaming() {
		_ = responder.sendFinal(out)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// restError writes an error in whichever shape the client is reading:
// an SSE event mid-stream, or an ordinary JSON body. Once headers are
// out for a stream, jsonErr's WriteHeader is a no-op and the client
// would be left waiting on a connection that never says why.
func (s *Server) restError(w http.ResponseWriter, responder *restResponder, code int, msg string) {
	if responder.Streaming() {
		_ = responder.sendError(msg)
		return
	}
	s.jsonErr(w, code, msg)
}

// handleHealthz returns 200 as long as the server is running.
// Does NOT check downstream — a misconfigured node that can't reach
// its LLM provider still reports healthy; readyz surfaces that.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// handleReadyz returns 200 when the node can accept messages
// (Agent constructed, server bound), 503 otherwise. Used by
// load balancers to decide whether to route traffic.
func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if !ready {
		http.Error(w, `{"status":"starting"}`, http.StatusServiceUnavailable)
		return
	}
	if s.agent == nil {
		http.Error(w, `{"status":"agent-not-configured"}`, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ready"}`))
}

// jsonErr writes a minimal JSON error body. Kept internal so all
// error responses share the same shape.
func (s *Server) jsonErr(w http.ResponseWriter, status int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": reason})
}

// authenticate extracts + validates the Authorization: Bearer JWT,
// if one is present, and returns a *types.Claims. Behaviour when
// no token or an invalid token is presented depends on RequireAuth:
//
//   - RequireAuth=false + no/invalid token → synthetic "anon" claims
//     with DefaultScope. Good for localhost / behind reverse proxy.
//   - RequireAuth=true  + no/invalid token → 401 error returned to
//     the caller via jsonErr. Good for internet-reachable deployments.
//
// When the validator itself is nil, RequireAuth is ignored (no way
// to validate) and anonymous is assumed. Operators who set
// RequireAuth without configuring a validator get a boot-time
// warning via Start's logs (Phase 6d.2 — JWKS wiring).
func (s *Server) authenticate(r *http.Request) (*types.Claims, error) {
	token := auth.ExtractBearer(r.Header.Get("Authorization"))

	if s.cfg.JWTValidator == nil {
		if s.cfg.RequireAuth {
			return nil, fmt.Errorf("auth required but no validator configured")
		}
		return anonClaims(s.cfg.DefaultScope), nil
	}
	if token == "" {
		if s.cfg.RequireAuth {
			return nil, fmt.Errorf("missing bearer token")
		}
		return anonClaims(s.cfg.DefaultScope), nil
	}

	claims, err := s.cfg.JWTValidator.Validate(token)
	if err != nil {
		if s.cfg.RequireAuth {
			return nil, fmt.Errorf("token validation failed: %w", err)
		}
		s.log.Warn("jwt validation failed; falling back to anon",
			"err", err, "remote", r.RemoteAddr)
		return anonClaims(s.cfg.DefaultScope), nil
	}
	if claims.Scope == "" {
		claims.Scope = s.cfg.DefaultScope
	}
	return claims, nil
}

// anonClaims builds the placeholder claims used for unauthenticated
// requests when RequireAuth is false. UserID is "anon" so audit
// logs show a distinct-from-real identity even without a JWT.
func anonClaims(scope string) *types.Claims {
	return &types.Claims{
		UserID: "anon",
		Scope:  scope,
	}
}

// handlePrompt serves two sub-routes under /v1/prompts/:
//
//	GET  /v1/prompts/<id>         — returns the prompt's current state
//	POST /v1/prompts/<id>/resolve — body {"approve": bool}; resolves
//
// Prompt state includes reason, decision, and timestamps — enough
// for a UI to render + render a decision and know when the prompt
// expires. Resolution is idempotent-on-conflict: a second attempt
// after the first (or after timeout) returns 409.
func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	// Parse path: /v1/prompts/<id>[/resolve]
	path := strings.TrimPrefix(r.URL.Path, "/v1/prompts/")
	if path == "" {
		s.jsonErr(w, http.StatusNotFound, "missing prompt id")
		return
	}
	var id, action string
	if idx := strings.Index(path, "/"); idx >= 0 {
		id = path[:idx]
		action = path[idx+1:]
	} else {
		id = path
	}

	switch {
	case action == "" && r.Method == http.MethodGet:
		s.handlePromptGet(w, r, id)
	case action == "resolve" && r.Method == http.MethodPost:
		s.handlePromptResolve(w, r, id)
	case action == "" && r.Method != http.MethodGet:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	case action == "resolve" && r.Method != http.MethodPost:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	default:
		s.jsonErr(w, http.StatusNotFound, "unknown prompt action")
	}
}

func (s *Server) handlePromptGet(w http.ResponseWriter, r *http.Request, id string) {
	p, err := s.cfg.Prompts.Get(id)
	if err != nil {
		if errors.Is(err, ErrPromptNotFound) {
			s.jsonErr(w, http.StatusNotFound, "prompt not found")
			return
		}
		s.jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(promptJSON{
		ID:        p.ID,
		TurnID:    p.TurnID,
		Reason:    p.Reason,
		Channel:   p.Channel,
		Decision:  p.Decision.String(),
		CreatedAt: p.CreatedAt,
		ExpiresAt: p.ExpiresAt,
	})
}

func (s *Server) handlePromptResolve(w http.ResponseWriter, r *http.Request, id string) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		Approve bool `json:"approve"`
		// Scope is "once" | "session" | "always". Absent or
		// unrecognised is "once" — a typo must narrow the grant,
		// never widen it.
		Scope string `json:"scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.jsonErr(w, http.StatusBadRequest, "bad JSON body: "+err.Error())
		return
	}
	decision := PromptDenied
	if body.Approve {
		decision = PromptApproved
	}
	scope := ParsePromptScope(body.Scope)
	if err := s.cfg.Prompts.Resolve(id, decision, scope); err != nil {
		switch {
		case errors.Is(err, ErrPromptNotFound):
			s.jsonErr(w, http.StatusNotFound, "prompt not found")
		case errors.Is(err, ErrPromptResolved):
			s.jsonErr(w, http.StatusConflict, "prompt already resolved")
		default:
			s.jsonErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"decision": decision.String(),
		"scope":    scope.String(),
	})
}

// handlePlan wraps PlanService.GetPlan. Accepts an optional
// ?window=<duration> query param (Go-duration syntax: "24h", "30m",
// "1h30m"); empty or invalid falls back to the service default.
func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req := &lobslawv1.GetPlanRequest{}
	if wq := r.URL.Query().Get("window"); wq != "" {
		if d, err := time.ParseDuration(wq); err == nil && d > 0 {
			req.Window = durationpb.New(d)
		}
	}
	resp, err := s.cfg.Plan.GetPlan(r.Context(), req)
	if err != nil {
		s.log.Error("plan: GetPlan failed", "err", err)
		s.jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := planResponseJSON{
		WindowSeconds: resp.Window.AsDuration().Seconds(),
	}
	for _, c := range resp.Commitments {
		out.Commitments = append(out.Commitments, commitmentJSON{
			ID:     c.Id,
			DueAt:  c.DueAt.AsTime(),
			Reason: c.Reason,
			Status: c.Status,
		})
	}
	for _, t := range resp.ScheduledTasks {
		entry := scheduledTaskJSON{
			ID:         t.Id,
			Name:       t.Name,
			Schedule:   t.Schedule,
			HandlerRef: t.HandlerRef,
		}
		if t.NextRun != nil {
			entry.NextRun = t.NextRun.AsTime()
		}
		out.ScheduledTasks = append(out.ScheduledTasks, entry)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// planResponseJSON mirrors the GetPlanResponse subset the REST API
// exposes. Kept narrow so the public JSON surface doesn't accidentally
// drag new proto fields into client expectations.
type planResponseJSON struct {
	WindowSeconds  float64             `json:"window_seconds"`
	Commitments    []commitmentJSON    `json:"commitments,omitempty"`
	ScheduledTasks []scheduledTaskJSON `json:"scheduled_tasks,omitempty"`
}

type commitmentJSON struct {
	ID     string    `json:"id"`
	DueAt  time.Time `json:"due_at"`
	Reason string    `json:"reason,omitempty"`
	Status string    `json:"status"`
}

type scheduledTaskJSON struct {
	ID         string    `json:"id"`
	Name       string    `json:"name,omitempty"`
	Schedule   string    `json:"schedule"`
	HandlerRef string    `json:"handler_ref"`
	NextRun    time.Time `json:"next_run,omitempty"`
}

// promptJSON is the on-the-wire shape for prompt state. Kept
// narrow so the user-visible API doesn't accidentally leak
// internal state.
type promptJSON struct {
	ID        string    `json:"id"`
	TurnID    string    `json:"turn_id,omitempty"`
	Reason    string    `json:"reason"`
	Channel   string    `json:"channel"`
	Decision  string    `json:"decision"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}
