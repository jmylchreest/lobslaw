package memory

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/raft"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/dataformat"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestHistoricalTeamRecordsUseCanonicalRaftReader(t *testing.T) {
	for _, profile := range []string{dataformat.LegacyTeamsEarly, dataformat.LegacyTeams} {
		t.Run(profile, func(t *testing.T) {
			raw, err := os.ReadFile("../dataformat/testdata/" + profile + ".binpb")
			if err != nil {
				t.Fatal(err)
			}
			source := raft.NewInmemStore()
			if err := source.StoreLog(&raft.Log{Index: 17, Term: 4, Type: raft.LogCommand, Data: raw}); err != nil {
				t.Fatal(err)
			}
			reader, err := preflightLogs(context.Background(), source, profile)
			if err != nil {
				t.Fatal(err)
			}
			var log raft.Log
			if err := reader.GetLog(17, &log); err != nil {
				t.Fatal(err)
			}
			if log.Index != 17 || log.Term != 4 {
				t.Fatal("changed raft identity")
			}
			var entry pb.LogEntry
			if err := proto.Unmarshal(log.Data, &entry); err != nil {
				t.Fatal(err)
			}
			const exactRevision uint64 = 9007199254740993
			if profile == dataformat.LegacyTeamsEarly {
				bot := entry.GetBot()
				if bot == nil || bot.Owner != "user:alice" || bot.Revision != exactRevision {
					t.Fatalf("lost bot identity/revision: %v", bot)
				}
				store, _ := newTestStore(t)
				fsm := NewFSM(store)
				if result := fsm.Apply(&log); result != nil {
					t.Fatal(result)
				}
				stored, err := store.Get(BucketBots, "worker")
				if err != nil {
					t.Fatal(err)
				}
				var got pb.BotRecord
				if err := proto.Unmarshal(stored, &got); err != nil {
					t.Fatal(err)
				}
				if got.Owner != bot.Owner || got.Revision != 1 {
					t.Fatal("replay changed owner or initial PUT revision semantics")
				}
			} else {
				inbox := entry.GetBotInbox()
				if inbox == nil || inbox.RequestedBy != "user:alice" || inbox.Status != pb.InboxStatus_INBOX_STATUS_DONE || entry.GetExpectedRevision() != exactRevision {
					t.Fatalf("lost task state: %v", inbox)
				}
			}
			var unchanged raft.Log
			if err := source.GetLog(17, &unchanged); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(raw, unchanged.Data) {
				t.Fatal("rewrote committed bytes")
			}
		})
	}
}

func TestTeamStateUpgradesFoundationAndKeepsRollback(t *testing.T) {
	store, path := newTestStore(t)
	key := store.key
	if err := store.Put(BucketPinned, "retained", []byte("original")); err != nil {
		t.Fatal(err)
	}
	if err := store.loadDB().Update(func(tx *bolt.Tx) error {
		return writeStateFormat(tx, StateFormat{Version: 1, Protocol: dataformat.PreviousStateProtocol})
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := OpenStore(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upgraded.Close() }()
	if err := upgraded.loadDB().View(func(tx *bolt.Tx) error {
		format, err := readStateFormat(tx)
		if err == nil && (format.Version != 2 || format.Protocol != dataformat.ClusterProtocol) {
			t.Fatalf("not migrated: %+v", format)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if raw, err := upgraded.Get(BucketPinned, "retained"); err != nil || string(raw) != "original" {
		t.Fatal("lost source data", err)
	}
	copies, err := filepath.Glob(filepath.Join(filepath.Dir(path), "state-before-migration-*.db"))
	if err != nil || len(copies) != 1 {
		t.Fatal("missing rollback image", err)
	}
}
