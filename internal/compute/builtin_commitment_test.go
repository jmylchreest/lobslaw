package compute

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestParseWhenAcceptsDuration(t *testing.T) {
	t.Parallel()
	got, err := parseWhen("2m", "")
	if err != nil {
		t.Fatal(err)
	}
	delta := time.Until(got)
	if delta < time.Minute || delta > 3*time.Minute {
		t.Errorf("expected ~2 minutes ahead; got %s", delta)
	}
}

func TestParseWhenAcceptsRFC3339WithOffset(t *testing.T) {
	t.Parallel()
	got, err := parseWhen("2030-01-01T12:00:00Z", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Year() != 2030 {
		t.Errorf("year=%d", got.Year())
	}
}

func TestParseWhenNakedTimestampInterpretedInUserTZ(t *testing.T) {
	t.Parallel()
	// "9am on April 30 2026" with user in Europe/London (BST=UTC+1)
	// should resolve to 08:00 UTC.
	got, err := parseWhen("2026-04-30T09:00:00", "Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 4, 30, 8, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("naked wall-clock in BST: got %s, want %s", got, want)
	}
	if got.Location() != time.UTC {
		t.Errorf("returned time should be in UTC; got %v", got.Location())
	}
}

func TestParseWhenNakedTimestampUTCWhenNoTZ(t *testing.T) {
	t.Parallel()
	got, err := parseWhen("2026-04-30T09:00:00", "")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 4, 30, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("naked wall-clock with empty TZ should fall back to UTC: got %s, want %s", got, want)
	}
}

func TestParseWhenRFC3339OffsetIgnoresUserTZ(t *testing.T) {
	t.Parallel()
	// Explicit offset → use as-is, never re-interpret via userTZ.
	got, err := parseWhen("2026-04-30T09:00:00+02:00", "Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 4, 30, 7, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("explicit offset should be honoured: got %s, want %s", got, want)
	}
}

func TestParseWhenRejectsGarbage(t *testing.T) {
	t.Parallel()
	if _, err := parseWhen("not a time", ""); err == nil {
		t.Error("garbage input should fail")
	}
}

func TestParseWhenRejectsEmpty(t *testing.T) {
	t.Parallel()
	if _, err := parseWhen("", ""); err == nil {
		t.Error("empty input should fail")
	}
}

func seedCommitment(t *testing.T, store *memory.Store, id, status string, due time.Time) {
	t.Helper()
	c := &lobslawv1.AgentCommitment{
		Id:     id,
		Status: status,
		DueAt:  timestamppb.New(due),
		Params: map[string]string{"prompt": "ping"},
	}
	raw, err := proto.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(memory.BucketCommitments, id, raw); err != nil {
		t.Fatal(err)
	}
}

func TestCommitmentListHidesNonPendingByDefault(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	now := time.Now()
	seedCommitment(t, store, "p1", "pending", now.Add(time.Hour))
	seedCommitment(t, store, "d1", "done", now.Add(-time.Hour))
	seedCommitment(t, store, "d2", "done", now.Add(-2*time.Hour))

	b := NewBuiltins()
	if err := RegisterCommitmentBuiltins(b, CommitmentConfig{Store: store, Raft: &fakeApplier{}}); err != nil {
		t.Fatal(err)
	}
	fn, _ := b.Get("commitment_list")
	out, exit, err := fn(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if exit != 0 {
		t.Errorf("exit = %d", exit)
	}
	var payload struct {
		Count          int  `json:"count"`
		HiddenCount    int  `json:"hidden_count"`
		IncludeHistory bool `json:"include_history"`
		Commitments    []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"commitments"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Count != 1 {
		t.Errorf("count = %d; want 1 (pending only)", payload.Count)
	}
	if payload.HiddenCount != 2 {
		t.Errorf("hidden_count = %d; want 2", payload.HiddenCount)
	}
	if payload.IncludeHistory {
		t.Error("include_history echoed true; want false")
	}
	if len(payload.Commitments) != 1 || payload.Commitments[0].ID != "p1" {
		t.Errorf("commitments = %+v; want [p1]", payload.Commitments)
	}
}

func TestCommitmentListIncludesHistoryWhenAsked(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	now := time.Now()
	seedCommitment(t, store, "p1", "pending", now.Add(time.Hour))
	seedCommitment(t, store, "d1", "done", now.Add(-time.Hour))

	b := NewBuiltins()
	_ = RegisterCommitmentBuiltins(b, CommitmentConfig{Store: store, Raft: &fakeApplier{}})
	fn, _ := b.Get("commitment_list")
	out, _, err := fn(context.Background(), map[string]string{"include_history": "true"})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Count       int `json:"count"`
		HiddenCount int `json:"hidden_count"`
		Commitments []struct {
			ID string `json:"id"`
		} `json:"commitments"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Count != 2 {
		t.Errorf("count = %d; want 2 (pending + done)", payload.Count)
	}
	if payload.HiddenCount != 0 {
		t.Errorf("hidden_count = %d; want 0", payload.HiddenCount)
	}
}

func TestCommitmentListRejectsBadBoolean(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	b := NewBuiltins()
	_ = RegisterCommitmentBuiltins(b, CommitmentConfig{Store: store, Raft: &fakeApplier{}})
	fn, _ := b.Get("commitment_list")
	_, exit, err := fn(context.Background(), map[string]string{"include_history": "yeppers"})
	if err == nil || exit == 0 {
		t.Error("non-boolean include_history should fail")
	}
}
