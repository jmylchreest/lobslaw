package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/textutil"
)

type fakeApplier struct {
	entries []*lobslawv1.LogEntry
	err     error
}

func (f *fakeApplier) Apply(data []byte, _ time.Duration) (any, error) {
	if f.err != nil {
		return f.err, nil
	}
	var le lobslawv1.LogEntry
	if err := proto.Unmarshal(data, &le); err != nil {
		return err, nil
	}
	f.entries = append(f.entries, &le)
	return nil, nil
}

func TestNewEpisodicIngesterRequiresRaft(t *testing.T) {
	t.Parallel()
	if _, err := NewEpisodicIngester(nil, 0, nil); err == nil {
		t.Error("nil raft should fail")
	}
}

func TestEpisodicIngestCapturesRecord(t *testing.T) {
	t.Parallel()
	applier := &fakeApplier{}
	ing, err := NewEpisodicIngester(applier, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = ing.IngestTurn(context.Background(), EpisodicTurn{
		Owner:       "user:alice",
		Channel:     "telegram",
		ChatID:      "123",
		UserID:      "user:alice",
		UserMessage: "hello",
		AssistReply: "hi there",
		TurnID:      "tg-7",
		CompletedAt: time.Unix(1_700_000_000, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(applier.entries) != 1 {
		t.Fatalf("apply called %d times; want 1", len(applier.entries))
	}
	le := applier.entries[0]
	epi, ok := le.Payload.(*lobslawv1.LogEntry_EpisodicRecord)
	if !ok {
		t.Fatalf("payload = %T", le.Payload)
	}
	if epi.EpisodicRecord.Event != "hello" {
		t.Errorf("event = %q", epi.EpisodicRecord.Event)
	}
	// Tags should include channel + user + chat + turn.
	wantTags := map[string]bool{
		"channel:telegram": true,
		"user:user:alice":  true,
		"chat:123":         true,
		"turn:tg-7":        true,
	}
	for _, got := range epi.EpisodicRecord.Tags {
		if !wantTags[got] {
			t.Errorf("unexpected tag %q", got)
		}
		delete(wantTags, got)
	}
	if len(wantTags) > 0 {
		t.Errorf("missing tags: %+v", wantTags)
	}
}

func TestEpisodicIngestLongEventTruncates(t *testing.T) {
	t.Parallel()
	applier := &fakeApplier{}
	ing, _ := NewEpisodicIngester(applier, 0, nil)
	var long strings.Builder
	for range 500 {
		long.WriteString("x")
	}
	_ = ing.IngestTurn(context.Background(), EpisodicTurn{
		Owner:       "user:alice",
		UserMessage: long.String(),
		AssistReply: "ok",
		CompletedAt: time.Now(),
	})
	le := applier.entries[0]
	epi := le.Payload.(*lobslawv1.LogEntry_EpisodicRecord)
	if len(epi.EpisodicRecord.Event) > 200 {
		t.Errorf("event not truncated: len=%d", len(epi.EpisodicRecord.Event))
	}
}

func TestEpisodicIngestSurfacesRaftError(t *testing.T) {
	t.Parallel()
	applier := &fakeApplier{err: errors.New("no quorum")}
	ing, _ := NewEpisodicIngester(applier, 0, nil)
	err := ing.IngestTurn(context.Background(), EpisodicTurn{
		Owner:       "user:alice",
		UserMessage: "x",
		AssistReply: "y",
		CompletedAt: time.Now(),
	})
	if err == nil {
		t.Error("raft error should propagate")
	}
}

// A Telegram paste with em dashes was cut at byte 140, the cut landed
// inside a character, and protobuf refused the record — so the turn
// answered and nothing was remembered about it. Silent by
// construction: the ingest runs in a background goroutine whose error
// is logged and dropped, because a memory write must not fail
// somebody's answer.
func TestATurnWithMultiByteTextIsStillRemembered(t *testing.T) {
	t.Parallel()
	// The cut must land INSIDE a character, not merely somewhere in a
	// string that contains some. A first version of this test padded
	// with a repeating phrase and byte 140 happened to fall on an
	// ASCII letter, so it passed against the very bug it was written
	// for.
	//
	// An em dash is three bytes. Putting it at bytes 139-141 means a
	// 140-byte slice keeps its first byte and nothing else.
	for _, pad := range []int{138, 139, 140} {
		msg := strings.Repeat("a", pad) + "—" + strings.Repeat("z", 200)

		event := turnEventSummary(msg)
		if !utf8.ValidString(event) {
			t.Fatalf("pad=%d: the event summary is not valid UTF-8; protobuf will refuse the record", pad)
		}
		rec := &lobslawv1.EpisodicRecord{
			Owner: "user:test", Id: "x", Event: event, Timestamp: timestamppb.Now()}
		if _, err := proto.Marshal(rec); err != nil {
			t.Fatalf("pad=%d: the record does not marshal, so the memory is lost: %v", pad, err)
		}
	}
}

// Text this process did not cut can still arrive invalid, from a
// provider or a channel.
func TestInvalidTextFromElsewhereStillMarshals(t *testing.T) {
	t.Parallel()
	broken := "hello " + string([]byte{0xff}) + " world"
	rec := &lobslawv1.EpisodicRecord{
		Owner: "user:test",
		Id:    "x", Event: textutil.Sanitise(turnEventSummary(broken)), Timestamp: timestamppb.Now(),
	}
	if _, err := proto.Marshal(rec); err != nil {
		t.Fatalf("a record built from invalid input does not marshal: %v", err)
	}
}

// Provenance survives ingest.
//
// The render side is tested in internal/compute, but a via= that is
// never written is decoration: the block would be correct and always
// empty, and every test of the renderer would still pass. This is the
// half that makes the attribute real.
func TestEpisodicIngestRecordsToolProvenance(t *testing.T) {
	t.Parallel()
	applier := &fakeApplier{}
	ing, err := NewEpisodicIngester(applier, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = ing.IngestTurn(context.Background(), EpisodicTurn{
		Owner:       "user:alice",
		Channel:     "telegram",
		UserMessage: "put galaxy chocolate on the list",
		AssistReply: "done, it's on the list",
		CompletedAt: time.Unix(1_700_000_000, 0).UTC(),
		Via:         []string{"kitchenowl_add_shoppinglist_item"},
	})
	if err != nil {
		t.Fatal(err)
	}
	epi, ok := applier.entries[0].Payload.(*lobslawv1.LogEntry_EpisodicRecord)
	if !ok {
		t.Fatalf("payload = %T", applier.entries[0].Payload)
	}
	got := epi.EpisodicRecord.Via
	if len(got) != 1 || got[0] != "kitchenowl_add_shoppinglist_item" {
		t.Errorf("via = %v; the tool that produced this memory was not recorded", got)
	}
}

// A turn that called nothing leaves the field absent, so "no tool ran"
// and "a tool ran whose name was lost" stay different states.
func TestEpisodicIngestLeavesProvenanceEmptyWithoutTools(t *testing.T) {
	t.Parallel()
	applier := &fakeApplier{}
	ing, err := NewEpisodicIngester(applier, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ing.IngestTurn(context.Background(), EpisodicTurn{
		Owner:       "user:alice",
		UserMessage: "what do you think about the plan",
		AssistReply: "it looks reasonable",
		CompletedAt: time.Unix(1_700_000_000, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	epi := applier.entries[0].Payload.(*lobslawv1.LogEntry_EpisodicRecord)
	if len(epi.EpisodicRecord.Via) != 0 {
		t.Errorf("via = %v; want empty for a turn that called no tools", epi.EpisodicRecord.Via)
	}
}
