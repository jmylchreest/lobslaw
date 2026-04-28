package oauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"crypto/rand"
)

// flowIDEntropy backs Tracker's flow-ID generation. ULIDs are
// sortable by time so introspecting active flows shows them in
// initiation order.
var flowIDEntropy = ulid.Monotonic(rand.Reader, 0)

// Flow is one in-progress device-code authorisation. The tracker
// keeps a Flow in memory until the polling goroutine resolves
// (success, expiry, denial, or operator cancellation). Successful
// flows trigger OnComplete so the caller can persist the resulting
// tokens via the CredentialService.
//
// Not raft-replicated: device-code flows are short-lived (typically
// 30 min cap) and pinned to the node that initiated them. A node
// restart loses any in-progress flows; the operator restarts the
// oauth_start command. This is simpler than persisting flow state
// across crashes and matches how the IdP's expires_in is set up.
type Flow struct {
	ID            string
	Provider      ProviderConfig
	Scopes        []string
	InitiatedBy   string // "scope:owner" or wherever the request came from
	StartedAt     time.Time
	ExpiresAt     time.Time
	UserCode      string
	VerificationURI string

	// Outcome is set to one of "pending", "complete", "expired",
	// "denied", "cancelled", "error". oauth_status surfaces it.
	mu      sync.RWMutex
	outcome string
	subject string // populated on success
	err     error
}

// FlowSnapshot is a copy-safe view of a Flow's current state for
// returning to oauth_status.
type FlowSnapshot struct {
	ID              string
	Provider        string
	Scopes          []string
	StartedAt       time.Time
	ExpiresAt       time.Time
	UserCode        string
	VerificationURI string
	Outcome         string
	Subject         string
	Error           string
}

// Snapshot returns a copy of the flow's current state. Safe to call
// from any goroutine; never returns the Flow's mutex.
func (f *Flow) Snapshot() FlowSnapshot {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := FlowSnapshot{
		ID:              f.ID,
		Provider:        f.Provider.Name,
		Scopes:          append([]string(nil), f.Scopes...),
		StartedAt:       f.StartedAt,
		ExpiresAt:       f.ExpiresAt,
		UserCode:        f.UserCode,
		VerificationURI: f.VerificationURI,
		Outcome:         f.outcome,
		Subject:         f.subject,
	}
	if f.err != nil {
		out.Error = f.err.Error()
	}
	return out
}

// CompleteCallback is fired exactly once when a flow succeeds.
// Carries the IdP's TokenResponse + the subject the flow
// authenticated as. Implementations persist via the credential
// service; failures are returned so the tracker can mark the flow
// as "error" rather than "complete".
type CompleteCallback func(ctx context.Context, flow *Flow, tok *TokenResponse) error

// Tracker holds in-progress flows + the polling goroutines that
// drive them. New flows are added via Start; oauth_status reads via
// List. Operator cancellation is via Cancel. Tracker has no raft
// state — restart loses flows, by design.
type Tracker struct {
	logger *slog.Logger

	mu     sync.RWMutex
	flows  map[string]*Flow         // flow ID → flow
	cancel map[string]context.CancelFunc // flow ID → background-poll cancel
}

// NewTracker constructs an empty tracker. logger is used for
// background-poll diagnostics; nil → slog.Default.
func NewTracker(logger *slog.Logger) *Tracker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Tracker{
		logger: logger,
		flows:  make(map[string]*Flow),
		cancel: make(map[string]context.CancelFunc),
	}
}

// Start initiates a new device-code flow. Returns a Flow describing
// the user_code + verification URI. A background goroutine polls
// the IdP until success / expiry / cancellation; on success the
// onComplete callback persists the credential.
//
// initiatedBy is recorded for the audit log (e.g. "scope:owner" for
// flows started by the operator via Telegram).
func (t *Tracker) Start(ctx context.Context, p ProviderConfig, scopes []string, initiatedBy string, onComplete CompleteCallback) (*Flow, error) {
	if onComplete == nil {
		return nil, errors.New("oauth tracker: onComplete required")
	}
	da, err := StartDeviceAuth(ctx, p, scopes)
	if err != nil {
		return nil, fmt.Errorf("oauth tracker: device auth: %w", err)
	}
	flow := &Flow{
		ID:              ulid.MustNew(ulid.Now(), flowIDEntropy).String(),
		Provider:        p,
		Scopes:          append([]string(nil), scopes...),
		InitiatedBy:     initiatedBy,
		StartedAt:       time.Now(),
		ExpiresAt:       time.Now().Add(time.Duration(da.ExpiresIn) * time.Second),
		UserCode:        da.UserCode,
		VerificationURI: da.VerificationLink(),
		outcome:         "pending",
	}

	pollCtx, cancel := context.WithCancel(context.Background())
	t.mu.Lock()
	t.flows[flow.ID] = flow
	t.cancel[flow.ID] = cancel
	t.mu.Unlock()

	go t.pollLoop(pollCtx, flow, da, onComplete)
	return flow, nil
}

