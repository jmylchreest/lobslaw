package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/hashicorp/raft"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/archive"
	"github.com/jmylchreest/lobslaw/internal/backup"
	"github.com/jmylchreest/lobslaw/internal/grpcinterceptors"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Exercise the actual CLI, encrypted repository, mTLS transport and Raft writes
// together: deployment trust can change independently of restored ownership.
func TestBackupRestoreWithNewCertificatesAndOwners(t *testing.T) {
	// A developer's default node must not override the recovery context.
	t.Setenv("LOBSLAW_NODE_ADDR", "")
	source := backupTestCredentials(t, "old-node", "alice")
	destination := backupTestCredentials(t, "recovered-node", "alice-new")
	store, service := backupTestService(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(destination.server.ServerCreds()))
	lobslawv1.RegisterArchiveServiceServer(server, memory.NewArchiveRPC(service,
		func(ctx context.Context, action string) error {
			cert := grpcinterceptors.VerifiedPeerCert(ctx)
			if cert == nil || !mtls.IsOperatorCert(cert) || cert.Subject.CommonName != "alice-new" || action != "archive:import" {
				return status.Error(codes.PermissionDenied, "recovery operator required")
			}
			return nil
		}, nil))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	dir := t.TempDir()
	contexts := fmt.Sprintf(`[contexts.recovered]
addr = %q
ca_cert = %q
cert = %q
key = %q
`, listener.Addr().String(), destination.ca, destination.cert, destination.key)
	contextsPath := filepath.Join(dir, "contexts.toml")
	if err := os.WriteFile(contextsPath, []byte(contexts), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOBSLAW_CONTEXTS", contextsPath)

	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	identityPath := filepath.Join(dir, "backup-key.txt")
	if err := os.WriteFile(identityPath, []byte(identity.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := func(kind, id string, msg proto.Message) archive.Record {
		data, err := protojson.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		return archive.Record{Kind: kind, ID: id, Data: data}
	}
	snapshot := archive.Snapshot{
		Manifest: archive.Manifest{SnapshotID: "before-rotation", SourceID: "original-deployment", CreatedAt: time.Now().UTC()},
		Records: []archive.Record{
			record("preferences", "user:alice", &lobslawv1.UserPreferences{UserId: "user:alice"}),
			record("pinned", "notes:user:alice", &lobslawv1.PinnedMemory{Id: "notes:user:alice", Kind: "notes", UserId: "user:alice"}),
			record("sessions", "conversation", &lobslawv1.SessionRecord{Id: "conversation", UserId: "alice"}),
		},
	}
	repo := backup.Repository{Path: filepath.Join(dir, "backups")}
	if _, err := repo.Create(snapshot, identity.Recipient()); err != nil {
		t.Fatal(err)
	}
	base := []string{"before-rotation", "--repository", repo.Path, "--identity", identityPath,
		"--context", "recovered", "--server-name", "recovered-node", "--timeout", "2s"}
	run := func(extra ...string) error {
		return backupRestore(append(append([]string(nil), base...), extra...))
	}
	assertEmpty := func() {
		t.Helper()
		records, err := store.ArchiveRecords(context.Background())
		if err != nil || len(records) != 0 {
			t.Fatalf("restore wrote before a valid apply: %d records, %v", len(records), err)
		}
	}
	for _, stale := range [][]string{
		{"--ca-cert", source.ca},
		{"--node-cert", source.cert, "--node-key", source.key},
	} {
		err := run(append(stale, "--owner", "user:alice=user:alice-new", "--owner", "alice=alice-new", "--apply")...)
		backupTestRPCError(t, err, codes.Unavailable, "tls:")
		assertEmpty()
	}
	if err := run("--apply"); err == nil || !strings.Contains(err.Error(), "explicit owner mapping required") {
		t.Fatalf("missing owner mapping: %v", err)
	}
	assertEmpty()
	backupTestRPCError(t, run("--owner", "user:alice=user:alice-new", "--apply"),
		codes.InvalidArgument, `explicit owner mapping required for "alice"`)
	assertEmpty()
	if err := run("--owner", "user:alice=user:alice-new", "--owner", "alice=alice-new"); err != nil {
		t.Fatalf("preview with new trust and owner: %v", err)
	}
	assertEmpty()
	for i := 0; i < 2; i++ {
		if err := run("--owner", "user:alice=user:alice-new", "--owner", "alice=alice-new", "--apply"); err != nil {
			t.Fatalf("apply/repeat %d: %v", i, err)
		}
	}
	want := []struct {
		bucket string
		id     string
		msg    proto.Message
	}{
		{memory.BucketUserPrefs, "user:alice-new", &lobslawv1.UserPreferences{UserId: "user:alice-new"}},
		{memory.BucketPinned, "notes:user:alice-new", &lobslawv1.PinnedMemory{Id: "notes:user:alice-new", Kind: "notes", UserId: "user:alice-new", Revision: 1}},
		{memory.BucketSessions, "conversation", &lobslawv1.SessionRecord{Id: "conversation", UserId: "alice-new"}},
	}
	for _, check := range want {
		data, err := store.Get(check.bucket, check.id)
		if err != nil {
			t.Fatal(err)
		}
		got := check.msg.ProtoReflect().New().Interface()
		if err := proto.Unmarshal(data, got); err != nil || !proto.Equal(got, check.msg) {
			t.Fatalf("restored %s/%s = %v, want %v: %v", check.bucket, check.id, got, check.msg, err)
		}
	}
	for _, old := range want[:2] {
		if _, err := store.Get(old.bucket, strings.ReplaceAll(old.id, "alice-new", "alice")); err == nil {
			t.Fatal("left an old owner-keyed record behind")
		}
	}
	backupTestRPCError(t, run("--owner", "user:alice=user:someone-else", "--owner", "alice=someone-else", "--apply"),
		codes.FailedPrecondition, "requires an empty knowledge store or its own partial restore")
}

func backupTestRPCError(t *testing.T, err error, code codes.Code, message string) {
	t.Helper()
	if err == nil || status.Code(err) != code || !strings.Contains(err.Error(), message) {
		t.Fatalf("expected %s containing %q, got: %v", code, message, err)
	}
}

type backupCredentials struct {
	ca, cert, key string
	server        *mtls.NodeCreds
}

func backupTestCredentials(t *testing.T, nodeID, operator string) backupCredentials {
	t.Helper()
	dir := t.TempDir()
	caPath, caKeyPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem")
	caPEM, caKeyPEM, err := mtls.GenerateCA(mtls.CAOpts{CommonName: nodeID + "-ca"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mtls.WriteCAFiles(caPath, caKeyPath, caPEM, caKeyPEM); err != nil {
		t.Fatal(err)
	}
	ca, caKey, err := mtls.LoadCA(caPath, caKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	nodeCert, nodeKey := filepath.Join(dir, "node.pem"), filepath.Join(dir, "node-key.pem")
	certPEM, keyPEM, err := mtls.SignNodeCert(ca, caKey, mtls.SignOpts{NodeID: nodeID})
	if err != nil {
		t.Fatal(err)
	}
	if err := mtls.WriteNodeFiles(nodeCert, nodeKey, certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	creds, err := mtls.LoadNodeCreds(caPath, nodeCert, nodeKey)
	if err != nil {
		t.Fatal(err)
	}
	opCAPath := filepath.Join(dir, "operator-ca.pem")
	opCA, opKey, err := mtls.LoadOrCreateOperatorCA(opCAPath, filepath.Join(dir, "operator-ca-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	opPEM, err := os.ReadFile(opCAPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := creds.TrustOperatorCA(opPEM); err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err = mtls.SignOperatorCert(opCA, opKey, mtls.SignOpts{NodeID: operator})
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(dir, "operator.pem"), filepath.Join(dir, "operator-key.pem")
	if err := mtls.WriteNodeFiles(certPath, keyPath, certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	return backupCredentials{ca: caPath, cert: certPath, key: keyPath, server: creds}
}

func backupTestService(t *testing.T) (*memory.Store, *memory.Service) {
	t.Helper()
	dir := t.TempDir()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(dir, "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, transport := raft.NewInmemTransport("recovery")
	node, err := memory.NewRaft(memory.RaftConfig{
		NodeID: "recovery", LocalAddr: "recovery", DataDir: dir,
		Bootstrap: true, Transport: transport,
	}, memory.NewFSM(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = node.Shutdown() })
	if err := node.WaitForLeader(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	return store, memory.NewService(node, store, nil)
}
