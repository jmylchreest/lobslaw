package main

import "testing"

// history and rollback read the version bucket, which the CLI cannot
// open while the node holds state.db. "What did it used to think" is
// a question about a running agent, so requiring it be stopped made
// the answer unavailable exactly when it was wanted.
func TestLearnedHistoryAndRollbackAreLive(t *testing.T) {
	t.Parallel()
	for _, sub := range []string{"history", "rollback"} {
		form, ok := learnedForms[sub]
		if !ok || form.live == nil {
			t.Errorf("learned %s has no live form", sub)
		}
		if _, stillOffline := learnedOfflineOnly[sub]; stillOffline {
			t.Errorf("learned %s is still listed as offline-only", sub)
		}
	}
	// discard stays offline-only on purpose: its preview covers the
	// whole live set, and composing it from per-artefact calls would
	// make the preview and the writes two different reads.
	if _, ok := learnedOfflineOnly["discard"]; !ok {
		t.Error("discard should still be offline-only, with the reason recorded")
	}
}
