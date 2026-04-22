package discovery

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func newTestService(t *testing.T) (*Service, *Registry) {
	t.Helper()
	reg := NewRegistry()
	local := types.NodeInfo{ID: "local", Address: "127.0.0.1:0", Functions: []types.NodeFunction{types.FunctionMemory}}
	return NewService(reg, local, nil, nil), reg
}

func TestServiceRegister(t *testing.T) {
	t.Parallel()
	svc, reg := newTestService(t)
	resp, err := svc.Register(context.Background(), &lobslawv1.RegisterRequest{
		Node: &lobslawv1.NodeInfo{Id: "peer-1", Address: "10.0.0.1:9090"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Accepted {
		t.Errorf("Register rejected: %s", resp.Reason)
	}
	if _, ok := reg.Get("peer-1"); !ok {
		t.Error("peer-1 should be in registry")
	}
}

func TestServiceRegisterRejectsEmpty(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	resp, err := svc.Register(context.Background(), &lobslawv1.RegisterRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Accepted {
		t.Error("Register with nil Node should be rejected")
	}

	resp, err = svc.Register(context.Background(), &lobslawv1.RegisterRequest{
		Node: &lobslawv1.NodeInfo{Id: "", Address: "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Accepted {
		t.Error("Register with empty ID should be rejected")
	}
}

func TestServiceHeartbeatUnknownPeer(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	_, err := svc.Heartbeat(context.Background(), &lobslawv1.HeartbeatRequest{NodeId: "ghost"})
	if err == nil {
		t.Fatal("expected error")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.NotFound {
		t.Errorf("code = %v, want NotFound", st.Code())
	}
}

func TestServiceGetPeersIncludesLocal(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	// Register two peers.
	svc.Register(context.Background(), &lobslawv1.RegisterRequest{
		Node: &lobslawv1.NodeInfo{Id: "peer-1", Address: "a"},
	})
	svc.Register(context.Background(), &lobslawv1.RegisterRequest{
		Node: &lobslawv1.NodeInfo{Id: "peer-2", Address: "b"},
	})

	resp, err := svc.GetPeers(context.Background(), &lobslawv1.GetPeersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// Expect: local + 2 peers = 3.
	if len(resp.Peers) != 3 {
		t.Fatalf("want 3 peers (incl local), got %d", len(resp.Peers))
	}
	ids := map[string]bool{}
	for _, p := range resp.Peers {
		ids[p.Id] = true
	}
	for _, want := range []string{"local", "peer-1", "peer-2"} {
		if !ids[want] {
			t.Errorf("peer %q missing from GetPeers response", want)
		}
	}
}

func TestServiceReloadUnimplementedByDefault(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	_, err := svc.Reload(context.Background(), &lobslawv1.ReloadRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", st.Code())
	}
}

func TestServiceReloadDispatchesWhenWired(t *testing.T) {
	t.Parallel()
	var called bool
	reload := func(_ context.Context, _ []string) (reloaded, restart []string, errs map[string]string) {
		called = true
		return []string{"providers"}, nil, nil
	}
	reg := NewRegistry()
	svc := NewService(reg, types.NodeInfo{ID: "local"}, nil, reload)

	resp, err := svc.Reload(context.Background(), &lobslawv1.ReloadRequest{Sections: []string{"providers"}})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("reload function wasn't invoked")
	}
	if len(resp.Reloaded) != 1 || resp.Reloaded[0] != "providers" {
		t.Errorf("unexpected response: %v", resp)
	}
}
