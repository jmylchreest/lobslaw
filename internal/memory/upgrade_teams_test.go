package memory

import (
	"testing"

	bolt "go.etcd.io/bbolt"

	"github.com/jmylchreest/lobslaw/internal/dataformat"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func activateTestTeams(t *testing.T, s *Store) {
	t.Helper()
	if err := s.loadDB().Update(func(tx *bolt.Tx) error { return activateContract(tx, 2) }); err != nil {
		t.Fatal(err)
	}
}

func TestOldContractRejectsExecutionMetadata(t *testing.T) {
	state := dataformat.ContractState{Active: 1}
	entries := []*pb.LogEntry{
		{Op: pb.LogOp_LOG_OP_PUT, Payload: &pb.LogEntry_Bot{Bot: &pb.BotRecord{Id: "new"}}},
		{Op: pb.LogOp_LOG_OP_PUT, Payload: &pb.LogEntry_SessionAppend{SessionAppend: &pb.SessionAppendRecord{Messages: []*pb.SessionMessage{{BudgetPending: true}}}}},
		{Op: pb.LogOp_LOG_OP_PUT, Payload: &pb.LogEntry_TaskApproval{TaskApproval: &pb.TaskApprovalRecord{Id: "task", Continuation: &pb.Continuation{BotId: "worker"}}}},
		{Op: pb.LogOp_LOG_OP_PUT, Payload: &pb.LogEntry_ArchiveBatch{ArchiveBatch: &pb.ArchiveBatch{Records: []*pb.ArchiveMutation{{Kind: "bots", Id: "worker"}}}}},
	}
	for _, entry := range entries {
		if err := validateContractEntry(entry, state); err == nil {
			t.Fatalf("contract 1 accepted %T", entry.Payload)
		}
		if err := validateContractEntry(entry, dataformat.ContractState{Active: 2}); err != nil {
			t.Fatal(err)
		}
	}
}
