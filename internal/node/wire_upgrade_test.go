package node

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"log/slog"
	"testing"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"

	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestUpgradeAuthoritySeparatesPeerReadAndOperatorWrite(t *testing.T) {
	store := crossOwnerTestStore(t)
	n := &Node{policyEngine: policy.NewEngine(store, slog.Default())}
	n.cfg.Users = []config.UserConfig{{ID: "alice", Roles: []string{"operator"}}}
	service := upgradeService{node: n}
	ctxFor := func(operator bool) context.Context {
		cert := &x509.Certificate{Subject: pkix.Name{CommonName: "alice"}}
		if operator {
			cert.Subject.OrganizationalUnit = []string{mtls.OperatorOU}
		}
		return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}}})
	}
	if err := service.authorize(context.Background(), false); err == nil {
		t.Fatal("anonymous status allowed")
	}
	if err := service.authorize(ctxFor(false), false); err != nil {
		t.Fatal("peer cannot negotiate", err)
	}
	if err := service.authorize(ctxFor(false), true); err == nil {
		t.Fatal("peer can activate cluster")
	}
	if err := service.authorize(ctxFor(true), true); err == nil {
		t.Fatal("certificate implied policy grant")
	}
	seedRule(t, store, &pb.PolicyRule{Id: "read", Subject: "role:operator", Action: "cluster.upgrade.read", Resource: "cluster:*", Effect: "allow", Priority: 50})
	if err := service.authorize(ctxFor(true), false); err != nil {
		t.Fatal(err)
	}
	if err := service.authorize(ctxFor(true), true); err == nil {
		t.Fatal("read grant implied write")
	}
	seedRule(t, store, &pb.PolicyRule{Id: "write", Subject: "role:operator", Action: "cluster.upgrade.write", Resource: "cluster:*", Effect: "allow", Priority: 50})
	if err := service.authorize(ctxFor(true), true); err != nil {
		t.Fatal(err)
	}
	n.cfg.Users = nil
	if err := service.authorize(ctxFor(true), true); err == nil {
		t.Fatal("grant without configured operator role")
	}
}
