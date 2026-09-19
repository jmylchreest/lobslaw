package compute

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type recordingRunner struct {
	last     turn.Request
	approved bool
}

func (r *recordingRunner) Run(_ context.Context, req turn.Request) (*turn.Response, error) {
	r.last = req
	return &turn.Response{Reply: "hello " + req.Claims.UserID}, nil
}

func (r *recordingRunner) Resume(ctx context.Context, req turn.Request, _ []turn.Message) (*turn.Response, error) {
	r.last = req
	r.approved = turn.Approved(ctx, "tool:exec", "read_file") && !turn.Approved(ctx, "tool:exec", "read_file")
	return &turn.Response{Reply: "resumed " + req.Claims.UserID}, nil
}

func TestRemoteResumeTransfersApprovalOnce(t *testing.T) {
	t.Parallel()
	rec := &recordingRunner{}
	remote := &RemoteRunner{client: startTurnRPC(t, rec)}
	ctx := turn.WithTurnApproval(context.Background(), "tool:exec", "read_file")
	req := turn.Request{Claims: &types.Claims{UserID: "alice"}}
	if _, err := remote.Resume(ctx, req, nil); err != nil {
		t.Fatal(err)
	}
	if !rec.approved || turn.ApprovalPending(ctx) {
		t.Fatal("approval was not transferred exactly once")
	}
	if _, err := remote.Resume(ctx, req, nil); err != nil {
		t.Fatal(err)
	}
	if rec.approved {
		t.Fatal("approval replayed across RPCs")
	}
}

func startTurnRPC(t *testing.T, runner turn.Runner) lobslawv1.AgentServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	lobslawv1.RegisterAgentServiceServer(srv, NewTurnServer(runner))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///turn-rpc",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return lobslawv1.NewAgentServiceClient(conn)
}

func TestRemoteTurnPreservesAuthenticatedUser(t *testing.T) {
	t.Parallel()
	rec := &recordingRunner{}
	client := startTurnRPC(t, rec)
	remote := &RemoteRunner{client: client}

	resp, err := remote.Run(context.Background(), turn.Request{
		Message:   "hi",
		Claims:    &types.Claims{UserID: "alice", Scope: "household", Roles: []string{"member"}},
		Principal: identity.User("alice"),
		TurnID:    "t1",
		Channel:   "rest",
		ChannelID: "alice:default",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Reply != "hello alice" {
		t.Fatalf("reply = %+v", resp)
	}
	if rec.last.Claims == nil || rec.last.Claims.UserID != "alice" {
		t.Fatalf("server saw claims %+v, want alice — the peer node must not become the user", rec.last.Claims)
	}
	if rec.last.Principal != identity.User("alice") {
		t.Errorf("principal = %q, want user:alice", rec.last.Principal)
	}
	if rec.last.Claims.UserID == "passthrough:///turn-rpc" {
		t.Fatal("peer address was substituted for the user")
	}
}

func TestRemoteTurnRefusesEmptyClaims(t *testing.T) {
	t.Parallel()
	client := startTurnRPC(t, &recordingRunner{})
	_, err := client.RunTurn(context.Background(), &lobslawv1.RunTurnRequest{Message: "hi"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty claims: %v, want InvalidArgument", err)
	}
}

func TestRemoteTurnRefusesBlankUserID(t *testing.T) {
	t.Parallel()
	client := startTurnRPC(t, &recordingRunner{})
	_, err := client.RunTurn(context.Background(), &lobslawv1.RunTurnRequest{
		Message: "hi",
		Claims:  &lobslawv1.Claims{UserId: ""},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("blank user_id: %v, want InvalidArgument", err)
	}
}

func TestRemoteResumePreservesUser(t *testing.T) {
	t.Parallel()
	rec := &recordingRunner{}
	remote := &RemoteRunner{client: startTurnRPC(t, rec)}
	resp, err := remote.Resume(context.Background(), turn.Request{
		Message: "again",
		Claims:  &types.Claims{UserID: "bob"},
	}, []turn.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Reply != "resumed bob" {
		t.Errorf("reply = %q", resp.Reply)
	}
	if rec.last.Claims.UserID != "bob" {
		t.Errorf("server user = %q, want bob", rec.last.Claims.UserID)
	}
}

func TestRemoteAvailableWhenPingSucceeds(t *testing.T) {
	t.Parallel()
	remote := &RemoteRunner{client: startTurnRPC(t, &recordingRunner{})}
	if !remote.Available(context.Background()) {
		t.Fatal("reachable backend must report available")
	}
}

func TestRemoteUnavailableWhenBackendMissing(t *testing.T) {
	t.Parallel()
	if (&RemoteRunner{}).Available(context.Background()) {
		t.Fatal("nil client must not report available")
	}
}

func TestTurnIdentityPrefersRequestPrincipal(t *testing.T) {
	t.Parallel()
	agent, err := NewAgent(AgentConfig{
		Provider: NewMockProvider(MockResponse{Content: "ok"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := agent.TurnIdentityFor(ProcessMessageRequest{
		Claims:    &types.Claims{UserID: "alice"},
		Principal: identity.User("alice"),
	})
	if got.Principal != identity.User("alice") {
		t.Errorf("principal = %q, want user:alice", got.Principal)
	}
	if got.UserID != "alice" {
		t.Errorf("user = %q", got.UserID)
	}
}

func TestPingFailsWithoutLocalCompute(t *testing.T) {
	t.Parallel()
	client := startTurnRPC(t, nil)
	_, err := client.Ping(context.Background(), &lobslawv1.PingRequest{})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ping with no runner: %v, want FailedPrecondition", err)
	}
}
