package memory_test

// This harness runs separate baseline and candidate test binaries. It exercises
// real encrypted stores, gRPC transport, snapshots and process restarts; it does
// not simulate an old node by changing a capability variable in a new binary.
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/dataformat"
	"github.com/jmylchreest/lobslaw/internal/grpcinterceptors"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/rafttransport"
)

type rollingConfig struct {
	ID, Addr, Dir, CertDir string
	Bootstrap              bool
}
type rollingRPC struct {
	pb.UnimplementedNodeServiceServer
	node *memory.RaftNode
}

func (s *rollingRPC) GetPeers(context.Context, *pb.GetPeersRequest) (*pb.GetPeersResponse, error) {
	return &pb.GetPeersResponse{}, nil
}
func (s *rollingRPC) AddMember(_ context.Context, r *pb.AddMemberRequest) (*pb.AddMemberResponse, error) {
	err := s.node.AddVoter(raft.ServerID(r.NodeId), raft.ServerAddress(r.Address))
	return &pb.AddMemberResponse{Accepted: err == nil}, err
}
func (s *rollingRPC) Propose(_ context.Context, r *pb.ProposeRequest) (*pb.ProposeResponse, error) {
	res, err := s.node.Apply(r.Entry, 5*time.Second)
	if err == nil {
		if e, ok := res.(error); ok {
			err = e
		}
	}
	return &pb.ProposeResponse{}, err
}
func (s *rollingRPC) UpgradeStatus(context.Context, *pb.UpgradeStatusRequest) (*pb.UpgradeStatusResponse, error) {
	return s.node.UpgradeStatus()
}
func (s *rollingRPC) ChangeUpgrade(ctx context.Context, r *pb.ChangeUpgradeRequest) (*pb.ChangeUpgradeResponse, error) {
	if r.Action == "test-auto-step" {
		plan, err := s.node.PlanAutomaticUpgrade(ctx)
		if err != nil {
			return nil, err
		}
		out, err := s.node.ChangeAutomaticUpgrade(ctx, plan)
		return &pb.ChangeUpgradeResponse{Status: out}, err
	}
	if r.Action == "test-snapshot" {
		if err := s.node.Raft.Snapshot().Error(); err != nil {
			return nil, err
		}
		out, err := s.node.UpgradeStatus()
		return &pb.ChangeUpgradeResponse{Status: out}, err

	}
	out, err := s.node.ChangeUpgrade(ctx, r)
	return &pb.ChangeUpgradeResponse{Status: out}, err
}

