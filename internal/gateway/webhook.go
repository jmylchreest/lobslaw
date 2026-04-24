package gateway

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// WebhookConfig configures a generic inbound-webhook channel. Enables
// integrations with Zapier, IFTTT, n8n, GitHub Actions, cron.run, or
// any service that can POST JSON to a URL. Each POST becomes one
// agent turn; the reply is returned in the HTTP response body.
//
// Auth is shared-secret via Authorization: Bearer <secret>. Operators
// rotate the secret by redeploying — no token renewal dance.
type WebhookConfig struct {
	// Name is the webhook's logical name; part of the default
	// mount path (/webhook/<Name>). Keep short and URL-safe.
	Name string

	// Path is the full URL path. Empty → "/webhook/<Name>". Operators
	// with path conventions (e.g. "/hooks/inbound/foo") can override.
	Path string

	// SharedSecret is compared to the inbound Bearer token. Empty
	// means the webhook is unauthenticated — refused at wire-up
	// unless explicitly allowed.
	SharedSecret string

	// Scope is the lobslaw security scope applied to dispatched
	// turns. The inbound caller is presumed trusted at this level
	// once the shared secret matches. Empty defaults to "webhook".
	Scope string

	// DefaultBudget applies per request. Same shape as other channels.
	DefaultBudget compute.BudgetCaps

	// Logger — nil → slog.Default().
	Logger *slog.Logger
}

// WebhookHandler is an http.Handler serving one webhook channel.
// Stateless across requests.
type WebhookHandler struct {
	cfg   WebhookConfig
	agent *compute.Agent
	log   *slog.Logger
}

// NewWebhookHandler wires a channel + agent. Refuses empty shared
// secrets — operators who genuinely want an unauthenticated webhook
// must surface that decision elsewhere (e.g. by front-ending with
// nginx auth).
func NewWebhookHandler(cfg WebhookConfig, agent *compute.Agent) (*WebhookHandler, error) {
	if cfg.SharedSecret == "" {
		return nil, errWebhookNoSecret
	}
	if cfg.Scope == "" {
		cfg.Scope = "webhook"
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &WebhookHandler{cfg: cfg, agent: agent, log: logger}, nil
}

// PathPrefix returns the mount path the operator requested (or the
// default derivation from Name). Used by the gateway router to mount
// this handler.
func (h *WebhookHandler) PathPrefix() string {
	if h.cfg.Path != "" {
		return h.cfg.Path
	}
	return "/webhook/" + h.cfg.Name
}

// Name returns the configured logical name (for log attribution).
func (h *WebhookHandler) Name() string { return h.cfg.Name }

// ServeHTTP handles one inbound webhook request. Accepted shapes:
//
//	POST /webhook/<name>
//	  Authorization: Bearer <shared_secret>
//	  Content-Type: application/json
//	  {"prompt": "...", "metadata": {...}}
//
//	POST /webhook/<name>
//	  Authorization: Bearer <shared_secret>
//	  Content-Type: text/plain
//	  <raw text used as the prompt>
//
// Response on success:
//
//	200 OK
//	{"reply": "...", "turn_id": "..."}
//
// Errors surface as 4xx (bad auth / input) or 500 (agent failure).
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.checkAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	prompt, err := h.extractPrompt(r)
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if prompt == "" {
		http.Error(w, "empty prompt", http.StatusBadRequest)
		return
	}

	budget, err := compute.NewTurnBudget(h.cfg.DefaultBudget)
	if err != nil {
		h.log.Error("webhook: budget init failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	claims := &types.Claims{
		UserID: "webhook:" + h.cfg.Name,
		Scope:  h.cfg.Scope,
	}
	turnID := "webhook-" + time.Now().UTC().Format("20060102T150405.000")

	resp, err := h.agent.RunToolCallLoop(r.Context(), compute.ProcessMessageRequest{
		Message: prompt,
		Claims:  claims,
		TurnID:  turnID,
		Budget:  budget,
	})
	if err != nil {
		h.log.Error("webhook: agent error", "turn_id", turnID, "err", err)
		http.Error(w, "agent error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	out := map[string]any{
		"reply":   resp.Reply,
		"turn_id": turnID,
	}
	if resp.NeedsConfirmation {
		out["needs_confirmation"] = true
		out["confirmation_reason"] = resp.ConfirmationReason
	}
	_ = json.NewEncoder(w).Encode(out)
}

// checkAuth does a constant-time comparison against the configured
// shared secret. Returns false if the header is missing/malformed.
func (h *WebhookHandler) checkAuth(r *http.Request) bool {
	raw := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(raw, prefix) {
		return false
	}
	got := strings.TrimPrefix(raw, prefix)
	return subtle.ConstantTimeCompare([]byte(got), []byte(h.cfg.SharedSecret)) == 1
}

// extractPrompt pulls the prompt text from either a JSON body
// ({"prompt": "..."}) or a plain-text body. JSON takes precedence
// when Content-Type is application/json.
func (h *WebhookHandler) extractPrompt(r *http.Request) (string, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		return "", err
	}
	ctype := r.Header.Get("Content-Type")
	if strings.Contains(ctype, "application/json") {
		var payload struct {
			Prompt string `json:"prompt"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return "", err
		}
		return strings.TrimSpace(payload.Prompt), nil
	}
	return strings.TrimSpace(string(body)), nil
}

var errWebhookNoSecret = &webhookErr{"webhook: shared_secret required (empty → refusing to run an unauthenticated webhook)"}

type webhookErr struct{ msg string }

func (e *webhookErr) Error() string { return e.msg }
