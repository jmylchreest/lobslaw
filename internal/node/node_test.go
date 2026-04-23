package node_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/node"
	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// signNodeCert generates CA + one signed node cert into tmp; returns
// the Creds ready to hand to node.New.
func mustKey(t *testing.T) crypto.Key {
	t.Helper()
	k, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func signNodeCert(t *testing.T, tmp, nodeID string) *mtls.NodeCreds {
	t.Helper()
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatalf("mkdir certs: %v", err)
	}
	caCertPath := filepath.Join(tmp, "ca.pem")
	caKeyPath := filepath.Join(tmp, "ca-key.pem")
	caCertPEM, caKeyPEM, err := mtls.GenerateCA(mtls.CAOpts{CommonName: "node-test-ca"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mtls.WriteCAFiles(caCertPath, caKeyPath, caCertPEM, caKeyPEM); err != nil {
		t.Fatal(err)
	}
	ca, caKey, err := mtls.LoadCA(caCertPath, caKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := mtls.SignNodeCert(ca, caKey, mtls.SignOpts{NodeID: nodeID})
	if err != nil {
		t.Fatal(err)
	}
	nodeCertPath := filepath.Join(tmp, nodeID+".cert.pem")
	nodeKeyPath := filepath.Join(tmp, nodeID+".key.pem")
	if err := mtls.WriteNodeFiles(nodeCertPath, nodeKeyPath, certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	creds, err := mtls.LoadNodeCreds(caCertPath, nodeCertPath, nodeKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	return creds
}

// clientCreds builds TLS creds for dialing an mtls-serving node. The
// loopback address doesn't match any cert SAN, so we skip ServerName
// matching but still verify the peer against the cluster CA.
func clientCreds(t *testing.T, creds *mtls.NodeCreds) credentials.TransportCredentials {
	t.Helper()
	return credentials.NewTLS(&tls.Config{
		Certificates:       []tls.Certificate{creds.NodeCert},
		RootCAs:            creds.CAPool,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("no peer certs")
			}
			peer, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			_, err = peer.Verify(x509.VerifyOptions{Roots: creds.CAPool})
			return err
		},
	})
}

// TestNewRejectsInvalidConfig covers the config-validation error
// paths that matter for operators.
func TestNewRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	creds := signNodeCert(t, t.TempDir(), "node-1")

	cases := []struct {
		name string
		cfg  node.Config
	}{
		{
			name: "missing node id",
			cfg:  node.Config{ListenAddr: "127.0.0.1:0", Creds: creds},
		},
		{
			name: "missing listen addr",
			cfg:  node.Config{NodeID: "n", Creds: creds},
		},
		{
			name: "missing creds",
			cfg:  node.Config{NodeID: "n", ListenAddr: "127.0.0.1:0"},
		},
		{
			name: "memory without storage",
			cfg: node.Config{
				NodeID:     "n",
				ListenAddr: "127.0.0.1:0",
				Creds:      creds,
				DataDir:    t.TempDir(),
				Functions:  []types.NodeFunction{types.FunctionMemory},
			},
		},
		{
			name: "memory with no snapshot target and no seeds",
			cfg: node.Config{
				NodeID:     "n",
				ListenAddr: "127.0.0.1:0",
				Creds:      creds,
				DataDir:    t.TempDir(),
				Functions:  []types.NodeFunction{types.FunctionMemory, types.FunctionStorage},
				MemoryKey:  mustKey(t),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := node.New(tc.cfg); err == nil {
				t.Errorf("New(%+v) should have errored", tc.cfg)
			}
		})
	}
}

// TestSingleNodeStartAndPolicyRoundTrip is the Phase 2.6 headline
// exit-criteria test: a node boots with memory+policy+storage
// enabled, accepts a PolicyService.AddRule RPC over mTLS gRPC, and
// the rule is readable back after a full Shutdown → new Node → restart
// cycle (proving the raft log + state.db survived).
func TestSingleNodeStartAndPolicyRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping single-node integration in short mode")
	}

	tmp := t.TempDir()
	certDir := filepath.Join(tmp, "certs")
	dataDir := filepath.Join(tmp, "data")
	nodeID := "single-node"
	creds := signNodeCert(t, certDir, nodeID)

	memoryKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	// Boot #1: bootstrap a fresh cluster, apply one rule, shut down.
	runApply := func(cfgOverride func(*node.Config)) *lobslawv1.PolicyRule {
		cfg := node.Config{
			NodeID:         nodeID,
			Functions:      []types.NodeFunction{types.FunctionMemory, types.FunctionPolicy, types.FunctionStorage},
			ListenAddr:     "127.0.0.1:0",
			DataDir:        dataDir,
			Bootstrap:      true,
			SnapshotTarget: "storage:test-backup",
			Creds:          creds,
			MemoryKey:      memoryKey,
		}
		if cfgOverride != nil {
			cfgOverride(&cfg)
		}

		n, err := node.New(cfg)
		if err != nil {
			t.Fatalf("node.New: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			close(started)
			done <- n.Start(ctx)
		}()
		<-started

		// Wait for leadership — single-node bootstrap is usually instant.
		if err := n.Raft().WaitForLeader(5 * time.Second); err != nil {
			cancel()
			<-done
			t.Fatalf("WaitForLeader: %v", err)
		}

		// AddRule via gRPC.
		conn, err := grpc.NewClient(n.ListenAddr(), grpc.WithTransportCredentials(clientCreds(t, creds)))
		if err != nil {
			cancel()
			<-done
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()

		client := lobslawv1.NewPolicyServiceClient(conn)
		rule := &lobslawv1.PolicyRule{
			Id:       "test-rule-1",
			Subject:  "user:alice",
			Action:   "memory:read",
			Resource: "*",
			Effect:   "allow",
		}
		resp, err := client.AddRule(ctx, &lobslawv1.AddRuleRequest{Rule: rule})
		if err != nil {
			cancel()
			<-done
			t.Fatalf("AddRule: %v", err)
		}
		if resp.Id != rule.Id {
			t.Errorf("AddRule returned id %q, want %q", resp.Id, rule.Id)
		}

		// Read back via Policy() — same store.
		got, err := n.Policy().GetRule(rule.Id)
		if err != nil {
			cancel()
			<-done
			t.Fatalf("GetRule immediately after AddRule: %v", err)
		}
		if got.Subject != rule.Subject {
			t.Errorf("subject = %q, want %q", got.Subject, rule.Subject)
		}

		// Shutdown.
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
		return got
	}

	rule := runApply(nil)

	// Boot #2: same data-dir, no bootstrap (raft.ErrCantBootstrap is
	// silently swallowed). The rule must be visible via GetRule.
	cfg := node.Config{
		NodeID:         nodeID,
		Functions:      []types.NodeFunction{types.FunctionMemory, types.FunctionPolicy, types.FunctionStorage},
		ListenAddr:     "127.0.0.1:0",
		DataDir:        dataDir,
		Bootstrap:      true, // harmless on restart thanks to ErrCantBootstrap handling
		SnapshotTarget: "storage:test-backup",
		Creds:          creds,
		MemoryKey:      memoryKey,
	}
	n2, err := node.New(cfg)
	if err != nil {
		t.Fatalf("restart: node.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- n2.Start(ctx)
	}()
	<-started

	if err := n2.Raft().WaitForLeader(5 * time.Second); err != nil {
		t.Fatalf("WaitForLeader on restart: %v", err)
	}

	got, err := n2.Policy().GetRule(rule.Id)
	if err != nil {
		t.Fatalf("after restart: GetRule: %v", err)
	}
	if got.Subject != rule.Subject {
		t.Errorf("after restart: subject %q, want %q", got.Subject, rule.Subject)
	}

	// Confirm it's actually serialised — non-empty marshalled form.
	if raw, err := proto.Marshal(got); err != nil || len(raw) == 0 {
		t.Errorf("marshal round-trip: len=%d err=%v", len(raw), err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("second Start: %v", err)
	}
}

// TestNodeBootsGatewayHTTPServer is the Phase 6h exit-criterion test:
// a node constructed with FunctionGateway + FunctionCompute enabled
// AND cfg.Gateway.Enabled = true must actually serve HTTP. Proves the
// cmd/lobslaw → node.New → wireGateway → gateway.Server chain is
// intact, not just that the handlers work in isolation.
func TestNodeBootsGatewayHTTPServer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping gateway boot integration in short mode")
	}

	tmp := t.TempDir()
	nodeID := "gateway-boot-node"
	creds := signNodeCert(t, filepath.Join(tmp, "certs"), nodeID)

	// Mock LLM so the node's agent can answer without a real provider.
	// injects via node.Config.LLMProvider — same path the binary would
	// use if an operator wired a test-mode provider.
	mockProvider := compute.NewMockProvider(compute.MockResponse{Content: "pong"})

	cfg := node.Config{
		NodeID:     nodeID,
		Functions:  []types.NodeFunction{types.FunctionCompute, types.FunctionGateway},
		ListenAddr: "127.0.0.1:0",
		Creds:      creds,
		LLMProvider: mockProvider,
		Gateway: config.GatewayConfig{
			Enabled:          true,
			HTTPPort:         0, // OS-picked ephemeral port
			UnknownUserScope: "public",
			// No channels configured — REST-only deployment.
		},
	}

	n, err := node.New(cfg)
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	if n.Gateway() == nil {
		t.Fatal("Gateway() nil even though cfg.Gateway.Enabled=true")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Start(ctx) }()

	// Wait for the gateway to bind. Gateway.Addr() returns empty until
	// then, so poll.
	deadline := time.Now().Add(3 * time.Second)
	for n.Gateway().Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n.Gateway().Addr() == "" {
		cancel()
		<-done
		t.Fatal("gateway didn't bind within 3s")
	}

	base := "http://" + n.Gateway().Addr()

	// /healthz — always 200 while the process is up.
	{
		resp, err := http.Get(base + "/healthz")
		if err != nil {
			cancel()
			<-done
			t.Fatalf("GET /healthz: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			cancel()
			<-done
			t.Errorf("/healthz: got %d", resp.StatusCode)
		}
	}

	// /readyz — 200 once the agent is wired.
	{
		resp, err := http.Get(base + "/readyz")
		if err != nil {
			cancel()
			<-done
			t.Fatalf("GET /readyz: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			cancel()
			<-done
			t.Errorf("/readyz with agent wired should 200; got %d", resp.StatusCode)
		}
	}

	// /v1/messages — full round-trip through the real agent + mock LLM.
	{
		resp, err := http.Post(base+"/v1/messages", "application/json",
			strings.NewReader(`{"message":"ping"}`))
		if err != nil {
			cancel()
			<-done
			t.Fatalf("POST /v1/messages: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			cancel()
			<-done
			t.Fatalf("/v1/messages: got %d body=%s", resp.StatusCode, body)
		}
		var out struct {
			Reply string `json:"reply"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			cancel()
			<-done
			t.Fatal(err)
		}
		if out.Reply != "pong" {
			cancel()
			<-done
			t.Errorf("reply didn't round-trip through the agent: %q", out.Reply)
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start returned: %v", err)
	}
}

// TestNodeGatewayDisabledWhenFlagOff — a node that enables the
// gateway function at boot but leaves cfg.Gateway.Enabled=false MUST
// NOT bind an HTTP port. Prevents deployments from serving channel
// endpoints by accident after enabling the function.
func TestNodeGatewayDisabledWhenFlagOff(t *testing.T) {
	t.Parallel()

	nodeID := "gateway-off-node"
	creds := signNodeCert(t, t.TempDir(), nodeID)

	cfg := node.Config{
		NodeID:     nodeID,
		Functions:  []types.NodeFunction{types.FunctionCompute, types.FunctionGateway},
		ListenAddr: "127.0.0.1:0",
		Creds:      creds,
		LLMProvider: compute.NewMockProvider(compute.MockResponse{Content: "unused"}),
		Gateway: config.GatewayConfig{
			Enabled: false, // explicit opt-out
		},
	}

	n, err := node.New(cfg)
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	if n.Gateway() != nil {
		t.Error("Gateway() non-nil when cfg.Gateway.Enabled=false")
	}
}

// TestNodeGatewayRejectsMissingAgent — an operator who enables the
// gateway without the compute function is misconfiguring the node
// (no agent to dispatch to). Fail construction loudly.
func TestNodeGatewayRejectsMissingAgent(t *testing.T) {
	t.Parallel()

	nodeID := "gateway-no-compute-node"
	creds := signNodeCert(t, t.TempDir(), nodeID)

	cfg := node.Config{
		NodeID:     nodeID,
		Functions:  []types.NodeFunction{types.FunctionGateway}, // no Compute
		ListenAddr: "127.0.0.1:0",
		Creds:      creds,
		Gateway:    config.GatewayConfig{Enabled: true},
	}

	if _, err := node.New(cfg); err == nil {
		t.Error("gateway without compute should fail node.New; it didn't")
	}
}

// TestNodeGatewayTelegramChannelConstructed — a gateway with a
// telegram channel listed (plus secrets via ChannelSecretResolver)
// actually constructs the handler and mounts /telegram on the mux.
func TestNodeGatewayTelegramChannelConstructed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping gateway Telegram boot in short mode")
	}

	tmp := t.TempDir()
	nodeID := "gateway-tg-node"
	creds := signNodeCert(t, filepath.Join(tmp, "certs"), nodeID)

	cfg := node.Config{
		NodeID:     nodeID,
		Functions:  []types.NodeFunction{types.FunctionCompute, types.FunctionGateway},
		ListenAddr: "127.0.0.1:0",
		Creds:      creds,
		LLMProvider: compute.NewMockProvider(compute.MockResponse{Content: "pong"}),
		ChannelSecretResolver: func(ref string) (string, error) {
			switch ref {
			case "env:TG_BOT":
				return "test-bot-token", nil
			case "env:TG_SECRET":
				return "test-webhook-secret-min-32bytes-fill", nil
			}
			return "", fmt.Errorf("unexpected ref: %q", ref)
		},
		Gateway: config.GatewayConfig{
			Enabled:  true,
			HTTPPort: 0,
			Channels: []config.GatewayChannelConfig{
				{
					Type:           "telegram",
					BotTokenRef:    "env:TG_BOT",
					SecretTokenRef: "env:TG_SECRET",
				},
			},
			UnknownUserScope: "public",
		},
	}

	n, err := node.New(cfg)
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Start(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for n.Gateway().Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n.Gateway().Addr() == "" {
		cancel()
		<-done
		t.Fatal("gateway didn't bind")
	}

	// /telegram should exist but reject (401) because we didn't send
	// the secret header. That's enough to prove the handler mounted.
	resp, err := http.Post("http://"+n.Gateway().Addr()+"/telegram",
		"application/json", strings.NewReader(`{"update_id":1}`))
	if err != nil {
		cancel()
		<-done
		t.Fatalf("POST /telegram: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		cancel()
		<-done
		t.Errorf("/telegram without secret header should 401 (proves mount);"+
			" got %d", resp.StatusCode)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start returned: %v", err)
	}
}

// TestNodeGatewayUnknownChannelTypeSkipped — a typo'd channel type
// ("telegrm") logs a warn and is skipped; other channels still boot.
// Prevents single-entry misconfigurations from taking down the node.
func TestNodeGatewayUnknownChannelTypeSkipped(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping gateway boot in short mode")
	}

	tmp := t.TempDir()
	nodeID := "gateway-typo-node"
	creds := signNodeCert(t, filepath.Join(tmp, "certs"), nodeID)

	cfg := node.Config{
		NodeID:     nodeID,
		Functions:  []types.NodeFunction{types.FunctionCompute, types.FunctionGateway},
		ListenAddr: "127.0.0.1:0",
		Creds:      creds,
		LLMProvider: compute.NewMockProvider(compute.MockResponse{Content: "pong"}),
		Gateway: config.GatewayConfig{
			Enabled:  true,
			HTTPPort: 0,
			Channels: []config.GatewayChannelConfig{
				{Type: "telegrm"},      // typo — unknown
				{Type: "future-thing"}, // completely unknown
			},
			UnknownUserScope: "public",
		},
	}

	n, err := node.New(cfg)
	if err != nil {
		t.Fatalf("node.New should tolerate unknown types: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- n.Start(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for n.Gateway().Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n.Gateway().Addr() == "" {
		t.Fatal("gateway didn't bind — unknown channel types should not block server startup")
	}

	// Healthz confirms the process is serving.
	resp, err := http.Get("http://" + n.Gateway().Addr() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz: %d", resp.StatusCode)
	}
}

// TestNodeSchedulerFiresCommitmentAfterBoot is the Phase 7c exit
// criterion: a node boots with Raft + scheduler + PlanService;
// PlanService.AddCommitment persists a due-now commitment; the
// scheduler picks it up through the FSM wake hook and fires a
// registered handler. Proves the boot wiring (New → wireRaft →
// wireCompute → scheduler.NewScheduler → Run from Start) + the
// full round-trip (PlanService → Raft → FSM callback → wake → CAS
// claim → handler dispatch → complete CAS) works end-to-end.
func TestNodeSchedulerFiresCommitmentAfterBoot(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scheduler boot integration in short mode")
	}

	tmp := t.TempDir()
	nodeID := "sched-boot-node"
	creds := signNodeCert(t, filepath.Join(tmp, "certs"), nodeID)
	memoryKey := mustKey(t)

	cfg := node.Config{
		NodeID:         nodeID,
		Functions:      []types.NodeFunction{types.FunctionMemory, types.FunctionPolicy, types.FunctionStorage},
		ListenAddr:     "127.0.0.1:0",
		DataDir:        filepath.Join(tmp, "data"),
		Bootstrap:      true,
		SnapshotTarget: "storage:test-backup",
		Creds:          creds,
		MemoryKey:      memoryKey,
	}
	n, err := node.New(cfg)
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	if n.Plan() == nil {
		t.Fatal("Plan() nil — PlanService should be wired when Raft is up")
	}
	if n.Scheduler() == nil {
		t.Fatal("Scheduler() nil — scheduler should be wired alongside PlanService")
	}

	// Register a commitment handler BEFORE Start so it's in place
	// when the scheduler starts ticking.
	var fired int32
	cb := make(chan struct{}, 1)
	_ = n.Scheduler().Handlers().RegisterCommitment("test-ping",
		func(_ context.Context, _ *lobslawv1.AgentCommitment) error {
			atomic.AddInt32(&fired, 1)
			select {
			case cb <- struct{}{}:
			default:
			}
			return nil
		})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- n.Start(ctx)
	}()
	<-started

	if err := n.Raft().WaitForLeader(5 * time.Second); err != nil {
		cancel()
		<-done
		t.Fatalf("WaitForLeader: %v", err)
	}

	// Add a commitment due immediately. Boot wiring under test:
	// PlanService → Raft.Apply → FSM.applyPut → schedulerChange
	// callback → scheduler wakes → fires handler.
	_, err = n.Plan().AddCommitment(ctx, &lobslawv1.AddCommitmentRequest{
		Commitment: &lobslawv1.AgentCommitment{
			DueAt:      timestamppb.New(time.Now().Add(-time.Second)),
			Reason:     "ping test",
			HandlerRef: "test-ping",
		},
	})
	if err != nil {
		cancel()
		<-done
		t.Fatalf("AddCommitment: %v", err)
	}

	select {
	case <-cb:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("handler never fired within 5s")
	}
	if atomic.LoadInt32(&fired) != 1 {
		t.Errorf("handler fired %d times; want 1", atomic.LoadInt32(&fired))
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start returned: %v", err)
	}
}

// TestNodeSchedulerAgentTurnHandler — the Phase 7d built-in
// "agent:turn" handler must dispatch the task's params.prompt
// through the real agent loop. Uses a MockProvider so we can
// assert on the ChatRequest the agent built.
func TestNodeSchedulerAgentTurnHandler(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping agent-turn boot integration in short mode")
	}

	tmp := t.TempDir()
	nodeID := "agent-turn-node"
	creds := signNodeCert(t, filepath.Join(tmp, "certs"), nodeID)
	memoryKey := mustKey(t)

	// MockProviderFunc lets us capture the request + gate "done."
	seen := make(chan compute.ChatRequest, 1)
	provider := compute.NewMockProviderFunc(func(req compute.ChatRequest, _ int) (compute.MockResponse, error) {
		select {
		case seen <- req:
		default:
		}
		return compute.MockResponse{Content: "ok"}, nil
	})

	cfg := node.Config{
		NodeID: nodeID,
		Functions: []types.NodeFunction{
			types.FunctionMemory, types.FunctionPolicy,
			types.FunctionStorage, types.FunctionCompute,
		},
		ListenAddr:     "127.0.0.1:0",
		DataDir:        filepath.Join(tmp, "data"),
		Bootstrap:      true,
		SnapshotTarget: "storage:test-backup",
		Creds:          creds,
		MemoryKey:      memoryKey,
		LLMProvider:    provider,
	}
	n, err := node.New(cfg)
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	if _, ok := n.Scheduler().Handlers().GetCommitmentHandler(node.AgentTurnHandlerRef); !ok {
		t.Fatal("boot did not register agent:turn commitment handler")
	}
	if _, ok := n.Scheduler().Handlers().GetTaskHandler(node.AgentTurnHandlerRef); !ok {
		t.Fatal("boot did not register agent:turn task handler")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- n.Start(ctx) }()

	if err := n.Raft().WaitForLeader(5 * time.Second); err != nil {
		cancel()
		<-done
		t.Fatalf("WaitForLeader: %v", err)
	}

	_, err = n.Plan().AddCommitment(ctx, &lobslawv1.AddCommitmentRequest{
		Commitment: &lobslawv1.AgentCommitment{
			DueAt:      timestamppb.New(time.Now().Add(-time.Second)),
			Reason:     "check the oven",
			HandlerRef: node.AgentTurnHandlerRef,
			CreatedFor: "alice",
		},
	})
	if err != nil {
		cancel()
		<-done
		t.Fatalf("AddCommitment: %v", err)
	}

	select {
	case req := <-seen:
		if len(req.Messages) == 0 {
			t.Fatal("agent saw no messages")
		}
		// Find the user-role message; promptgen may prepend
		// system/RAG content.
		var userPrompt string
		for _, m := range req.Messages {
			if m.Role == "user" {
				userPrompt = m.Content
			}
		}
		if userPrompt != "check the oven" {
			t.Errorf("user prompt: %q want %q", userPrompt, "check the oven")
		}
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("agent never called within 5s")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start returned: %v", err)
	}
}