func TestRollingProcessHelper(t *testing.T) {
	raw := os.Getenv("LOBSLAW_ROLLING_HELPER")
	if raw == "" {
		t.Skip("subprocess only")
	}
	var cfg rollingConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	creds, err := mtls.LoadNodeCreds(filepath.Join(cfg.CertDir, "ca.pem"), filepath.Join(cfg.CertDir, cfg.ID+".cert.pem"), filepath.Join(cfg.CertDir, cfg.ID+".key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := os.ReadFile(filepath.Join(cfg.CertDir, "memory-key"))
	if err != nil {
		t.Fatal(err)
	}
	var key crypto.Key
	copy(key[:], keyBytes)
	store, err := memory.OpenStore(filepath.Join(cfg.Dir, "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	required := func() uint32 {
		state, err := store.ContractState()
		if err != nil {
			return 0
		}
		return state.Required()
	}
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds.ClientCreds()), grpc.WithChainUnaryInterceptor(grpcinterceptors.DataFormatClient(required)), grpc.WithChainStreamInterceptor(grpcinterceptors.DataFormatStreamClient(required))}
	transport, err := rafttransport.New(rafttransport.Config{LocalAddr: raft.ServerAddress(cfg.Addr), DialOpts: opts})
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(creds.ServerCreds()), grpc.ChainUnaryInterceptor(grpcinterceptors.OperatorNotAPeer(), grpcinterceptors.DataFormat(required)), grpc.ChainStreamInterceptor(grpcinterceptors.OperatorNotAPeerStream(), grpcinterceptors.DataFormatStream(required)))
	transport.Register(grpcinterceptors.PersistenceRegistrar{ServiceRegistrar: server})
	node, err := memory.NewRaft(memory.RaftConfig{NodeID: cfg.ID, LocalAddr: raft.ServerAddress(cfg.Addr), DataDir: cfg.Dir, Bootstrap: cfg.Bootstrap, Transport: transport.RaftTransport()}, memory.NewFSM(store))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = node.Shutdown() }()
	reload := node.Raft.ReloadableConfig()
	reload.TrailingLogs = 1
	if err := node.Raft.ReloadConfig(reload); err != nil {
		t.Fatal(err)
	}
	node.SetAutomaticUpgradeReady(func() bool { return true })
	node.SetUpgradeProbe(func(ctx context.Context, member raft.Server) (*memory.UpgradePeer, error) {
		conn, err := grpc.NewClient(string(member.Address), opts...)
		if err != nil {
			return nil, err
		}
		defer func() { _ = conn.Close() }()
		status, err := pb.NewUpgradeServiceClient(conn).UpgradeStatus(ctx, &pb.UpgradeStatusRequest{})
		if err != nil {
			return nil, err
		}
		state := dataformat.ContractState{Active: status.ActiveContract, Epoch: status.Epoch}
		if p := status.Prepared; p != nil {
			state.Prepared = &dataformat.Transition{ID: p.TransitionId, Target: p.Target, Epoch: p.ExpectedEpoch, MembershipIndex: p.MembershipIndex, MembershipFingerprint: p.MembershipFingerprint, Members: p.MemberIds, Index: status.PreparedIndex}
		}
		return &memory.UpgradePeer{ID: status.NodeId, Supported: status.SupportedContracts, State: state, AutomaticTargets: status.AutomaticTargets, AutomaticReady: status.AutomaticReady, AppliedIndex: status.AppliedIndex}, nil
	})
	rpc := &rollingRPC{node: node}
	pb.RegisterNodeServiceServer(grpcinterceptors.PersistenceRegistrar{ServiceRegistrar: server}, rpc)
	pb.RegisterUpgradeServiceServer(server, rpc)
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Serve(ln); err != nil {
		t.Fatal(err)
	}
}

type rollingChild struct {
	cfg  rollingConfig
	cmd  *exec.Cmd
	conn *grpc.ClientConn
}

func (c *rollingChild) stop() {
	if c.cmd != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
		c.cmd = nil
	}
}
func (c *rollingChild) start(t *testing.T, binary string) {
	t.Helper()
	raw, _ := json.Marshal(c.cfg)
	cmd := exec.Command(binary, "-test.run=^TestRollingProcessHelper$", "-test.timeout=3m")
	cmd.Env = append(os.Environ(), "LOBSLAW_ROLLING_HELPER="+string(raw))
	log, err := os.CreateTemp(c.cfg.Dir, "process-*.log")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			raw, _ := os.ReadFile(log.Name())
			t.Logf("child %s: %s", c.cfg.ID, raw)
		}
	})
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	_ = log.Close()
	c.cmd = cmd
	rollingWait(t, func() bool { _, err := c.status(); return err == nil })
}
func (c *rollingChild) status() (*pb.UpgradeStatusResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	return pb.NewUpgradeServiceClient(c.conn).UpgradeStatus(ctx, &pb.UpgradeStatusRequest{})
}
func rollingWait(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("rolling cluster did not become ready")
}
func currentRollingLeader(nodes []*rollingChild) *rollingChild {
	for _, member := range nodes {
		status, err := member.status()
		if err == nil && status.LeaderAddress == member.cfg.Addr {
			return member
		}
	}
	return nil
}

