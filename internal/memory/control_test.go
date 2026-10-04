package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestMembershipProbeHasDeadline(t *testing.T) {
	node, _ := newTestRaft(t)
	var bounded bool
	node.SetUpgradeProbe(func(ctx context.Context, _ raft.Server) (*UpgradePeer, error) {
		_, bounded = ctx.Deadline()
		return nil, errors.New("probe complete")
	})
	_ = node.AddVoter("other", "other")
	if !bounded {
		t.Fatal("membership probe has no deadline")
	}
}

func TestMembershipProbeDoesNotBlockWrites(t *testing.T) {
	node, _ := newTestRaft(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	node.SetUpgradeProbe(func(ctx context.Context, _ raft.Server) (*UpgradePeer, error) {
		close(entered)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return nil, errors.New("probe complete")
		}
	})
	go func() { done <- node.AddVoter("other", "other") }()
	<-entered
	raw, err := proto.Marshal(&pb.LogEntry{Op: pb.LogOp_LOG_OP_PUT, Id: "while-probing", Payload: &pb.LogEntry_PolicyRule{PolicyRule: &pb.PolicyRule{Id: "while-probing", Subject: "user:alice", Action: "memory:read", Resource: "*", Effect: "allow"}}})
	if err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	applied := make(chan error, 1)
	go func() {
		result, err := node.Apply(raw, time.Second)
		if err == nil {
			err, _ = result.(error)
		}
		applied <- err
	}()
	var blocked bool
	var applyErr error
	select {
	case applyErr = <-applied:
	case <-time.After(time.Second):
		blocked = true
	}
	close(release)
	<-done
	if blocked {
		<-applied
	}
	if blocked {
		t.Fatal("membership network probe blocked ordinary writes")
	}
	if applyErr != nil {
		t.Fatal("ordinary write during probe failed", applyErr)
	}
}

type pendingControlFuture struct{ done <-chan struct{} }

func (f pendingControlFuture) Error() error { <-f.done; return nil }

func TestCanceledControlRetainsLeaseUntilFutureCompletes(t *testing.T) {
	node, _ := newTestRaft(t)
	lease, err := node.acquireControl(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	if err := lease.wait(ctx, pendingControlFuture{done: done}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	lease.release()
	if len(node.control) != 1 {
		t.Fatal("canceled mutation released serialization before outcome was known")
	}
	if _, err := node.acquireControl(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(done)
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	next, err := node.acquireControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	next.release()
}

func TestMembershipProbeHonorsCallerCancellation(t *testing.T) {
	node, _ := newTestRaft(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node.SetUpgradeProbe(func(ctx context.Context, _ raft.Server) (*UpgradePeer, error) {
		cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err := node.AddVoterContext(ctx, "other", "other"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := node.ApplyContext(ctx, nil, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
