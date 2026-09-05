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

// A wake source faster than MaxSleep must not stop the scheduler
// firing.
//
// This is the bug exactly: a 1s leadership republish against a 60s
// sleep meant every wake rebuilt the timer before it could expire, so
// fireDue was reached zero times in three minutes on a healthy leader
// and nothing scheduled ran.
func TestFastWakesDoNotStarveFiring(t *testing.T) {
	t.Parallel()
	node, _ := singleNodeRaft(t, "n1")
	s, err := NewScheduler(Config{
		NodeID:       "node-a",
		MaxSleep:     300 * time.Millisecond,
		WakeDebounce: time.Millisecond,
		Logger:       slog.New(slog.DiscardHandler),
	}, node, NewHandlerRegistry())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Wake far faster than MaxSleep, the way the leadership ticker did.
	go func() {
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.Notify()
			}
		}
	}()
	go func() { _ = s.Run(ctx) }()
	<-ctx.Done()

	// 2s at a 300ms deadline is ~6 attempts. Zero is the bug.
	if got := s.fireAttempts.Load(); got == 0 {
		t.Fatal("fireDue was never reached under a fast wake source; scheduled tasks would never run")
	} else {
		t.Logf("fire attempts under a 50/s wake source: %d", got)
	}
}
