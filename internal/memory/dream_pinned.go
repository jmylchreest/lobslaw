package memory

import (
	"context"
	"fmt"
	"strings"
)

// Dream acting on the pinned-memory threshold.
//
// NeedsConsolidation has existed as a signal with nothing reading it.
// Its own doc says why it fires early: "the point is to consolidate
// BEFORE a write fails, so the pressure produces curation in the
// background rather than an error the user sees." Nothing did the
// curating, so the pressure produced the error instead.
//
// This is background curation, not an approval flow. Pinned memory is
// small and capped by design — it is a fixed tax on every request —
// and the alternative to tidying it is a user told their notes are
// full, which is the thing the threshold exists to avoid.
//
// It follows the merge phase's shape exactly: propose to the
// Summarizer, act only on a result that is demonstrably safe, and
// default to doing nothing when no Summarizer is wired. A Dream pass
// on a node with no LLM must not quietly rewrite the one memory the
// user authored by hand.

// PinnedConsolidation reports what the pass did.
type PinnedConsolidation struct {
	// Considered is the number of (kind, owner) blocks over threshold.
	Considered int
	// Consolidated is the number actually rewritten.
	Consolidated int
	// Refused counts proposals rejected by the safety checks below —
	// separately from blocks nobody proposed anything for, because the
	// two say different things about whether this is working.
	Refused int
}

// consolidatePinned tidies pinned blocks that are near their cap.
//
// Returns zero values and no error when there is no Summarizer or no
// PinnedStore: a soft skip, like the rest of Dream.
func (d *DreamRunner) consolidatePinned(ctx context.Context, pinned *PinnedStore) (PinnedConsolidation, error) {
	var out PinnedConsolidation
	if d.summarizer == nil || pinned == nil {
		return out, nil
	}

	owners, err := pinnedOwners(d.store)
	if err != nil {
		return out, fmt.Errorf("pinned owners: %w", err)
	}

	for _, o := range owners {
		need, err := pinned.NeedsConsolidation(o.kind, o.owner)
		if err != nil {
			return out, fmt.Errorf("pinned threshold: %w", err)
		}

		if !need {
			continue
		}
		out.Considered++

		block, err := pinned.Get(o.kind, o.owner)
		if err != nil || block == nil || len(block.GetEntries()) < 2 {
			// Nothing to merge. A single entry at the cap is a long
			// entry, and shortening it is editing what the user wrote
			// rather than deduplicating what they wrote twice.
			continue
		}
		before := block.GetEntries()

		summary, _, err := d.summarizer.Summarize(ctx, before)
		if err != nil {
			// A summariser outage is not a reason to fail the whole
			// Dream pass. The block stays as it is and the threshold
			// fires again tomorrow.
			d.logger.Warn("dream: pinned consolidation unavailable",
				"kind", o.kind, "owner", o.owner, "err", err)
			continue
		}

		after := splitPinnedSummary(summary)
		if reason := refusePinnedConsolidation(before, after, pinned.Cap(o.kind)); reason != "" {
			out.Refused++
			// ERROR, not warn. A refused consolidation means the block
			// is still near its cap AND the thing meant to fix it
			// produced something unusable — the user is heading for
			// the write failure this exists to prevent.
			d.logger.Error("dream: refusing a pinned consolidation",
				"kind", o.kind, "owner", o.owner, "reason", reason,
				"entries_before", len(before), "entries_after", len(after))
			continue
		}

		if err := pinned.ReplaceAll(ctx, o.kind, o.owner, after); err != nil {
			return out, fmt.Errorf("pinned rewrite: %w", err)
		}
		out.Consolidated++
		d.logger.Info("dream: pinned memory consolidated",
			"kind", o.kind, "owner", o.owner,
			"entries_before", len(before), "entries_after", len(after))
	}
	return out, nil
}

// refusePinnedConsolidation returns a reason to refuse, or empty to
// proceed.
//
// The whole safety of doing this unattended lives here. A summariser
// asked to compact somebody's notes can return anything, and what it
// returns replaces memory the user wrote by hand — which no retrieval
// pass can reconstruct.
func refusePinnedConsolidation(before, after []string, cap int) string {
	switch {
	case len(after) == 0:
		// The catastrophic case. Losing every pinned entry is not
		// recoverable from the user's side: they would have to
		// remember what they had told the assistant to remember.
		return "the consolidation is empty"

	case len(after) >= len(before):
		// Not a consolidation. Rewriting the same number of entries is
		// the assistant rewording the user's notes for no benefit, and
		// each pass would reword them again.
		return "the consolidation is not shorter"

	case renderedLen(after) >= renderedLen(before):
		// Fewer entries can still be longer, and length is what the
		// cap measures — a "consolidation" that grows the block makes
		// the write failure arrive sooner.
		return "the consolidation is larger than what it replaces"

	case cap > 0 && renderedLen(after) > cap:
		return "the consolidation still exceeds the cap"
	}
	return ""
}

// splitPinnedSummary turns the summariser's prose back into entries.
//
// One per line, blanks dropped. A summariser handed a list and asked
// to compact it returns a list; anything that comes back as a
// paragraph becomes one long entry, which the length check above then
// refuses if it is not actually shorter.
func splitPinnedSummary(summary string) []string {
	var out []string
	for line := range strings.SplitSeq(summary, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
		if line == "" {
			continue
		}
		out = append(out, strings.TrimSpace(line))
	}
	return out
}
