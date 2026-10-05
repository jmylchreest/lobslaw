// upgrade-probe is the mTLS workload and offline evidence collector for the
// real-binary startup upgrade rehearsal. Build it from each historical module
// so historical writes use that binary's actual protobuf definitions.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/pkg/crypto"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func main() {
	mode := flag.String("mode", "verify", "seed, connector, verify, dump, or future-state")
	addr := flag.String("addr", "", "running node address")
	certDir := flag.String("cert-dir", "", "CA and node credential directory")
	state := flag.String("state", "", "stopped state database")
	prefix := flag.String("prefix", "before", "workload id prefix")
	churn := flag.Int("churn", 0, "additional real Raft writes to trigger a snapshot")
	flag.Parse()
	var err error
	switch *mode {
	case "dump":
		err = dump(*state)
	case "future-state":
		err = futureState(*state)
	default:
		err = workload(*mode, *addr, *certDir, *prefix, *churn)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func emit(value any) error { return json.NewEncoder(os.Stdout).Encode(value) }

func workload(mode, addr, certDir, prefix string, churn int) error {
	creds, err := mtls.LoadNodeCreds(certDir+"/ca.pem", certDir+"/node.pem", certDir+"/node-key.pem")
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds.ClientCreds()))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	mem := pb.NewMemoryServiceClient(conn)
	node := pb.NewNodeServiceClient(conn)
	var peerHeaders metadata.MD
	if _, err := node.GetPeers(ctx, &pb.GetPeersRequest{}, grpc.Header(&peerHeaders)); err != nil {
		return fmt.Errorf("peer discovery: %w", err)
	}
	propose := func(raw []byte) error {
		request := &pb.ProposeRequest{Entry: raw}
		if len(peerHeaders.Get("lobslaw-data-protocol")) == 0 {
			_, err := node.Propose(ctx, request)
			return err
		}
		// Match the versioned wire endpoint and contract headers of the real
		// node. The legacy module predates the internal interceptor package.
		wireCtx := metadata.AppendToOutgoingContext(ctx, "lobslaw-data-protocol", "lobslaw-rolling-v1", "lobslaw-data-required", "1", "lobslaw-data-supported", "1,2")
		return conn.Invoke(wireCtx, "/lobslaw.persistence.v1."+strings.TrimPrefix(pb.NodeService_Propose_FullMethodName, "/"), request, &pb.ProposeResponse{})
	}
	if mode == "connector" {
		entry := &pb.LogEntry{Op: pb.LogOp_LOG_OP_PUT, Id: prefix + "-connector", Payload: &pb.LogEntry_IntegrationState{IntegrationState: &pb.IntegrationStateRecord{Id: prefix + "-connector", Owner: "user:alice", Kind: "rehearsal-connector-attempt", Data: []byte(`{"pending":"preserve this historical connector state"}`), ExpiresAt: timestamppb.New(time.Now().Add(24 * time.Hour))}}}
		raw, err := proto.Marshal(entry)
		if err != nil {
			return err
		}
		if err := propose(raw); err != nil {
			return fmt.Errorf("connector state: %w", err)
		}
	}
	if mode == "seed" {
		if err := seedWorkload(ctx, conn, mem, propose, prefix, churn); err != nil {
			return err
		}
	}
	if err := verifyVectors(ctx, mem, prefix); err != nil {
		return err
	}
	search, err := mem.Search(ctx, &pb.SearchRequest{Embedding: vector(prefix, 0).Embedding, ScopeFilter: "upgrade-rehearsal", Limit: 100})
	if err != nil || len(search.GetHits()) < 64 {
		return fmt.Errorf("semantic search lost records: hits=%d err=%v", len(search.GetHits()), err)
	}
	records, err := mem.ListRecords(ctx, &pb.ListRecordsRequest{Kind: "episodic", Tag: prefix})
	if err != nil || len(records.GetEpisodics()) != 32 {
		return fmt.Errorf("episodic listing: count=%d err=%v", len(records.GetEpisodics()), err)
	}
	for _, rec := range records.Episodics {
		if rec.Owner != "user:alice" || !strings.Contains(rec.Event, "café, 日本語, 🦞") {
			return fmt.Errorf("episodic content/ownership changed: %s", rec.Id)
		}
	}
	plan, err := pb.NewPlanServiceClient(conn).GetPlan(ctx, &pb.GetPlanRequest{Window: durationpb.New(48 * time.Hour)})
	if err != nil {
		return fmt.Errorf("get plan: %w", err)
	}
	found := false
	for _, commitment := range plan.Commitments {
		if commitment.Id == prefix+"-commitment" && commitment.Status == "pending" && commitment.Owner == "user:alice" {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("pending commitment missing: %v", plan)
	}
	return emit(map[string]any{"mode": mode, "prefix": prefix, "vectors_verified": 64, "episodes_verified": 32, "search_hits": len(search.Hits), "commitment_verified": true, "churn_writes": churn})
}

