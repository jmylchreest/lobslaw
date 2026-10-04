package memory

import (
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/pkg/crypto"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestFSMRefusesStaleCredentialBoundaryChanges(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fsm := NewFSM(store)
	rec := &pb.CredentialRecord{Id: "c", Provider: "google", Subject: "c", Owner: "alice", Connector: "google-calendar", Generation: "g"}
	raw, _ := proto.Marshal(rec)
	if err := store.Put(BucketCredentials, "google/c", raw); err != nil {
		t.Fatal(err)
	}
	for _, other := range []*pb.CredentialRecord{{Provider: "google", Subject: "c"}, {Provider: "google", Subject: "c", Owner: "bob", Connector: "google-calendar"}, {Provider: "google", Subject: "c", Owner: "alice", Connector: "other"}} {
		entry := &pb.LogEntry{Id: "google/c", Payload: &pb.LogEntry_Credential{Credential: other}}
		if err := fsm.applyPut(entry); err == nil {
			t.Fatal("stale PUT crossed boundary")
		}
		if err := fsm.applyDelete(entry); err == nil {
			t.Fatal("stale DELETE crossed boundary")
		}
	}
}
