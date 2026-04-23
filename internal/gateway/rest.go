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

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/auth"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// RESTConfig tunes the REST channel.
type RESTConfig struct {
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

	// Prompts is the confirmation-prompt registry. When set, the
	// REST server mounts /v1/prompts/{id} endpoints. Agents that
	// return NeedsConfirmation register a prompt here; UIs poll
	// and resolve. Nil = no prompt flow (NeedsConfirmation returns
	// as plain text like Phase 6e).
	Prompts *PromptRegistry

	// ConfirmationTTL is how long a pending prompt waits before
	// auto-denying on timeout. 0 → 5 minutes default.
	ConfirmationTTL time.Duration

	// Logger is used for structured log output. Nil → slog.Default().
	Logger *slog.Logger
}

// Server is the REST channel handler. Stateful only for lifecycle
// bookkeeping (net.Listener, underlying http.Server).
type Server struct {
	cfg   RESTConfig
	agent *compute.Agent
	log   *slog.Logger

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
	return &Server{cfg: cfg, agent: agent, log: cfg.Logger}
}

// Start binds the listener and serves. Blocks until ctx is
// cancelled or the HTTP server returns an error. A cancelled ctx
// triggers a graceful shutdown with a bounded timeout.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", s.handleMessages)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	if s.cfg.Telegram != nil {
		mux.Handle("/telegram", s.cfg.Telegram)
	}
	if s.cfg.Prompts != nil {
		mux.HandleFunc("/v1/prompts/", s.handlePrompt)
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
}

// messageResponse is what we return. Mirrors the internal
// ProcessMessageResponse with enough fidelity for UIs to render
// tool-call history + confirmation prompts, but with only the
// fields a channel client needs.
type messageResponse struct {
	Reply              string              `json:"reply"`
	ToolCalls          []toolCallJSON      `json:"tool_calls,omitempty"`
	NeedsConfirmation  bool                `json:"needs_confirmation,omitempty"`
	ConfirmationReason string              `json:"confirmation_reason,omitempty"`
	// PromptID is populated when NeedsConfirmation and Prompts is
	// configured — the client polls /v1/prompts/<id> and resolves
	// via POST /v1/prompts/<id>/resolve.
	PromptID           string              `json:"prompt_id,omitempty"`
	Budget             budgetStateJSON     `json:"budget,omitempty"`
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

	agentReq := compute.ProcessMessageRequest{
		Message: req.Message,
		Claims:  claims,
		TurnID:  req.TurnID,
		Model:   req.Model,
		Budget:  budget,
	}

	resp, err := s.agent.RunToolCallLoop(r.Context(), agentReq)
	if err != nil {
		s.log.Error("agent error", "turn_id", req.TurnID, "err", err)
		s.jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := messageResponse{
		Reply:              resp.Reply,
		NeedsConfirmation:  resp.NeedsConfirmation,
		ConfirmationReason: resp.ConfirmationReason,
		Budget: budgetStateJSON{
			ToolCalls:   resp.BudgetState.ToolCalls,
			SpendUSD:    resp.BudgetState.SpendUSD,
			EgressBytes: resp.BudgetState.EgressBytes,
		},
	}
	if resp.NeedsConfirmation && s.cfg.Prompts != nil {
		ttl := s.cfg.ConfirmationTTL
		if ttl <= 0 {
			ttl = 5 * time.Minute
		}
		if p, err := s.cfg.Prompts.Create(req.TurnID, resp.ConfirmationReason, "rest", ttl); err == nil {
			out.PromptID = p.ID
		} else {
			s.log.Warn("rest: prompt registration failed", "err", err)
		}
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

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
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
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.jsonErr(w, http.StatusBadRequest, "bad JSON body: "+err.Error())
		return
	}
	decision := PromptDenied
	if body.Approve {
		decision = PromptApproved
	}
	if err := s.cfg.Prompts.Resolve(id, decision); err != nil {
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
	_ = json.NewEncoder(w).Encode(map[string]string{"decision": decision.String()})
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