func verifyVectors(ctx context.Context, mem pb.MemoryServiceClient, prefix string) error {
	for i := range 64 {
		expected := vector(prefix, i)
		out, err := mem.Recall(ctx, &pb.RecallRequest{Id: expected.Id})
		if err != nil {
			return fmt.Errorf("recall %s: %w", expected.Id, err)
		}
		got := out.Record
		if got.Text != expected.Text || got.Owner != expected.Owner || got.Scope != expected.Scope || got.Retention != expected.Retention || got.Metadata["test"] != expected.Metadata["test"] || len(got.Embedding) != len(expected.Embedding) {
			return fmt.Errorf("record %s changed: %v", expected.Id, got)
		}
		for j, value := range expected.Embedding {
			if got.Embedding[j] != value {
				return fmt.Errorf("embedding %s[%d] changed", expected.Id, j)
			}
		}
	}
	return nil
}

func seedWorkload(ctx context.Context, conn *grpc.ClientConn, mem pb.MemoryServiceClient, propose func([]byte) error, prefix string, churn int) error {
	for i := range 64 {
		rec := vector(prefix, i)
		if _, err := mem.Store(ctx, &pb.StoreRequest{Record: rec}); err != nil {
			return fmt.Errorf("store %s: %w", rec.Id, err)
		}
	}
	for i := range 32 {
		rec := &pb.EpisodicRecord{Id: fmt.Sprintf("%s-episode-%02d", prefix, i), Event: fmt.Sprintf("Rehearsal event %02d: café, 日本語, 🦞", i), Context: "An old install remembered this event", Importance: 7, Owner: "user:alice", Tags: []string{"upgrade-rehearsal", prefix}, Retention: pb.Retention_RETENTION_LONG_TERM}
		if _, err := mem.EpisodicAdd(ctx, &pb.EpisodicAddRequest{Record: rec}); err != nil {
			return fmt.Errorf("episodic add: %w", err)
		}
	}
	entries := []*pb.LogEntry{
		{Op: pb.LogOp_LOG_OP_PUT, Id: "profile:alice", Payload: &pb.LogEntry_Pinned{Pinned: &pb.PinnedMemory{Id: "profile:alice", UserId: "alice", Kind: "profile", Entries: []string{"UPGRADE-PINNED: Alice prefers concise answers", "Keep Unicode: café 日本語 🦞"}, UpdatedAt: timestamppb.Now()}}},
		{Op: pb.LogOp_LOG_OP_PUT, Id: prefix + "-credential", Payload: &pb.LogEntry_Credential{Credential: &pb.CredentialRecord{Id: prefix + "-credential", Provider: "rehearsal", Subject: "alice@example.invalid", AccessToken: []byte("synthetic-access-for-migration-test"), RefreshToken: []byte("synthetic-refresh-for-migration-test"), Owner: "user:alice", Scopes: []string{"read"}, CreatedAt: timestamppb.Now(), ExpiresAt: timestamppb.New(time.Now().Add(24 * time.Hour))}}},
		{Op: pb.LogOp_LOG_OP_PUT, Id: prefix + "-task", Payload: &pb.LogEntry_ScheduledTask{ScheduledTask: &pb.ScheduledTaskRecord{Id: prefix + "-task", Name: "Preserved disabled task", Schedule: "0 0 * * *", HandlerRef: "rehearsal-disabled", Owner: "user:alice", Enabled: false, CreatedAt: timestamppb.Now(), NextRun: timestamppb.New(time.Now().Add(24 * time.Hour))}}},
	}
	for _, entry := range entries {
		raw, err := proto.Marshal(entry)
		if err != nil {
			return err
		}
		if err := propose(raw); err != nil {
			return fmt.Errorf("propose %s: %w", entry.Id, err)
		}
	}
	_, err := pb.NewPlanServiceClient(conn).AddCommitment(ctx, &pb.AddCommitmentRequest{Commitment: &pb.AgentCommitment{Id: prefix + "-commitment", Reason: "Preserve this pending reminder", DueAt: timestamppb.New(time.Now().Add(24 * time.Hour)), Trigger: "time", Status: "pending", Owner: "user:alice", CreatedFor: "alice"}})
	if err != nil {
		return fmt.Errorf("add commitment: %w", err)
	}
	for i := range churn {
		rec := vector(prefix, 0)
		rec.Id = prefix + "-churn"
		rec.Text = fmt.Sprintf("Snapshot workload revision %d", i)
		if _, err := mem.Store(ctx, &pb.StoreRequest{Record: rec}); err != nil {
			return fmt.Errorf("snapshot churn %d: %w", i, err)
		}
	}
	return nil
}