// pollLoop drives the device-code flow to completion. Runs in a
// background goroutine for the lifetime of the flow.
func (t *Tracker) pollLoop(ctx context.Context, flow *Flow, da *DeviceAuthResponse, onComplete CompleteCallback) {
	defer t.removeCancel(flow.ID)
	tok, err := PollUntilGrant(ctx, flow.Provider, da)
	if err != nil {
		t.markOutcome(flow, classifyError(err), err)
		t.logger.Info("oauth flow ended",
			"flow", flow.ID,
			"provider", flow.Provider.Name,
			"outcome", flow.Snapshot().Outcome,
			"err", err,
		)
		return
	}
	// Success path — invoke the callback to persist the credential.
	persistCtx, persistCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer persistCancel()
	if err := onComplete(persistCtx, flow, tok); err != nil {
		t.markOutcome(flow, "error", fmt.Errorf("persist: %w", err))
		t.logger.Warn("oauth flow persist failed",
			"flow", flow.ID, "provider", flow.Provider.Name, "err", err)
		return
	}
	flow.mu.Lock()
	flow.outcome = "complete"
	if tok.Scope != "" {
		// Some providers narrow the granted scopes from what was
		// requested; record the actual set on success for the
		// audit trail.
		flow.Scopes = strings.Fields(strings.ReplaceAll(tok.Scope, ",", " "))
	}
	flow.mu.Unlock()
	t.logger.Info("oauth flow complete",
		"flow", flow.ID,
		"provider", flow.Provider.Name,
		"subject", flow.subject,
	)
}

// SetSubject is called by onComplete callbacks once they've extracted
// the authenticated subject (email, login, etc) from the token's
// id_token or /userinfo lookup. Stored on the flow for oauth_status.
func (t *Tracker) SetSubject(flowID, subject string) {
	t.mu.RLock()
	flow := t.flows[flowID]
	t.mu.RUnlock()
	if flow == nil {
		return
	}
	flow.mu.Lock()
	flow.subject = subject
	flow.mu.Unlock()
}

// Cancel stops a pending flow. Safe to call on a flow that's
// already terminal — no-op in that case.
func (t *Tracker) Cancel(flowID string) bool {
	t.mu.Lock()
	cancel, ok := t.cancel[flowID]
	delete(t.cancel, flowID)
	t.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	if flow := t.Get(flowID); flow != nil {
		t.markOutcome(flow, "cancelled", errors.New("operator cancelled"))
	}
	return true
}

// Get returns the named flow or nil.
func (t *Tracker) Get(flowID string) *Flow {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.flows[flowID]
}

// List returns snapshots of all tracked flows, newest first.
func (t *Tracker) List() []FlowSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]FlowSnapshot, 0, len(t.flows))
	for _, f := range t.flows {
		out = append(out, f.Snapshot())
	}
	// Newest first — flow IDs are ULIDs which sort by time so a
	// reverse string sort reads as "newest first."
	for i := 0; i < len(out)/2; i++ {
		out[i], out[len(out)-1-i] = out[len(out)-1-i], out[i]
	}
	return out
}

// Forget evicts a finished flow from the tracker. Called periodically
// or by oauth_status when the operator wants to clear the history.
func (t *Tracker) Forget(flowID string) {
	t.mu.Lock()
	delete(t.flows, flowID)
	delete(t.cancel, flowID)
	t.mu.Unlock()
}

func (t *Tracker) markOutcome(flow *Flow, outcome string, err error) {
	flow.mu.Lock()
	defer flow.mu.Unlock()
	flow.outcome = outcome
	flow.err = err
}

func (t *Tracker) removeCancel(flowID string) {
	t.mu.Lock()
	delete(t.cancel, flowID)
	t.mu.Unlock()
}

// classifyError maps PollUntilGrant errors to outcome strings used
// in FlowSnapshot.Outcome. Anything not recognised lands as
// "error" with the underlying err preserved.
func classifyError(err error) string {
	switch {
	case errors.Is(err, ErrExpiredToken):
		return "expired"
	case errors.Is(err, ErrAccessDenied):
		return "denied"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	default:
		return "error"
	}
}
