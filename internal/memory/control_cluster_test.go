package memory

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/jmylchreest/lobslaw/internal/dataformat"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func controlCluster(t *testing.T) []*RaftNode {
	t.Helper()
	ids := []string{"a", "b", "c", "d"}
	nodes := make([]*RaftNode, len(ids))
	transports := make([]*raft.InmemTransport, len(ids))
	for i, id := range ids {
		_, transports[i] = raft.NewInmemTransport(raft.ServerAddress(id))
	}
	for i := range transports {
		for j := range transports {
			if i != j {
				transports[i].Connect(raft.ServerAddress(ids[j]), transports[j])
			}
		}
	}
	for i, id := range ids {
		dir := t.TempDir()
		key, err := crypto.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		store, err := OpenStore(filepath.Join(dir, "state.db"), key)
		if err != nil {
			t.Fatal(err)
		}
		node, err := NewRaft(RaftConfig{
			NodeID: id, LocalAddr: raft.ServerAddress(id), DataDir: dir,
			Transport: transports[i], Bootstrap: i == 0,
			HeartbeatTimeout: 100 * time.Millisecond, ElectionTimeout: 100 * time.Millisecond, LeaderLeaseTimeout: 50 * time.Millisecond,
		}, NewFSM(store))
		if err != nil {
			t.Fatal(err)
		}
		nodes[i] = node
		t.Cleanup(func() { _ = node.Shutdown(); _ = store.Close(); _ = transports[i].Close() })
	}
	if err := nodes[0].WaitForLeader(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"b", "c"} {
		if err := nodes[0].AddVoter(raft.ServerID(id), raft.ServerAddress(id)); err != nil {
			t.Fatal(err)
		}
	}
	return nodes
}

func transferControlLeader(t *testing.T, from, to *RaftNode) {
	t.Helper()
	if err := from.Raft.LeadershipTransferToServer(to.nodeID, to.localAddr).Error(); err != nil {
		t.Fatal(err)
	}
	if err := to.WaitForLeader(time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestControlRejectsLeadershipTurnoverDuringProbe(t *testing.T) {
	for _, operation := range []string{"membership", "transfer"} {
		t.Run(operation, func(t *testing.T) {
			nodes := controlCluster(t)
			entered, release := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			nodes[0].SetUpgradeProbe(func(ctx context.Context, server raft.Server) (*UpgradePeer, error) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return &UpgradePeer{ID: string(server.ID), Supported: dataformat.SupportedContracts(), State: dataformat.ContractState{Active: 1}}, nil
			})
			done := make(chan error, 1)
			go func() {
				if operation == "membership" {
					done <- nodes[0].AddVoter("d", "d")
					return
				}
				_, err := nodes[0].ChangeUpgrade(context.Background(), &pb.ChangeUpgradeRequest{Action: "transfer", TargetNodeId: "b"})
				done <- err
			}()
			<-entered
			transferControlLeader(t, nodes[0], nodes[2])
			transferControlLeader(t, nodes[2], nodes[0])
			close(release)
			if err := <-done; !errors.Is(err, errControlChanged) {
				t.Fatalf("stale operation returned %v", err)
			}
			if len(nodes[0].ConfigurationServers()) != 3 {
				t.Fatal("stale operation changed membership")
			}
		})
	}
}

func TestControlFenceCoversValidationToEnqueue(t *testing.T) {
	nodes := controlCluster(t)
	leader := nodes[0]
	generation, err := leader.controlGeneration(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	transfer := make(chan error, 1)
	future, err := leader.enqueueControl(ctx, generation, func(time.Duration) raft.Future {
		go func() { transfer <- leader.Raft.LeadershipTransferToServer("b", "b").Error() }()
		// The pinned Raft implementation publishes State before invoking its
		// synchronous filter. Force the exact check-to-enqueue race while that
		// filter waits for our admission lock; it must not re-enter a new leader loop.
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for leader.Raft.State() == raft.Leader {
			select {
			case <-ticker.C:
			case <-ctx.Done():
				t.Fatal("leadership did not change")
			}
		}
		return leader.Raft.AddVoter("d", "d", 0, 20*time.Millisecond)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := future.Error(); !errors.Is(err, raft.ErrEnqueueTimeout) {
		t.Fatalf("racing enqueue returned %v", err)
	}
	if err := <-transfer; err != nil {
		t.Fatal(err)
	}
	transferControlLeader(t, nodes[1], nodes[0])
	if len(leader.ConfigurationServers()) != 3 {
		t.Fatal("stale queued membership survived turnover")
	}
}

func TestMembershipTurnoverPreservesPreparedFreeze(t *testing.T) {
	if !slices.Contains(dataformat.SupportedContracts(), uint32(2)) {
		t.Skip("requires the contract-2 consumer on top of the rolling foundation")
	}
	nodes := controlCluster(t)
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	for i, node := range nodes {
		node.SetUpgradeProbe(func(ctx context.Context, server raft.Server) (*UpgradePeer, error) {
			if i == 0 && server.ID == "d" {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			for _, remote := range nodes {
				if remote.nodeID == server.ID {
					state, err := remote.fsm.store.ContractState()
					return &UpgradePeer{ID: string(server.ID), Supported: dataformat.SupportedContracts(), State: state}, err
				}
			}
			return nil, errors.New("unknown member")
		})
	}
	done := make(chan error, 1)
	go func() { done <- nodes[0].AddVoter("d", "d") }()
	<-entered
	transferControlLeader(t, nodes[0], nodes[1])
	if _, err := nodes[1].ChangeUpgrade(context.Background(), &pb.ChangeUpgradeRequest{Action: "prepare", TransitionId: "turnover", Target: 2}); err != nil {
		t.Fatal(err)
	}
	transferControlLeader(t, nodes[1], nodes[0])
	close(release)
	if err := <-done; !errors.Is(err, errControlChanged) {
		t.Fatalf("stale membership request returned %v", err)
	}
	if len(nodes[0].ConfigurationServers()) != 3 {
		t.Fatal("membership changed while prepared")
	}
	status, err := nodes[0].ChangeUpgrade(context.Background(), &pb.ChangeUpgradeRequest{Action: "abort", TransitionId: "turnover", Target: 2})
	if err != nil {
		t.Fatal("prepared transition could not be aborted", err)
	}
	if status.Prepared != nil || status.ActiveContract != 1 {
		t.Fatal("abort changed the active contract", status)
	}
}
