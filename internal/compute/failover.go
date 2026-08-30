package compute

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// FailoverHandler pairs a handler with the provider label its health
// is tracked under. The label is what makes a demotion shared: a
// provider that failed for the image modality is the same endpoint the
// speak modality would reach, and rediscovering that separately per
// modality is the waste this exists to avoid.
type FailoverHandler struct {
	Label string
	// Tier is the provider's declared trust tier, checked against the
	// soul floor before the handler runs. A modality provider is not a
	// lesser recipient of content — a vision provider is handed the
	// user's image, and a speak provider the text of the reply.
	Tier types.TrustTier
	Fn   BuiltinFunc
}

// FailoverBuiltin turns an ordered list of handlers for one modality
// into a single handler that walks them.
//
// The ordering is not invented here. Operators already express it —
// [[compute.providers]] entries carry capability tags and a priority,
// and SelectByCapability has always returned them sorted, with a
// comment saying the order existed "for the future fallback-chain
// layer". This is that layer; the resolver simply stopped at the first
// match and threw the rest away.
//
// The advance decision is isRetryableProviderError, the same predicate
// the chat backup chain uses. That is deliberate: a second failover
// policy would drift from the first, and the point of the driver waist
// is that "should I try the next provider" has one answer per failure
// class no matter which modality asked.
//
// Argument errors need no special case. A bad path or a missing
// required field fails before any HTTP call, is unclassified, and so
// classifies permanent — the chain stops on the first handler rather
// than re-validating the same bad input against every provider.
func FailoverBuiltin(modality string, log *slog.Logger, health *ProviderHealth, floor func() types.TrustTier, handlers ...FailoverHandler) BuiltinFunc {
	if log == nil {
		log = slog.Default()
	}
	// The single-provider case gets the wrapper too: the floor has to
	// be checked whether or not there is anywhere to fall through to,
	// and "one provider" is exactly the config where an unchecked one
	// is the only thing that runs.
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		var (
			lastOut    []byte
			lastCode   int
			lastErr    error
			skipped    int
			belowFloor int
			considered []TrustCandidate
		)
		want := trustFloorOf(floor)
		for i, h := range handlers {
			considered = append(considered, TrustCandidate{Label: h.Label, Tier: h.Tier})
			if !MeetsFloor(want, h.Tier) {
				belowFloor++
				log.Warn("compute: provider excluded by the trust floor",
					"modality", modality, "label", h.Label,
					"trust_tier", h.Tier, "min_trust_tier", want)
				continue
			}
			if !health.Available(h.Label) {
				skipped++
				log.Debug("compute: skipping demoted provider",
					"modality", modality, "label", h.Label,
					"cooldown_remaining", health.CooldownRemaining(h.Label))
				continue
			}
			out, code, err := h.Fn(ctx, args)
			if err == nil {
				health.RecordSuccess(h.Label)
				if i > 0 || skipped > 0 {
					log.Info("compute: modality backup succeeded",
						"modality", modality, "provider_index", i,
						"skipped_demoted", skipped,
						"prior_error", ErrorText(lastErr))
				}
				return out, code, nil
			}
			if !IsRetryableProviderError(ctx, err) {
				return out, code, err
			}
			health.RecordFailure(h.Label, ClassifyFailure(err))
			LogProviderFailure(log, err, "modality", modality,
				"provider_index", i, "label", h.Label)
			lastOut, lastCode, lastErr = out, code, err
		}
		if lastErr == nil && belowFloor > 0 && belowFloor+skipped == len(handlers) {
			return nil, 1, fmt.Errorf("%s: %w", modality,
				&ErrBelowTrustFloor{Floor: want, Considered: considered})
		}
		if lastErr == nil {
			return nil, 1, fmt.Errorf(
				"%s: every provider is in cooldown (%d demoted); "+
					"check the logs for credential or quota errors", modality, skipped)
		}
		// A lone provider's error is its own. "all 1 providers in the
		// chain failed" reads as an outage across a chain that does not
		// exist, and sends an operator looking for the other providers.
		//
		// This used to be handled by returning the bare handler when
		// there was only one — which also skipped the trust-floor
		// check, in exactly the config where the unchecked provider is
		// the only thing that runs. The wrapper now always applies and
		// the MESSAGE is what varies.
		if len(handlers) == 1 {
			return lastOut, lastCode, fmt.Errorf("%s: %w", modality, lastErr)
		}
		// Every provider was tried and every one failed retryably. The
		// last error is the most recent evidence, and the count tells an
		// operator this was a chain-wide outage rather than one endpoint
		// having a bad minute.
		return lastOut, lastCode, fmt.Errorf(
			"%s: all %d providers in the chain failed; last error: %w",
			modality, len(handlers), lastErr)
	}
}

// trustFloorOf tolerates a nil accessor, which is what a caller that
// has no soul wired passes — a test, or a node with no SOUL.md. Empty
// permits everything, because an operator who has not opted in has not
// asked for a restriction.
func trustFloorOf(floor func() types.TrustTier) types.TrustTier {
	if floor == nil {
		return TrustUnsetFloor
	}
	return floor()
}
