package scheduler

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/memory"
)

// notLeader wraps a real Raft and reports that it is a follower.
//
// The state fireDue declines in, held indefinitely. On a real cluster
// this is a node that lost an election and never won one back, and the
// only outward sign is that scheduled work stops happening.
type notLeader struct {
	Raft
	mu sync.Mutex
}

func (n *notLeader) IsLeader() bool { return false }

func (n *notLeader) FSM() *memory.FSM {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.Raft.FSM()
}

// A scheduler that cannot fire has to say so. Before this it declined
// silently, so "nothing is due" and "nothing can run" produced
// identical logs — and the difference is every scheduled task.
func TestStallIsReported(t *testing.T) {
	t.Parallel()
	node, _ := singleNodeRaft(t, "n1")

	var buf bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(&syncWriter{w: &buf, mu: &mu}, nil))

	s, err := NewScheduler(Config{
		NodeID:   "node-a",
		MaxSleep: 60 * time.Millisecond, // stallAfter = 180ms
		Logger:   log,
	}, &notLeader{Raft: node}, NewHandlerRegistry())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	go func() { _ = s.Run(ctx) }()
	<-ctx.Done()

	mu.Lock()
	out := buf.String()
	mu.Unlock()

	if !strings.Contains(out, "scheduled tasks are not running") {
		t.Fatalf("a scheduler that never fired said nothing about it:\n%s", out)
	}
	if !strings.Contains(out, "not the leader") {
		t.Errorf("the warning does not name the reason:\n%s", out)
	}
	// Once, not once per pass.
	if n := strings.Count(out, "scheduled tasks are not running"); n != 1 {
		t.Errorf("warning logged %d times; it must not become the noise it is trying to stand out from", n)
	}
}

type syncWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
