package memory

import (
	"context"
	"fmt"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/dataformat"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// The adapter changes historical reads, never committed bytes, indexes, terms,
// membership entries, extensions or the Raft stable store. Replication and local
// replay receive the same canonical envelopes.
type compatibleLogStore struct {
	raft.LogStore
	profile string
}

func (s *compatibleLogStore) GetLog(index uint64, out *raft.Log) error {
	if err := s.LogStore.GetLog(index, out); err != nil {
		return err
	}
	if out.Type != raft.LogCommand {
		return nil
	}
	raw, err := dataformat.NormalizeLog(out.Data, s.profile)
	if err != nil {
		return fmt.Errorf("raft log %d: %w", index, err)
	}
	var entry pb.LogEntry
	if err := proto.Unmarshal(raw, &entry); err != nil {
		return err
	}
	if err := validateLogEntrySupport(&entry); err != nil {
		return fmt.Errorf("raft log %d is unsupported: %w", index, err)
	}
	out.Data = raw
	return nil
}

func preflightLogs(ctx context.Context, store raft.LogStore, profile string) (*compatibleLogStore, error) {
	adapted := &compatibleLogStore{LogStore: store, profile: profile}
	first, err := store.FirstIndex()
	if err != nil {
		return nil, err
	}
	last, err := store.LastIndex()
	if err != nil {
		return nil, err
	}
	if first == 0 {
		return adapted, nil
	}
	for index := first; ; index++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var entry raft.Log
		if err := adapted.GetLog(index, &entry); err != nil {
			return nil, err
		}
		if index == last {
			break
		}
	}
	return adapted, nil
}
