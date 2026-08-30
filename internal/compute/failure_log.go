package compute

import (
	"log/slog"
)

// A provider failing mid-chain is usually weather — a 503, a rate
// limit — and the chain absorbing it is the system working. Logging
// all of those at the same level as a rejected credential is how a
// configuration fault goes unnoticed for weeks: it sits in a stream of
// identical warnings that everyone has learned to scroll past, while
// the operator pays a second provider to do the work the first one was
// configured for.
//
// So the level and the wording come from the class.

// LogProviderFailure records one failed attempt, at a level and with
// wording chosen by what actually went wrong.
//
// attrs are appended verbatim so each call site can add its own
// identifiers ("modality", "provider_index", "failed_label") without
// this having to know about either chain's shape.
func LogProviderFailure(log *slog.Logger, err error, attrs ...any) {
	if log == nil {
		log = slog.Default()
	}
	class := ClassifyFailure(err)
	attrs = append(attrs, "class", class.String(), "err", err)

	switch class {
	case FailureCredential:
		// Error, not warn. Nothing about this improves by waiting, the
		// chain is silently spending somebody else's quota to cover it,
		// and the fix is a one-line config change that will not happen
		// until a human sees this.
		log.Error("compute: provider rejected the credential; "+
			"check the API key for this provider — the chain is covering for it",
			attrs...)
	case FailureQuotaExhausted:
		// Also error: an operator whose plan ran out on the 3rd and has
		// been quietly failing over ever since will want to have known.
		log.Error("compute: provider is out of quota; "+
			"the chain is covering for it until the plan resets",
			attrs...)
	default:
		log.Warn("compute: provider failed; walking the chain", attrs...)
	}
}
