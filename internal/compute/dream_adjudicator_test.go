package compute

import (
	"context"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func testCluster() *lobslawv1.Cluster {
	return &lobslawv1.Cluster{
		Id: "cluster-1",
		Records: []*lobslawv1.VectorRecord{
			{Id: "v1", Text: "john is vegetarian", SourceIds: []string{"a"}},
			{Id: "v2", Text: "john had the steak", SourceIds: []string{"b"}},
		},
	}
}

// The verdict has to survive the wrapper models add unasked.
func TestAdjudicatorReadsFencedJSON(t *testing.T) {
	t.Parallel()
	p := NewMockProvider(MockResponse{Content: "```json\n" +
		`{"verdict":"conflict","reason":"Which is it?"}` + "\n```"})
	a := NewDreamAdjudicator(p, "m", nil)

	got, err := a.AdjudicateMerge(context.Background(), testCluster())
	if err != nil {
		t.Fatal(err)
	}
	if got.Verdict != memory.VerdictConflict || got.Reason != "Which is it?" {
		t.Fatalf("adjudication = %+v; want the fenced verdict", got)
	}
}

// An unreadable reply must not leave the cluster to be asked about
// every night forever, and must not be read as permission to delete
// anything.
func TestAdjudicatorFallsBackToKeepDistinct(t *testing.T) {
	t.Parallel()
	for _, reply := range []string{"I'm not sure", `{"verdict":"obliterate"}`} {
		p := NewMockProvider(MockResponse{Content: reply})
		a := NewDreamAdjudicator(p, "m", nil)

		got, err := a.AdjudicateMerge(context.Background(), testCluster())
		if err != nil {
			t.Fatalf("%q: %v", reply, err)
		}
		if got.Verdict != memory.VerdictKeepDistinct {
			t.Errorf("%q gave verdict %q; want keep_distinct", reply, got.Verdict)
		}
		if got.Reason == "" {
			t.Errorf("%q gave no reason; the log has to say why", reply)
		}
	}
}

// The prompt identifies members by the memory id, not the vector's:
// a "current" verdict names something the caller has to be able to
// act on.
func TestAdjudicatorNamesMemoriesNotVectors(t *testing.T) {
	t.Parallel()
	p := NewMockProvider(MockResponse{Content: `{"verdict":"keep_distinct"}`})
	a := NewDreamAdjudicator(p, "m", nil)

	if _, err := a.AdjudicateMerge(context.Background(), testCluster()); err != nil {
		t.Fatal(err)
	}
	sent := p.Calls()[0].Messages[1].Content
	for _, want := range []string{"id=a", "id=b"} {
		if !strings.Contains(sent, want) {
			t.Errorf("prompt does not name %q:\n%s", want, sent)
		}
	}
	if strings.Contains(sent, "id=v1") {
		t.Errorf("prompt names the vector id; a verdict about it could not be applied:\n%s", sent)
	}
}
