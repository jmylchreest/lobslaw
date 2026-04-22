package discovery_test

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/jmylchreest/lobslaw/internal/discovery"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// testNode bundles the two nodes the integration test spins up.
type testNode struct {
	id       string
	addr     string
	listener net.Listener
	server   *grpc.Server
	registry *discovery.Registry
	local    types.NodeInfo
}

func (n *testNode) stop() {
	n.server.Stop()
	_ = n.listener.Close()
}

// TestSeedListExchangeTwoNodes stands up two gRPC servers on loopback,
// each with a NodeService + empty registry. Node A uses node B's
// address as a seed (and vice versa). After DialSeeds, both registries
// contain each other.
func TestSeedListExchangeTwoNodes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration in short mode")
	}

	a := newTestNode(t, "node-a")
	defer a.stop()
	b := newTestNode(t, "node-b")
	defer b.stop()

	// Start both servers.
	go func() { _ = a.server.Serve(a.listener) }()
	go func() { _ = b.server.Serve(b.listener) }()

	insecureDialer := func(ctx context.Context, addr string) (*grpc.ClientConn, error) {
		return grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	// Node A dials B as seed.
	clientA := discovery.NewClient(a.local, a.registry, insecureDialer, nil)
	if _, err := clientA.DialSeeds(context.Background(), []string{b.addr}, 2*time.Second); err != nil {
		t.Fatalf("A->B DialSeeds: %v", err)
	}

	// Node B dials A as seed.
	clientB := discovery.NewClient(b.local, b.registry, insecureDialer, nil)
	if _, err := clientB.DialSeeds(context.Background(), []string{a.addr}, 2*time.Second); err != nil {
		t.Fatalf("B->A DialSeeds: %v", err)
	}

	// A's registry should now include B.
	if _, ok := a.registry.Get("node-b"); !ok {
		t.Error("A's registry missing node-b after seed exchange")
	}
	// B's registry should now include A.
	if _, ok := b.registry.Get("node-a"); !ok {
		t.Error("B's registry missing node-a after seed exchange")
	}
}

// TestSeedListAllFailReturnsError proves the "all seeds unreachable"
// path surfaces an error rather than silently succeeding.
func TestSeedListAllFailReturnsError(t *testing.T) {
	t.Parallel()
	local := types.NodeInfo{ID: "lonely", Address: "127.0.0.1:0"}
	reg := discovery.NewRegistry()

	// Dialer that always fails immediately.
	failingDialer := func(ctx context.Context, addr string) (*grpc.ClientConn, error) {
		return nil, context.DeadlineExceeded
	}

	client := discovery.NewClient(local, reg, failingDialer, nil)
	failed, err := client.DialSeeds(context.Background(), []string{"10.255.255.1:9090", "10.255.255.2:9090"}, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected error when all seeds fail")
	}
	if len(failed) != 2 {
		t.Errorf("failed=%v, want both addresses", failed)
	}
}

func newTestNode(t *testing.T, id string) *testNode {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()

	registry := discovery.NewRegistry()
	local := types.NodeInfo{
		ID:        types.NodeID(id),
		Address:   addr,
		Functions: []types.NodeFunction{types.FunctionGateway},
	}

	server := grpc.NewServer()
	svc := discovery.NewService(registry, local, nil, nil, nil)
	lobslawv1.RegisterNodeServiceServer(server, svc)

	return &testNode{
		id:       id,
		addr:     addr,
		listener: ln,
		server:   server,
		registry: registry,
		local:    local,
	}
}
