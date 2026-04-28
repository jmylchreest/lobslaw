package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// DefaultTTL is the expiry window applied to notifications that
// don't carry an explicit ExpiresAt. Picked at 5 minutes per the
// design constraint: stale commitment-fire messages shouldn't
// reach the user hours after the bot recovered from an outage.
const DefaultTTL = 5 * time.Minute

// Urgency tags affect default TTL and may later affect routing
// (which sinks get the message in priority order). Today they're
// metadata only — the broadcast strategy delivers to all sinks
// regardless of urgency.
type Urgency string

const (
	UrgencyLow    Urgency = "low"
	UrgencyNormal Urgency = "normal"
	UrgencyHigh   Urgency = "high"
)

// Notification is one outbound message to a user. The payload is
// channel-agnostic plaintext; sinks render it however their
// channel demands.
type Notification struct {
	UserID            string
	Body              string
	Urgency           Urgency
	ExpiresAt         time.Time
	OriginatorChannel string
	OriginatorID      string
	Reason            string
}

// Sink is one channel's delivery adapter. Each gateway channel
// (telegram, REST, future Slack/Matrix) registers a Sink at boot.
type Sink interface {
	ChannelType() string
	Deliver(ctx context.Context, address, body string) error
}

// PrefsLookup is the subset of memory.UserPrefsService the notify
// service needs. Interface so tests can substitute a fake.
type PrefsLookup interface {
	Get(ctx context.Context, userID string) (*lobslawv1.UserPreferences, error)
}

// Service dispatches Notifications across registered Sinks. One
// Service per node; multi-node clusters each run their own and
// the agent calls into whichever is local.
type Service struct {
	prefs  PrefsLookup
	logger *slog.Logger

	mu    sync.RWMutex
	sinks map[string]Sink
}

// NewService constructs a Service. prefs may be nil for test setups
// that pre-populate addresses out-of-band; production wires the
// memory.UserPrefsService here.
func NewService(prefs PrefsLookup, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		prefs:  prefs,
		logger: logger,
		sinks:  make(map[string]Sink),
	}
}

// RegisterSink installs a sink. Per channel type — registering a
// second sink for the same type replaces the first (the gateway
// channel layer guarantees one handler per channel anyway). Fails
// on empty ChannelType so a misconfigured handler crashes loudly
// at boot rather than silently dropping notifications.
func (s *Service) RegisterSink(sink Sink) error {
	if sink == nil {
		return errors.New("notify: nil sink")
	}
	t := strings.TrimSpace(sink.ChannelType())
	if t == "" {
		return errors.New("notify: sink has empty ChannelType")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sinks[t] = sink
	return nil
}

// ErrExpired surfaces when a notification's ExpiresAt is in the
// past at the moment Send is called. Callers can branch on this
// for retry vs. drop logic.
var ErrExpired = errors.New("notify: notification expired before delivery")

// ErrUserUnbound surfaces when prefs has no record for the target
// user_id, OR has a record without channel addresses for any
// registered sink. Different from delivery failure (where a sink
// returned an error mid-deliver).
var ErrUserUnbound = errors.New("notify: user has no reachable channel addresses")

// Send dispatches n. Behaviour:
//
//   - Expired ⇒ return ErrExpired without touching any sink.
//   - OriginatorChannel set ⇒ deliver only on that channel using
//     the user's bound address for that channel type.
//   - OriginatorChannel empty ⇒ broadcast: deliver on every
//     channel-address pair the user has bound that has a
//     registered sink.
//
// Per-sink failures are logged and continue (one bad channel
// shouldn't block the rest). Returns nil iff at least one sink
// successfully delivered, OR ErrUserUnbound when no sink could
// be matched, OR ErrExpired.
func (s *Service) Send(ctx context.Context, n Notification) error {
	if strings.TrimSpace(n.UserID) == "" {
		return errors.New("notify: user_id required")
	}
	if strings.TrimSpace(n.Body) == "" {
		return errors.New("notify: body required")
	}
	if n.ExpiresAt.IsZero() {
		n.ExpiresAt = time.Now().Add(DefaultTTL)
	}
	if time.Now().After(n.ExpiresAt) {
		s.logger.Warn("notify: dropping expired notification",
			"user", n.UserID, "expires_at", n.ExpiresAt, "reason", n.Reason)
		return ErrExpired
	}

	prefs, err := s.lookupPrefs(ctx, n.UserID)
	if err != nil {
		return err
	}

	if n.OriginatorChannel != "" {
		return s.deliverOriginator(ctx, n, prefs)
	}
	return s.broadcast(ctx, n, prefs)
}

func (s *Service) lookupPrefs(ctx context.Context, userID string) (*lobslawv1.UserPreferences, error) {
	if s.prefs == nil {
		return nil, errors.New("notify: prefs lookup not wired")
	}
	prefs, err := s.prefs.Get(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("notify: lookup prefs for %s: %w", userID, err)
	}
	return prefs, nil
}

// deliverOriginator handles the inbound-reply path: deliver only
// on the originating channel using the address bound for that
// channel type. Failures here are real errors (the user is
// expecting a reply on this exact channel).
func (s *Service) deliverOriginator(ctx context.Context, n Notification, prefs *lobslawv1.UserPreferences) error {
	addr := findChannelAddress(prefs, n.OriginatorChannel)
	if addr == "" {
		// Originator delivery falls back to OriginatorID — the
		// channel-level identifier from the inbound message.
		// This handles the case where prefs hasn't been
		// populated yet but we still have a live channel context.
		addr = n.OriginatorID
	}
	if addr == "" {
		return fmt.Errorf("%w (originator channel %q has no bound address)",
			ErrUserUnbound, n.OriginatorChannel)
	}
	sink := s.sinkFor(n.OriginatorChannel)
	if sink == nil {
		return fmt.Errorf("notify: no sink registered for channel %q", n.OriginatorChannel)
	}
	return sink.Deliver(ctx, addr, n.Body)
}

// broadcast handles the self-generated path: deliver on every
// (channel, address) pair in prefs that has a registered sink.
// Continues past per-sink errors — partial delivery is better
// than zero delivery. Returns ErrUserUnbound iff no sinks
// matched any of the user's bindings.
func (s *Service) broadcast(ctx context.Context, n Notification, prefs *lobslawv1.UserPreferences) error {
	if prefs == nil || len(prefs.Channels) == 0 {
		return ErrUserUnbound
	}
	delivered := 0
	for _, c := range prefs.Channels {
		sink := s.sinkFor(c.Type)
		if sink == nil {
			s.logger.Debug("notify: no sink for channel type; skipping",
				"user", n.UserID, "channel", c.Type)
			continue
		}
		if err := sink.Deliver(ctx, c.Address, n.Body); err != nil {
			s.logger.Warn("notify: sink delivery failed",
				"user", n.UserID, "channel", c.Type, "err", err)
			continue
		}
		delivered++
	}
	if delivered == 0 {
		return ErrUserUnbound
	}
	return nil
}

func (s *Service) sinkFor(channelType string) Sink {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sinks[channelType]
}

// findChannelAddress returns the prefs address bound for the given
// channel type, or "" when not found.
func findChannelAddress(prefs *lobslawv1.UserPreferences, channelType string) string {
	if prefs == nil {
		return ""
	}
	for _, c := range prefs.Channels {
		if c.Type == channelType {
			return c.Address
		}
	}
	return ""
}
