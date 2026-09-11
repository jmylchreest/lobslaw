package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func newWatchBuiltins(t *testing.T, store *memory.Store) (*Builtins, *fakeApplier) {
	t.Helper()
	raft := &fakeApplier{}
	b := NewBuiltins()
	if err := RegisterWatchBuiltins(b, WatchConfig{Store: store, Raft: raft}); err != nil {
		t.Fatal(err)
	}
	return b, raft
}

func TestWatchCreateWritesAWatchCommitment(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	b, raft := newWatchBuiltins(t, store)
	fn, _ := b.Get("watch_create")

	out, exit, err := fn(turnAs("alice"), map[string]string{
		"what":     "the fare on BA117 on 3 March",
		"interval": "30m",
	})
	if err != nil || exit != 0 {
		t.Fatalf("exit=%d err=%v", exit, err)
	}
	if len(raft.entries) != 1 {
		t.Fatalf("expected one raft write; got %d", len(raft.entries))
	}
	c := raft.entries[0].GetCommitment()
	if c == nil {
		t.Fatal("the write should carry a commitment")
	}
	if c.HandlerRef != WatchHandlerRef {
		t.Errorf("handler ref = %q, want %q", c.HandlerRef, WatchHandlerRef)
	}
	if c.Owner != "user:alice" {
		t.Errorf("owner = %q, want user:alice", c.Owner)
	}
	if c.Watch == nil {
		t.Fatal("a watch must carry watch state; without it the handler cannot compare anything")
	}
	if got := c.Watch.BaseInterval.AsDuration(); got != 30*time.Minute {
		t.Errorf("base interval = %s, want 30m", got)
	}
	if c.Watch.MaxFailures == 0 {
		t.Error("max failures should be stamped so the record is self-describing")
	}
	if c.Watch.ExpiresAt == nil {
		t.Error("a watch must expire by default; an unbounded one runs until somebody remembers it")
	}
	// Due now, so "I'm watching that" is true as soon as it is said.
	if c.DueAt.AsTime().After(time.Now().Add(time.Minute)) {
		t.Errorf("first check should be due immediately; got %s", c.DueAt.AsTime())
	}
	var reply map[string]any
	if err := json.Unmarshal(out, &reply); err != nil {
		t.Fatal(err)
	}
	if reply["id"] == "" {
		t.Error("the reply should carry the id so the model can refer to it")
	}
}

func TestWatchCreateRejectsBadInput(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	b, raft := newWatchBuiltins(t, store)
	fn, _ := b.Get("watch_create")

	cases := []struct {
		name string
		args map[string]string
	}{
		{"no what", map[string]string{"interval": "1h"}},
		{"interval is not a duration", map[string]string{"what": "x", "interval": "soon"}},
		{"negative interval", map[string]string{"what": "x", "interval": "-1h"}},
		// An absolute timestamp as an interval would otherwise make a
		// watch that checks once and looks like it is working.
		{"timestamp as interval", map[string]string{"what": "x", "interval": "2030-01-01T00:00:00Z"}},
		{"expiry in the past", map[string]string{"what": "x", "expires": "2020-01-01T00:00:00Z"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, exit, err := fn(turnAs("alice"), tc.args); err == nil || exit == 0 {
				t.Errorf("expected refusal; got exit=%d err=%v", exit, err)
			}
		})
	}
	if len(raft.entries) != 0 {
		t.Errorf("a refused watch must not be written; got %d writes", len(raft.entries))
	}
}

func seedWatch(t *testing.T, store *memory.Store, id, owner, what, status string, observation string) {
	t.Helper()
	c := &lobslawv1.AgentCommitment{
		Id:         id,
		Owner:      owner,
		Status:     status,
		HandlerRef: WatchHandlerRef,
		DueAt:      timestamppb.New(time.Now().Add(time.Hour)),
		Params:     map[string]string{"what": what},
		Watch: &lobslawv1.WatchState{
			Observation:  observation,
			Digest:       "d",
			Interval:     durationpb.New(time.Hour),
			BaseInterval: durationpb.New(time.Hour),
			LastChecked:  timestamppb.New(time.Now().Add(-time.Hour)),
			LastChanged:  timestamppb.New(time.Now().Add(-2 * time.Hour)),
		},
	}
	raw, err := proto.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(memory.BucketCommitments, id, raw); err != nil {
		t.Fatal(err)
	}
}

