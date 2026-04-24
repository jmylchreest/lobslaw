package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
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
	if _, err := NewEpisodicIngester(nil, 0); err == nil {
		t.Error("nil raft should fail")
	}
}

func TestEpisodicIngestCapturesRecord(t *testing.T) {
	t.Parallel()
	applier := &fakeApplier{}
	ing, err := NewEpisodicIngester(applier, 0)
	if err != nil {
		t.Fatal(err)
	}
	err = ing.IngestTurn(context.Background(), EpisodicTurn{
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
	ing, _ := NewEpisodicIngester(applier, 0)
	long := ""
	for i := 0; i < 500; i++ {
		long += "x"
	}
	_ = ing.IngestTurn(context.Background(), EpisodicTurn{
		UserMessage: long,
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
	ing, _ := NewEpisodicIngester(applier, 0)
	err := ing.IngestTurn(context.Background(), EpisodicTurn{
		UserMessage: "x",
		AssistReply: "y",
		CompletedAt: time.Now(),
	})
	if err == nil {
		t.Error("raft error should propagate")
	}
}