func (c *rollingChild) change(r *pb.ChangeUpgradeRequest) (*pb.UpgradeStatusResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	reply, err := pb.NewUpgradeServiceClient(c.conn).ChangeUpgrade(ctx, r)
	return reply.GetStatus(), err
}
func (c *rollingChild) write(id string) error {
	raw, err := proto.Marshal(&pb.LogEntry{Op: pb.LogOp_LOG_OP_PUT, Id: id, Payload: &pb.LogEntry_PolicyRule{PolicyRule: &pb.PolicyRule{Id: id, Subject: "user:test", Action: "memory:read", Resource: "*"}}})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = pb.NewNodeServiceClient(c.conn).Propose(ctx, &pb.ProposeRequest{Entry: raw})
	return err
}

func TestRollingMixedBinaries(t *testing.T) {
	old := os.Getenv("LOBSLAW_ROLLING_BASELINE")
	if old == "" {
		t.Skip("set LOBSLAW_ROLLING_BASELINE to the compiled baseline memory test binary")
	}
	if !slices.Contains(dataformat.SupportedContracts(), uint32(2)) {
		t.Skip("run parent from candidate supporting contract 2")
	}
	candidate, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	nodes := newRollingChildren(t, old)
	leader := nodes[0]
	rollingWait(t, func() bool { s, err := leader.status(); return err == nil && s.LeaderAddress == leader.cfg.Addr })
	for _, n := range nodes[1:] {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := pb.NewNodeServiceClient(leader.conn).AddMember(ctx, &pb.AddMemberRequest{NodeId: n.cfg.ID, Address: n.cfg.Addr, Voter: true})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := leader.write("before"); err != nil {
		t.Fatal(err)
	}
	// Replace one follower; old leader still writes old-contract records.
	nodes[1].stop()
	nodes[1].start(t, candidate)
	if err := leader.write("mixed-old-leader"); err != nil {
		t.Fatal(err)
	}
	if _, err := leader.change(&pb.ChangeUpgradeRequest{Action: "transfer", TargetNodeId: nodes[1].cfg.ID}); err != nil {
		t.Fatal(err)
	}
	leader = nodes[1]
	rollingWait(t, func() bool { s, err := leader.status(); return err == nil && s.LeaderAddress == leader.cfg.Addr })
	if err := leader.write("mixed-new-leader"); err != nil {
		t.Fatal(err)
	}
	if err := leader.writeGroup(); err == nil {
		t.Fatal("new-only record accepted before activation")
	}
	if err := leader.write("after-refused-feature"); err != nil {
		t.Fatal("refused proposal halted cluster", err)
	}
	prepare := &pb.ChangeUpgradeRequest{Action: "prepare", TransitionId: "teams", Target: 2}
	if _, err := leader.change(prepare); err == nil {
		t.Fatal("activated with old members")
	}
	snapshotCatchup(t, leader, nodes[2], old)
	for _, i := range []int{0, 2} {
		nodes[i].stop()
		nodes[i].start(t, candidate)
	}
	rollingWait(t, func() bool {
		_, err := leader.change(prepare)
		if err != nil {
			t.Log("prepare", err)
		}
		return err == nil
	})
	if _, err := pb.NewNodeServiceClient(leader.conn).AddMember(context.Background(), &pb.AddMemberRequest{NodeId: "extra", Address: "127.0.0.1:1", Voter: true}); err == nil {
		t.Fatal("membership changed while prepared")
	}
	// Preparation remains a durable fence through a process restart.
	rollingWait(t, func() bool { s, err := nodes[2].status(); return err == nil && s.Prepared != nil })
	nodes[2].stop()
	nodes[2].refuseOld(t, old)
	nodes[2].start(t, candidate)
	finalize := &pb.ChangeUpgradeRequest{Action: "finalize", TransitionId: "teams", Target: 2}
	rollingWait(t, func() bool {
		_, err := leader.change(finalize)
		if err != nil {
			t.Log("finalize", err)
		}
		return err == nil
	})
	for _, n := range nodes {
		rollingWait(t, func() bool { s, err := n.status(); return err == nil && s.ActiveContract == 2 && s.Epoch == 1 })
	}
	if _, err := leader.change(finalize); err != nil {
		t.Fatal("finalize response retry", err)
	}
	if err := leader.writeGroup(); err != nil {
		t.Fatal("new contract write", err)
	}
	nodes[2].stop()
	nodes[2].refuseOld(t, old)
	nodes[2].start(t, candidate)

}

func newRollingChildren(t *testing.T, old string) []*rollingChild {
	t.Helper()
	certDir, ca, caKey, caPath := newClusterCA(t)
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "memory-key"), key[:], 0600); err != nil {
		t.Fatal(err)
	}
	nodes := make([]*rollingChild, 3)
	for i := range nodes {
		id := fmt.Sprintf("rolling-%d", i)
		cert, k, err := mtls.SignNodeCert(ca, caKey, mtls.SignOpts{NodeID: id, IPs: []net.IP{net.ParseIP("127.0.0.1")}, ValidFor: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		certPath := filepath.Join(certDir, id+".cert.pem")
		keyPath := filepath.Join(certDir, id+".key.pem")
		if err := mtls.WriteNodeFiles(certPath, keyPath, cert, k); err != nil {
			t.Fatal(err)
		}
		creds, err := mtls.LoadNodeCreds(caPath, certPath, keyPath)
		if err != nil {
			t.Fatal(err)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds.ClientCreds()), grpc.WithUnaryInterceptor(grpcinterceptors.DataFormatClient()))
		if err != nil {
			t.Fatal(err)
		}
		c := &rollingChild{cfg: rollingConfig{ID: id, Addr: addr, Dir: t.TempDir(), CertDir: certDir, Bootstrap: i == 0}, conn: conn}
		nodes[i] = c
		t.Cleanup(func() { c.stop(); _ = conn.Close() })
		c.start(t, old)
	}

	return nodes
}