func TestWatchListShowsWhatItLastSawAndWhenItNextChecks(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	seedWatch(t, store, "w1", "user:alice", "the fare", "pending", "price: 212.00 GBP")
	// A plain reminder is not a watch and belongs to commitment_list.
	seedCommitment(t, store, "c1", "pending", time.Now().Add(time.Hour))

	b, _ := newWatchBuiltins(t, store)
	fn, _ := b.Get("watch_list")
	out, exit, err := fn(turnAs("alice"), nil)
	if err != nil || exit != 0 {
		t.Fatalf("exit=%d err=%v", exit, err)
	}
	var reply struct {
		Count   int `json:"count"`
		Watches []struct {
			ID          string `json:"id"`
			What        string `json:"what"`
			LastSeen    string `json:"last_seen"`
			LastChanged string `json:"last_changed"`
			NextCheck   string `json:"next_check"`
		} `json:"watches"`
	}
	if err := json.Unmarshal(out, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Count != 1 {
		t.Fatalf("count = %d, want 1 (a commitment without watch state is not a watch)", reply.Count)
	}
	w := reply.Watches[0]
	if w.LastSeen != "price: 212.00 GBP" {
		t.Errorf("last seen = %q; the answer to \"what did you see\" must come from the record", w.LastSeen)
	}
	if w.LastChanged == "" || w.NextCheck == "" {
		t.Errorf("last change and next check must both be reported; got %+v", w)
	}
}

func TestWatchListHidesOtherPrincipals(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	seedWatch(t, store, "w1", "user:alice", "the fare", "pending", "price: 1")
	seedWatch(t, store, "w2", "user:bob", "bob's thing", "pending", "price: 2")

	b, _ := newWatchBuiltins(t, store)
	fn, _ := b.Get("watch_list")
	out, _, err := fn(turnAs("alice"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "bob") {
		t.Errorf("another principal's watch leaked into the list: %s", out)
	}
	// Not even the hidden counter: that would leak how many watches
	// other people are running.
	var reply struct {
		Count  int `json:"count"`
		Hidden int `json:"hidden_count"`
	}
	if err := json.Unmarshal(out, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Count != 1 || reply.Hidden != 0 {
		t.Errorf("count=%d hidden=%d, want 1 and 0", reply.Count, reply.Hidden)
	}
}

func TestWatchCancelRefusesAnotherPrincipalTheSameWayAsMissing(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	seedWatch(t, store, "w2", "user:bob", "bob's thing", "pending", "price: 2")

	b, raft := newWatchBuiltins(t, store)
	fn, _ := b.Get("watch_cancel")

	_, exitOther, errOther := fn(turnAs("alice"), map[string]string{"id": "w2"})
	_, exitMissing, errMissing := fn(turnAs("alice"), map[string]string{"id": "nope"})
	if errOther == nil || errMissing == nil {
		t.Fatal("both should be refused")
	}
	// Identical answers, so an id cannot be probed for existence.
	if errOther.Error() != errMissing.Error() || exitOther != exitMissing {
		t.Errorf("refusals differ and so leak existence:\n other:   %v\n missing: %v", errOther, errMissing)
	}
	if len(raft.entries) != 0 {
		t.Errorf("a refused cancel must not write; got %d", len(raft.entries))
	}
}

func TestWatchReportRecordsStateAndSummary(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	b, _ := newWatchBuiltins(t, store)
	fn, _ := b.Get("watch_report")

	ctx, collector := compute.WithWatchCollector(turnAs("alice"))
	if _, exit, err := fn(ctx, map[string]string{
		"state":   "price: 212.00 GBP",
		"summary": "Unchanged.",
	}); err != nil || exit != 0 {
		t.Fatalf("exit=%d err=%v", exit, err)
	}
	got, calls := collector.Report()
	if got == nil {
		t.Fatal("the report should reach the collector")
	}
	if got.State != "price: 212.00 GBP" || got.Summary != "Unchanged." {
		t.Errorf("report = %+v", got)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

// Reaching this outside a watch check means something put the tool in
// a turn that cannot use it. Accepting the report would be worse than
// refusing it: nothing downstream would ever read it.
func TestWatchReportOutsideAWatchTurnIsAnError(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	b, _ := newWatchBuiltins(t, store)
	fn, _ := b.Get("watch_report")

	_, exit, err := fn(context.Background(), map[string]string{"state": "price: 1"})
	if err == nil || exit == 0 {
		t.Fatalf("expected an error outside a watch check; got exit=%d err=%v", exit, err)
	}
	if !strings.Contains(err.Error(), "not a watch check") {
		t.Errorf("the error should say why; got %v", err)
	}
}

// watch_report is registered so it can be invoked, and unlisted so the
// model is not offered a tool that fails in every context but one.
func TestWatchReportIsRegisteredButNotAdvertised(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	for _, td := range WatchToolDefs() {
		if err := r.Register(td); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := r.Get("watch_report"); !ok {
		t.Fatal("watch_report must be registered — the watch turn has to be able to call it")
	}
	for _, tl := range r.LLMTools() {
		if tl.Name == "watch_report" {
			t.Error("watch_report must not appear in the default LLM tool list")
		}
	}
	var sawCreate bool
	for _, tl := range r.LLMTools() {
		if tl.Name == "watch_create" {
			sawCreate = true
		}
	}
	if !sawCreate {
		t.Error("the rest of the family must still be advertised")
	}
}

// watch_cancel deletes records irreversibly, so it must only delete
// the kind of record it says it does. A model that confuses a
// reminder's id for a watch's would otherwise silently drop the
// reminder and report success.
func TestWatchCancelWillNotCancelAPlainReminder(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	seedCommitment(t, store, "r1", "pending", time.Now().Add(time.Hour)) // owned by user:alice
	seedWatch(t, store, "w1", "user:alice", "the fare", "pending", "price: 1")

	b, raft := newWatchBuiltins(t, store)
	fn, _ := b.Get("watch_cancel")

	_, exitReminder, errReminder := fn(turnAs("alice"), map[string]string{"id": "r1"})
	if errReminder == nil {
		t.Fatal("watch_cancel deleted a plain reminder")
	}
	if len(raft.entries) != 0 {
		t.Fatalf("a refused cancel must not write; got %d", len(raft.entries))
	}

	// Indistinguishable from "no such id", so the refusal cannot be
	// used to learn that a reminder with that id exists.
	_, exitMissing, errMissing := fn(turnAs("alice"), map[string]string{"id": "nope"})
	if errReminder.Error() != errMissing.Error() || exitReminder != exitMissing {
		t.Errorf("refusals differ and so leak existence:\n reminder: %v\n missing:  %v", errReminder, errMissing)
	}

	// The real watch still cancels.
	if _, exit, err := fn(turnAs("alice"), map[string]string{"id": "w1"}); err != nil || exit != 0 {
		t.Fatalf("cancelling an actual watch failed: exit=%d err=%v", exit, err)
	}
	if len(raft.entries) != 1 {
		t.Errorf("expected one delete; got %d", len(raft.entries))
	}
}

// A state is meant to be the shortest canonical form of a fact. A long
// one is prose, which is what the digest cannot compare — and it is
// replayed into every future probe, so an unbounded one compounds.
func TestWatchReportRejectsAnOversizedState(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	b, _ := newWatchBuiltins(t, store)
	fn, _ := b.Get("watch_report")

	ctx, collector := compute.WithWatchCollector(turnAs("alice"))
	_, exit, err := fn(ctx, map[string]string{"state": strings.Repeat("x", MaxWatchStateChars+1)})
	if err == nil {
		t.Fatal("an oversized state should be refused")
	}
	// Exit 2 is "you did it wrong" — the model can shorten and call
	// again in this same turn rather than the check counting as silent.
	if exit != 2 {
		t.Errorf("exit = %d, want 2 so the model retries rather than failing the check", exit)
	}
	if got, _ := collector.Report(); got != nil {
		t.Error("a refused report must not reach the collector")
	}

	// Exactly at the limit is fine.
	if _, exit, err := fn(ctx, map[string]string{"state": strings.Repeat("x", MaxWatchStateChars)}); err != nil || exit != 0 {
		t.Errorf("a state at the limit should be accepted: exit=%d err=%v", exit, err)
	}
}
