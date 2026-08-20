package node

import (
	"os"
	"strings"
	"testing"
)

// THE WIRING, NOT THE LOGIC.
//
// compute's own tests prove a DreamSummarizer embeds its summary when
// given an embedder. None of them notice if this package hands it nil
// — and that is the failure that actually shipped, twice, in this
// file: n.embedder was once assigned inside wireEmbedder's remote
// branch, so the builtin path left it nil and the node wrote no
// vectors at all while logging that the model was ready.
//
// Changing the argument at the call site below to nil passes every
// other test in the tree. This one is what fails.
func TestDreamSummarizerIsGivenTheNodeEmbedder(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("wire_compute.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)

	call := strings.Index(s, "compute.NewDreamSummarizer(")
	if call < 0 {
		t.Fatal("no call to NewDreamSummarizer — if it moved, move this guard with it")
	}
	end := strings.Index(s[call:], ")\n")
	if end < 0 {
		t.Fatal("could not find the end of the NewDreamSummarizer call")
	}
	if args := s[call : call+end]; !strings.Contains(args, "n.embedder") {
		t.Errorf("the dream summarizer is not given n.embedder:\n\t%s\n"+
			"every consolidation it writes will have no vector, and vector search "+
			"must skip those for the rest of their lives", strings.TrimSpace(args))
	}

	// ORDER. The field has to be set before the summarizer is built,
	// or the argument above is a correctly-spelled nil.
	assign := strings.Index(s, "n.embedder = embedder")
	attach := strings.Index(s, "n.attachDreamSummarizer()")
	switch {
	case assign < 0:
		t.Fatal("n.embedder is never assigned in wire_compute.go")
	case attach < 0:
		t.Fatal("attachDreamSummarizer is never called from wire_compute.go")
	case assign > attach:
		t.Error("the dream summarizer is attached BEFORE n.embedder is assigned, " +
			"so it receives nil however the call site is written")
	}
}