func (c *rollingChild) writeGroup() error {
	group := protowire.AppendTag(nil, 1, protowire.BytesType)
	group = protowire.AppendString(group, "rolling-team")
	group = protowire.AppendTag(group, 11, protowire.BytesType)
	group = protowire.AppendString(group, "user:test")
	entry, _ := proto.Marshal(&pb.LogEntry{Op: pb.LogOp_LOG_OP_PUT, Id: "rolling-team"})
	entry = protowire.AppendTag(entry, 54, protowire.BytesType)
	entry = protowire.AppendBytes(entry, group)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pb.NewNodeServiceClient(c.conn).Propose(ctx, &pb.ProposeRequest{Entry: entry})
	return err
}

func (c *rollingChild) refuseOld(t *testing.T, binary string) {
	t.Helper()
	before, err := os.ReadFile(filepath.Join(c.cfg.Dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(c.cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestRollingProcessHelper$", "-test.timeout=7s")
	cmd.Env = append(os.Environ(), "LOBSLAW_ROLLING_HELPER="+string(raw))
	output, err := cmd.CombinedOutput()
	if err == nil || ctx.Err() != nil || !strings.Contains(string(output), "contract") {
		t.Fatalf("older binary did not refuse persisted fence: %v %s", err, output)
	}
	after, err := os.ReadFile(filepath.Join(c.cfg.Dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("older binary mutated newer data")
	}
}

func snapshotCatchup(t *testing.T, leader, follower *rollingChild, old string) {
	t.Helper()
	// An old follower must install a newer producer's old-contract snapshot.
	follower.stop()
	for i := range 30 {
		if err := leader.write(fmt.Sprintf("snapshot-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := leader.change(&pb.ChangeUpgradeRequest{Action: "test-snapshot"}); err != nil {
		t.Fatal(err)
	}
	follower.start(t, old)
	latest, err := leader.status()
	if err != nil {
		t.Fatal(err)
	}
	rollingWait(t, func() bool {
		s, err := follower.status()
		return err == nil && s.AppliedIndex >= latest.AppliedIndex && s.ActiveContract == 1
	})

}

// The baseline for this test already understands contract 2. Its lack of
// automatic readiness must still block the candidate; format support is not
// permission to take over a rollout.
func TestRollingAutomaticUpgrade(t *testing.T) {
	old := os.Getenv("LOBSLAW_AUTOMATIC_BASELINE")
	if old == "" {
		t.Skip("set LOBSLAW_AUTOMATIC_BASELINE to the pre-automatic contract-2 memory test binary")
	}
	candidate, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	nodes := newRollingChildren(t, old)
	leader := nodes[0]
	rollingWait(t, func() bool { s, e := leader.status(); return e == nil && s.LeaderAddress == leader.cfg.Addr })
	for _, n := range nodes[1:] {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := pb.NewNodeServiceClient(leader.conn).AddMember(ctx, &pb.AddMemberRequest{NodeId: n.cfg.ID, Address: n.cfg.Addr, Voter: true})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	nodes[1].stop()
	nodes[1].start(t, candidate)
	if _, err := leader.change(&pb.ChangeUpgradeRequest{Action: "transfer", TargetNodeId: nodes[1].cfg.ID}); err != nil {
		t.Fatal(err)
	}
	leader = nodes[1]
	rollingWait(t, func() bool { s, e := leader.status(); return e == nil && s.LeaderAddress == leader.cfg.Addr })
	if _, err := leader.change(&pb.ChangeUpgradeRequest{Action: "test-auto-step"}); err == nil {
		t.Fatal("old contract-2 members did not block automatic activation")
	}
	if err := leader.write("mixed-auto"); err != nil {
		t.Fatal(err)
	}
	nodes[0].stop()
	nodes[0].start(t, candidate)
	nodes[2].stop()
	if _, err := leader.change(&pb.ChangeUpgradeRequest{Action: "test-auto-step"}); err == nil {
		t.Fatal("offline member did not block automatic activation")
	}
	nodes[2].start(t, candidate)
	// A rollout can elect a different leader while followers restart. Force
	// another handoff so preparation cannot rely on the earlier leader.
	rollingWait(t, func() bool {
		leader = currentRollingLeader(nodes)
		if leader == nil {
			return false
		}
		target := nodes[0]
		if leader == target {
			target = nodes[1]
		}
		_, err := leader.change(&pb.ChangeUpgradeRequest{Action: "transfer", TargetNodeId: target.cfg.ID})
		return err == nil
	})
	var prepared *pb.UpgradeStatusResponse
	rollingWait(t, func() bool {
		leader = currentRollingLeader(nodes)
		if leader == nil {
			return false
		}
		prepared, err = leader.change(&pb.ChangeUpgradeRequest{Action: "test-auto-step"})
		if err != nil {
			t.Log("automatic prepare", err)
		}
		return err == nil
	})
	if prepared.ActiveContract != 1 || prepared.Prepared == nil {
		t.Fatal(prepared)
	}
	id := prepared.Prepared.TransitionId
	follower := nodes[2]
	if follower == leader {
		follower = nodes[0]
	}
	rollingWait(t, func() bool {
		s, e := follower.status()
		return e == nil && s.Prepared != nil && s.Prepared.TransitionId == id
	})
	if _, err := follower.change(&pb.ChangeUpgradeRequest{Action: "test-snapshot"}); err != nil {
		t.Fatal(err)
	}
	follower.stop()
	follower.start(t, candidate)
	rollingWait(t, func() bool {
		leader = currentRollingLeader(nodes)
		if leader == nil {
			return false
		}
		nextLeader := nodes[0]
		if leader == nextLeader {
			nextLeader = nodes[1]
		}
		if _, err := leader.change(&pb.ChangeUpgradeRequest{Action: "transfer", TargetNodeId: nextLeader.cfg.ID}); err != nil {
			return false
		}
		leader = nextLeader
		return true
	})
	rollingWait(t, func() bool { s, e := leader.status(); return e == nil && s.LeaderAddress == leader.cfg.Addr })
	rollingWait(t, func() bool {
		s, e := leader.change(&pb.ChangeUpgradeRequest{Action: "test-auto-step"})
		return e == nil && s.ActiveContract == 2
	})
	for _, n := range nodes {
		rollingWait(t, func() bool { s, e := n.status(); return e == nil && s.ActiveContract == 2 && s.Epoch == 1 })
	}
	if err := leader.writeGroup(); err != nil {
		t.Fatal(err)
	}
}