func vector(prefix string, i int) *pb.VectorRecord {
	return &pb.VectorRecord{Id: fmt.Sprintf("%s-vector-%02d", prefix, i), Text: fmt.Sprintf("Historical memory %02d: café, 日本語, 🦞; multiline\nsecond line", i), Embedding: []float32{1, float32(i) / 64, 0.5, -0.25, 0, 0.125, 0.75, -1}, EmbeddingModel: "rehearsal-embedding", Metadata: map[string]string{"test": prefix, "source": "real-old-node"}, Scope: "upgrade-rehearsal", Retention: pb.Retention_RETENTION_LONG_TERM, Owner: "user:alice", Visibility: pb.Visibility_VISIBILITY_PRIVATE}
}

// Read raw bbolt data without opening a Store (which itself could migrate).
// Hash decrypted payloads and ciphertext, rather than disclosing credentials.
func dump(path string) error {
	keyBytes, err := base64.StdEncoding.DecodeString(os.Getenv("LOBSLAW_MEMORY_KEY"))
	if err != nil || len(keyBytes) != 32 {
		return fmt.Errorf("expected a 32-byte memory key: %v", err)
	}
	var key crypto.Key
	copy(key[:], keyBytes)
	db, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	result := map[string]map[string]any{}
	err = db.View(func(tx *bolt.Tx) error {
		return tx.ForEach(func(name []byte, bucket *bolt.Bucket) error {
			rows := map[string]any{}
			result[string(name)] = rows
			return bucket.ForEach(func(k, v []byte) error {
				if v == nil {
					return fmt.Errorf("unexpected nested bucket %s", name)
				}
				plain, err := crypto.Open(key, v)
				if err != nil {
					// Format metadata and certain derived indexes are plaintext.
					if string(name) != "data_format" && string(name) != "inbox_notification_keys_v1" && string(name) != "task_history_v1" {
						return fmt.Errorf("decrypt %s/%s: %w", name, k, err)
					}
					plain = v
				}
				payload := sha256.Sum256(plain)
				ciphertext := sha256.Sum256(v)
				row := map[string]any{"payload_sha256": hex.EncodeToString(payload[:]), "ciphertext_sha256": hex.EncodeToString(ciphertext[:])}
				if string(name) == "data_format" {
					row["format"] = json.RawMessage(plain)
				}
				rows[string(k)] = row
				return nil
			})
		})
	})
	if err != nil {
		return err
	}
	return emit(result)
}

func futureState(path string) error {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte("data_format"))
		if err != nil {
			return err
		}
		return bucket.Put([]byte("manifest"), []byte(`{"version":999,"protocol":"future-rehearsal"}`))
	})
}
