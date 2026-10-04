package node

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/audit"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestUpgradeMutationsAuditAuthorityAndOutcome(t *testing.T) {
	store := crossOwnerTestStore(t)
	_, transport := raft.NewInmemTransport("audit-node")
	rn, err := memory.NewRaft(memory.RaftConfig{NodeID: "audit-node", LocalAddr: "audit-node", DataDir: t.TempDir(), Bootstrap: true, Transport: transport}, memory.NewFSM(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rn.Shutdown() })
	if err = rn.WaitForLeader(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	sink, err := audit.NewLocalSink(audit.LocalConfig{Path: filepath.Join(t.TempDir(), "audit.jsonl")})
	if err != nil {
		t.Fatal(err)
	}
	log, err := audit.NewAuditLog(context.Background(), audit.Config{Sinks: []audit.AuditSink{sink}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	n := &Node{raft: rn, auditLog: log, log: slog.Default(), policyEngine: policy.NewEngine(store, slog.Default())}
	n.cfg.Users = []config.UserConfig{{ID: "alice", Roles: []string{"operator"}}}
	service := upgradeService{node: n}
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: "alice", OrganizationalUnit: []string{mtls.OperatorOU}}}
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}}})
	req := &pb.ChangeUpgradeRequest{Action: "transfer", TargetNodeId: "audit-node"}
	if _, err = service.ChangeUpgrade(ctx, req); err == nil {
		t.Fatal("missing grant accepted")
	}
	seedRule(t, store, &pb.PolicyRule{Id: "upgrade-write", Subject: "role:operator", Action: "cluster.upgrade.write", Resource: "cluster:*", Effect: "allow", Priority: 50})
	if _, err = service.ChangeUpgrade(ctx, req); err != nil {
		t.Fatal(err)
	}
	entries, err := log.Query(ctx, "local", types.AuditFilter{Action: "cluster.upgrade.write"})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("want denied, admitted and completed audit; got %+v", entries)
	}
	for _, e := range entries {
		if e.ActorScope != "operator:alice" {
			t.Fatalf("actor missing: %+v", e)
		}
	}
	if entries[0].Effect != types.EffectDeny || entries[1].PolicyRule != "upgrade-write" || entries[2].PolicyRule != "upgrade-write" || entries[2].ResultHash == "" {
		t.Fatalf("missing grant/outcome: %+v", entries)
	}
}

func TestUpgradeRefusesMutationWhenAdmissionCannotBeAudited(t *testing.T) {
	store := crossOwnerTestStore(t)
	seedRule(t, store, &pb.PolicyRule{Id: "write", Subject: "role:operator", Action: "cluster.upgrade.write", Resource: "cluster:*", Effect: "allow", Priority: 50})
	n := &Node{policyEngine: policy.NewEngine(store, slog.Default())}
	n.cfg.Users = []config.UserConfig{{ID: "alice", Roles: []string{"operator"}}}
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: "alice", OrganizationalUnit: []string{mtls.OperatorOU}}}
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}}})
	// Raft is intentionally nil: reaching mutation dispatch would panic.
	_, err := (&upgradeService{node: n}).ChangeUpgrade(ctx, &pb.ChangeUpgradeRequest{Action: "transfer", TargetNodeId: "other"})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("mutation not refused at audit admission: %v", err)
	}
}
