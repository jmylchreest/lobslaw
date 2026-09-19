package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/jmylchreest/lobslaw/internal/identity"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type outageConsoleClient struct {
	lobslawv1.ConsoleServiceClient
	down atomic.Bool
}

func (c *outageConsoleClient) ConsoleForward(ctx context.Context, in *lobslawv1.ConsoleForwardRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[lobslawv1.ConsoleForwardResponse], error) {
	if c.down.Load() {
		return nil, status.Error(codes.Unavailable, "backend unavailable")
	}
	return c.ConsoleServiceClient.ConsoleForward(ctx, in, opts...)
}

func TestRemoteDiscoveryDistinguishesUnknownFromDisabled(t *testing.T) {
	t.Parallel()
	backend := startWebREST(t, &captureRunner{}, func(c *RESTConfig) { c.Bots = &memBots{} })
	client := &outageConsoleClient{ConsoleServiceClient: testConsoleClient(t, backend)}
	client.down.Store(true)
	front := startWebREST(t, nil, func(c *RESTConfig) { c.RemoteConsole = client })
	auth := http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}}
	resp := doJSON(t, http.MethodGet, webBaseURL(front)+"/v1/capabilities", "", auth)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatal("unknown backend capabilities were reported as disabled")
	}
	client.down.Store(false)
	resp = doJSON(t, http.MethodGet, webBaseURL(front)+"/v1/capabilities", "", auth)
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatal("capability discovery did not recover")
	}
	client.down.Store(true)
	resp = doJSON(t, http.MethodGet, webBaseURL(front)+"/v1/capabilities", "", auth)
	var caps capabilitiesResponse
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
		t.Fatal(err)
	}
	if !caps.ComputeTeams.Enabled || caps.ComputeTeams.Available {
		t.Fatalf("outage discarded known team capability: %+v", caps)
	}
}

type consolePeerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s consolePeerStream) Context() context.Context { return s.ctx }

func testConsoleClient(t *testing.T, backend *Server) lobslawv1.ConsoleServiceClient {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.StreamInterceptor(func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := peer.NewContext(stream.Context(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{}}}}}})
		return handler(srv, consolePeerStream{ServerStream: stream, ctx: ctx})
	}))
	lobslawv1.RegisterConsoleServiceServer(server, backend)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///console-test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return lobslawv1.NewConsoleServiceClient(conn)
}

func TestRemoteConsoleUsesBackendTeamsAndUserAuthorization(t *testing.T) {
	t.Parallel()
	runner := &captureRunner{}
	backend := startWebREST(t, runner, func(c *RESTConfig) {
		c.Bots = &memBots{recs: map[string]*lobslawv1.BotRecord{
			"alice-worker": {Id: "alice-worker", Owner: "user:alice", Enabled: true},
			"bob-worker":   {Id: "bob-worker", Owner: "user:bob", Enabled: true},
		}}
	})
	client := testConsoleClient(t, backend)
	front := startWebREST(t, nil, func(c *RESTConfig) { c.RemoteConsole = client })
	auth := http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}}
	capResp := doJSON(t, http.MethodGet, webBaseURL(front)+"/v1/capabilities", "", auth)
	var caps capabilitiesResponse
	if err := json.NewDecoder(capResp.Body).Decode(&caps); err != nil {
		t.Fatal(err)
	}
	if !caps.ComputeTeams.Enabled {
		t.Fatal("remote teams disappeared on a web-only node")
	}
	for _, bot := range []string{"alice-worker", "bob-worker"} {
		resp := doJSON(t, http.MethodPost, webBaseURL(front)+"/v1/bots/"+bot+"/messages", `{"message":"hello"}`, auth)
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if bot == "bob-worker" {
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("cross-owner forwarded request: %d", resp.StatusCode)
			}
		} else if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "event: reply") {
			t.Fatalf("remote chat: %d %s", resp.StatusCode, body)
		}
	}
	req := runner.lastRequest()
	if req.Claims.UserID != "alice" || req.Principal != identity.Bot("alice-worker") {
		t.Fatalf("identity changed across forwarding: %+v", req)
	}
	stream, err := client.ConsoleForward(context.Background(), &lobslawv1.ConsoleForwardRequest{Method: http.MethodPost, Path: "/v1/session/code", Claims: &lobslawv1.Claims{UserId: "alice"}})
	if err == nil {
		_, err = stream.Recv()
	}
	if err == nil {
		t.Fatal("login endpoint was exposed over forwarding")
	}
}
