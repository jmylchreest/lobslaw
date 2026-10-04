package node

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"log/slog"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
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

// Exercise the registered stage, including its function gate: memory-only
// nodes serve upgrade RPCs even though they have no compute approval gates.
func wireUpgradeTestDefaults(t *testing.T, n *Node) {
	t.Helper()
	for _, stage := range nodeWireStages() {
		if stage.Name == "policy-defaults" {
			if err := n.runWireStages([]WireStage{stage}); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("policy defaults stage missing")
}

func TestOperatorUpgradeDefaults(t *testing.T) {
	for _, functions := range [][]types.NodeFunction{
		{types.FunctionMemory},
		{types.FunctionMemory, types.FunctionCompute},
	} {
		t.Run(string(functions[len(functions)-1]), func(t *testing.T) {
			n := &Node{policyEngine: policy.NewEngine(crossOwnerTestStore(t), slog.Default()), log: slog.Default()}
			if len(functions) > 1 {
				n, _ = approvalGateNode(t, true, true)
			}
			n.cfg.Functions = functions
			n.cfg.Users = []config.UserConfig{{ID: "alice", Roles: []string{"operator"}}}
			wireUpgradeTestDefaults(t, n)
			service := upgradeService{node: n}
			cert := &x509.Certificate{Subject: pkix.Name{CommonName: "alice", OrganizationalUnit: []string{mtls.OperatorOU}}}
			ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}}})
			for _, write := range []bool{false, true} {
				decision, err := service.authorizeDecision(ctx, write)
				if err != nil || decision.Effect != types.EffectAllow || decision.RuleID == "" {
					t.Fatalf("operator default write=%v: decision=%+v err=%v", write, decision, err)
				}
			}
			if gateCompute(n.cfg) {
				for action, resource := range map[string]string{compute.MemoryWriteAction: "episodic", compute.ShellAction: "git status"} {
					if effect := effectFor(t, n.policyEngine, action, resource); effect != types.EffectRequireConfirmation {
						t.Fatalf("approval default lost for %s: %s", action, effect)
					}
				}
			}
			// A valid operator role cannot replace the certificate boundary.
			peerCert := &x509.Certificate{Subject: pkix.Name{CommonName: "alice"}}
			peerCtx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{peerCert}}}}})
			if err := service.authorize(peerCtx, false); err != nil {
				t.Fatalf("peer status denied: %v", err)
			}
			if err := service.authorize(peerCtx, true); status.Code(err) != codes.PermissionDenied {
				t.Fatalf("peer mutation allowed: %v", err)
			}
			unverified := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}}})
			for _, invalidCtx := range []context.Context{context.Background(), unverified} {
				for _, write := range []bool{false, true} {
					if err := service.authorize(invalidCtx, write); status.Code(err) != codes.PermissionDenied {
						t.Fatalf("unverified request allowed: %v", err)
					}
				}
			}
			engine := n.policyEngine
			n.policyEngine = nil
			if err := service.authorize(ctx, true); status.Code(err) != codes.PermissionDenied {
				t.Fatalf("missing policy engine allowed mutation: %v", err)
			}
			n.policyEngine = engine
			n.cfg.Users = nil
			for _, write := range []bool{false, true} {
				if err := service.authorize(ctx, write); status.Code(err) != codes.PermissionDenied {
					t.Fatalf("certificate without configured operator role write=%v: %v", write, err)
				}
			}
		})
	}
}

func TestOperatorUpgradeDefaultOverrides(t *testing.T) {
	for _, deniedAction := range []string{"cluster.upgrade.read", "cluster.upgrade.write"} {
		t.Run(deniedAction, func(t *testing.T) {
			store := crossOwnerTestStore(t)
			n := &Node{policyEngine: policy.NewEngine(store, slog.Default()), log: slog.Default()}
			n.cfg.Functions = []types.NodeFunction{types.FunctionMemory}
			n.cfg.Users = []config.UserConfig{{ID: "alice", Roles: []string{"operator"}}}
			wireUpgradeTestDefaults(t, n)
			// Even the lowest-priority stored rule outranks a shipped fallback.
			seedRule(t, store, &pb.PolicyRule{Id: "deny-upgrade", Subject: "role:operator", Action: deniedAction, Resource: "cluster:*", Effect: "deny", Priority: -1 << 31})
			service := upgradeService{node: n}
			cert := &x509.Certificate{Subject: pkix.Name{CommonName: "alice", OrganizationalUnit: []string{mtls.OperatorOU}}}
			ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}}})
			for _, write := range []bool{false, true} {
				decision, err := service.authorizeDecision(ctx, write)
				denied := write == (deniedAction == "cluster.upgrade.write")
				if denied {
					if status.Code(err) != codes.PermissionDenied || decision.RuleID != "deny-upgrade" {
						t.Fatalf("deny not enforced: %+v %v", decision, err)
					}
				} else if err != nil {
					t.Fatalf("deny affected other action: %v", err)
				}
			}
		})
	}
}
