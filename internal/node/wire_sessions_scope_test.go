package node

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// seededBrowser builds the real adapter over a real store. Records go
// in directly rather than through raft: this is testing the read side,
// and a raft node per test costs a second of leader election for
// nothing.
func seededBrowser(t *testing.T, recs ...*lobslawv1.SessionRecord) *sessionBrowserAdapter {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(t.TempDir(), "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	for _, r := range recs {
		raw, err := proto.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(memory.BucketSessions, r.Id, raw); err != nil {
			t.Fatal(err)
		}
	}
	return &sessionBrowserAdapter{inner: memory.NewSessionService(nil, store, memory.SessionConfig{})}
}

func sessionRec(channel, channelID, userID, title string, ageMinutes int) *lobslawv1.SessionRecord {
	return &lobslawv1.SessionRecord{
		Id:        channel + ":" + channelID,
		Channel:   channel,
		ChannelId: channelID,
		UserId:    userID,
		Title:     title,
		FirstSeq:  0,
		NextSeq:   4,
		UpdatedAt: timestamppb.New(time.Now().Add(-time.Duration(ageMinutes) * time.Minute)),
	}
}

// Recent must drop what the caller can't see before it truncates.
// Filtering afterwards would hand a user an empty list whenever
// enough other people had spoken more recently — which on a busy
// shared node is always.
func TestSessionBrowserRecentFiltersBeforeLimiting(t *testing.T) {
	t.Parallel()
	recs := []*lobslawv1.SessionRecord{sessionRec("telegram", "1", "alice", "Alice's thread", 100)}
	for i := range 5 {
		recs = append(recs, sessionRec("telegram", fmt.Sprintf("9%d", i), "bob", "Bob's thread", i))
	}
	browser := seededBrowser(t, recs...)

	scope := compute.TurnIdentity{UserID: "alice"}
	got, err := browser.Recent(context.Background(), 2, scope.Visible)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].UserID != "alice" {
		t.Fatalf("got %+v, want only alice's conversation", got)
	}
}

func TestSessionBrowserInfoReportsOwnerAndAbsence(t *testing.T) {
	t.Parallel()
	browser := seededBrowser(t, sessionRec("telegram", "-300", "alice", "Weekend plans", 1))

	info, found, err := browser.Info(context.Background(),
		compute.SessionKey{Channel: "telegram", ChannelID: "-300"})
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if info.UserID != "alice" {
		t.Errorf("owner = %q, want alice", info.UserID)
	}

	// session_read's gate turns "not found" into a refusal, so this
	// must not come back as an error the tool would report verbatim.
	if _, found, err := browser.Info(context.Background(),
		compute.SessionKey{Channel: "telegram", ChannelID: "404"}); found || err != nil {
		t.Errorf("absent session: found=%v err=%v", found, err)
	}
}
