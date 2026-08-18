package gateway

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
)

// The channels talk to Prompts, not to a particular implementation, so
// swapping the in-memory registry for the raft-backed one must not
// change what a user sees. Everything below runs against both.
//
// Behaviour the two do NOT share stays out of this file: ID shape, the
// snapshot-pointer guarantee, and Reap are properties of the in-memory
// registry and are asserted in prompts_test.go.

func newRaftPrompts(t *testing.T) *RaftPrompts {
	t.Helper()
	dataDir := t.TempDir()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(dataDir, "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	_, inmem := raft.NewInmemTransport("prompt-conformance")
	node, err := memory.NewRaft(memory.RaftConfig{
		NodeID: "prompt-conformance", LocalAddr: "prompt-conformance",
		DataDir: dataDir, Bootstrap: true, Transport: inmem,
	}, memory.NewFSM(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = node.Shutdown()
		_ = store.Close()
	})
	if err := node.WaitForLeader(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	ps, err := memory.NewPromptStore(memory.PromptStoreConfig{Raft: node, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	return NewRaftPrompts(ps, "conformance-node", compute.BudgetCaps{})
}

func eachPromptImpl(t *testing.T, fn func(t *testing.T, r Prompts)) {
	t.Helper()
	for name, mk := range map[string]func(*testing.T) Prompts{
		"in-memory": func(*testing.T) Prompts { return NewPromptRegistry() },
		"raft":      func(t *testing.T) Prompts { return newRaftPrompts(t) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fn(t, mk(t))
		})
	}
}

func TestPromptsCreateRoundTrips(t *testing.T) {
	t.Parallel()
	eachPromptImpl(t, func(t *testing.T, r Prompts) {
		p, err := r.Create(NewPrompt{TurnID: "turn-1", Reason: "dangerous thing", Channel: "rest", TTL: longTTL})
		if err != nil {
			t.Fatal(err)
		}
		if p.ID == "" {
			t.Error("no ID assigned")
		}
		if p.TurnID != "turn-1" || p.Reason != "dangerous thing" || p.Channel != "rest" {
			t.Errorf("field round-trip: %+v", p)
		}
		if p.Decision != PromptPending {
			t.Errorf("new prompt is %s, want pending", p.Decision)
		}
		if !p.ExpiresAt.After(p.CreatedAt) {
			t.Errorf("ExpiresAt %v is not after CreatedAt %v", p.ExpiresAt, p.CreatedAt)
		}

		got, err := r.Get(p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != p.ID || got.Reason != p.Reason || got.TurnID != p.TurnID {
			t.Errorf("Get returned %+v, want the created prompt %+v", got, p)
		}
	})
}

func TestPromptsResolveRecordsTheDecision(t *testing.T) {
	t.Parallel()
	eachPromptImpl(t, func(t *testing.T, r Prompts) {
		for _, want := range []PromptDecision{PromptApproved, PromptDenied} {
			p, err := r.Create(NewPrompt{TurnID: "t", Reason: "r", Channel: "rest", TTL: longTTL})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Resolve(p.ID, want, PromptScopeOnce); err != nil {
				t.Fatal(err)
			}
			got, err := r.Get(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Decision != want {
				t.Errorf("decision = %s, want %s", got.Decision, want)
			}
		}
	})
}

// "Approve, then deny on a re-tap" must not replay. The first answer
// is the one the user meant.
func TestPromptsFirstWriterWins(t *testing.T) {
	t.Parallel()
	eachPromptImpl(t, func(t *testing.T, r Prompts) {
		p, err := r.Create(NewPrompt{TurnID: "t", Reason: "r", Channel: "rest", TTL: longTTL})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Resolve(p.ID, PromptApproved, PromptScopeOnce); err != nil {
			t.Fatal(err)
		}
		if err := r.Resolve(p.ID, PromptDenied, PromptScopeOnce); !errors.Is(err, ErrPromptResolved) {
			t.Errorf("second Resolve returned %v, want ErrPromptResolved", err)
		}
		got, _ := r.Get(p.ID)
		if got.Decision != PromptApproved {
			t.Errorf("decision = %s, want the first answer to stand", got.Decision)
		}
	})
}

func TestPromptsConcurrentResolveHasOneWinner(t *testing.T) {
	t.Parallel()
	eachPromptImpl(t, func(t *testing.T, r Prompts) {
		p, err := r.Create(NewPrompt{TurnID: "t", Reason: "r", Channel: "rest", TTL: longTTL})
		if err != nil {
			t.Fatal(err)
		}

		const goroutines = 16
		var wins, losses atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range goroutines {
			d := PromptApproved
			if i%2 == 0 {
				d = PromptDenied
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				switch err := r.Resolve(p.ID, d, PromptScopeOnce); {
				case err == nil:
					wins.Add(1)
				case errors.Is(err, ErrPromptResolved):
					losses.Add(1)
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()

		if wins.Load() != 1 {
			t.Errorf("%d winners, want 1 (%d losses)", wins.Load(), losses.Load())
		}
		if wins.Load()+losses.Load() != goroutines {
			t.Errorf("%d wins + %d losses != %d attempts", wins.Load(), losses.Load(), goroutines)
		}
	})
}

func TestPromptsRejectNonUserDecisions(t *testing.T) {
	t.Parallel()
	eachPromptImpl(t, func(t *testing.T, r Prompts) {
		p, err := r.Create(NewPrompt{TurnID: "t", Reason: "r", Channel: "rest", TTL: longTTL})
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []PromptDecision{PromptPending, PromptTimedOut} {
			if err := r.Resolve(p.ID, bad, PromptScopeOnce); err == nil {
				t.Errorf("Resolve accepted %s", bad)
			}
		}
	})
}

func TestPromptsUnknownIDIsNotFound(t *testing.T) {
	t.Parallel()
	eachPromptImpl(t, func(t *testing.T, r Prompts) {
		if _, err := r.Get("nonexistent"); !errors.Is(err, ErrPromptNotFound) {
			t.Errorf("Get returned %v, want ErrPromptNotFound", err)
		}
		if err := r.Resolve("nonexistent", PromptApproved, PromptScopeOnce); !errors.Is(err, ErrPromptNotFound) {
			t.Errorf("Resolve returned %v, want ErrPromptNotFound", err)
		}
		if _, err := r.Wait(context.Background(), "nonexistent"); !errors.Is(err, ErrPromptNotFound) {
			t.Errorf("Wait returned %v, want ErrPromptNotFound", err)
		}
	})
}

func TestPromptsWaitUnblocksOnResolve(t *testing.T) {
	t.Parallel()
	eachPromptImpl(t, func(t *testing.T, r Prompts) {
		p, err := r.Create(NewPrompt{TurnID: "t", Reason: "r", Channel: "rest", TTL: longTTL})
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = r.Resolve(p.ID, PromptApproved, PromptScopeOnce)
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		got, err := r.Wait(ctx, p.ID)
		if err != nil {
			t.Fatalf("Wait errored: %v", err)
		}
		if got != PromptApproved {
			t.Errorf("Wait returned %s, want approved", got)
		}
	})
}

func TestPromptsWaitReturnsImmediatelyWhenResolved(t *testing.T) {
	t.Parallel()
	eachPromptImpl(t, func(t *testing.T, r Prompts) {
		p, err := r.Create(NewPrompt{TurnID: "t", Reason: "r", Channel: "rest", TTL: longTTL})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Resolve(p.ID, PromptDenied, PromptScopeOnce); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		got, err := r.Wait(ctx, p.ID)
		if err != nil {
			t.Fatalf("Wait on an already-resolved prompt errored: %v", err)
		}
		if got != PromptDenied {
			t.Errorf("Wait returned %s, want denied", got)
		}
	})
}

// "I stopped waiting" must be distinguishable from "resolved", or a
// long-poll that gives up would look like a decision.
func TestPromptsWaitHonoursCancellation(t *testing.T) {
	t.Parallel()
	eachPromptImpl(t, func(t *testing.T, r Prompts) {
		p, err := r.Create(NewPrompt{TurnID: "t", Reason: "r", Channel: "rest", TTL: longTTL})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		got, err := r.Wait(ctx, p.ID)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Wait returned %v, want context.Canceled", err)
		}
		if got != PromptPending {
			t.Errorf("a cancelled Wait reported %s, want pending", got)
		}
	})
}

// An unanswered prompt must close itself. The in-memory registry uses
// a timer; the raft-backed one closes it when a waiter notices, with
// the leader's sweeper as the backstop for prompts nobody waits on.
func TestPromptsExpireWithoutAnAnswer(t *testing.T) {
	t.Parallel()
	eachPromptImpl(t, func(t *testing.T, r Prompts) {
		p, err := r.Create(NewPrompt{TurnID: "t", Reason: "r", Channel: "rest", TTL: 50 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		got, err := r.Wait(ctx, p.ID)
		if err != nil {
			t.Fatalf("Wait never unblocked on expiry: %v", err)
		}
		if got != PromptTimedOut {
			t.Errorf("expired prompt resolved to %s, want timed_out", got)
		}
	})
}

// The clock must not overwrite an answer the user already gave.
func TestPromptsExpiryDoesNotOverwriteAnAnswer(t *testing.T) {
	t.Parallel()
	eachPromptImpl(t, func(t *testing.T, r Prompts) {
		p, err := r.Create(NewPrompt{TurnID: "t", Reason: "r", Channel: "rest", TTL: 20 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Resolve(p.ID, PromptApproved, PromptScopeOnce); err != nil {
			t.Fatal(err)
		}
		time.Sleep(60 * time.Millisecond)

		got, err := r.Get(p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Decision != PromptApproved {
			t.Errorf("decision = %s; expiry overwrote the user's answer", got.Decision)
		}
		// Wait must agree with Get, not re-decide.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if d, err := r.Wait(ctx, p.ID); err != nil || d != PromptApproved {
			t.Errorf("Wait returned (%s, %v), want (approved, nil)", d, err)
		}
	})
}

// The audience and the enrolment link must survive BOTH stores.
//
// They did not. Both fields were added to the in-memory Prompt and to
// nothing else, so on a real node — which uses the raft-backed store —
// they were dropped in conversion. Every prompt came back
// unattributable, and the fail-closed guard then refused the very
// person it had been raised for, telling them on their own screen that
// the confirmation "cannot be attributed to anyone".
//
// Every gateway test passed, because they all used the in-memory
// registry. This file already existed, and its opening comment already
// said why: the channels talk to Prompts, not to an implementation.
// The fix was to put the new fields under that same rule.
func TestPromptsCarryTheAudienceAndEnrolment(t *testing.T) {
	t.Parallel()
	eachPromptImpl(t, func(t *testing.T, r Prompts) {
		p, err := r.Create(NewPrompt{
			TurnID: "turn-1", Reason: "operator enrolment: alice",
			Channel: "telegram", ChannelID: "6972251926", TTL: longTTL,
			RaisedFor: "tg-6972251926",
			Enrolment: "enr-abc123",
		})
		if err != nil {
			t.Fatal(err)
		}
		if p.RaisedFor != "tg-6972251926" {
			t.Errorf("Create returned RaisedFor %q; nobody could answer this prompt", p.RaisedFor)
		}
		if p.Enrolment != "enr-abc123" {
			t.Errorf("Create returned Enrolment %q; the answer reaches nothing", p.Enrolment)
		}

		// The read-back is what the callback path actually uses.
		got, err := r.Get(p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.RaisedFor != "tg-6972251926" {
			t.Errorf("Get returned RaisedFor %q; the guard would refuse the person it was raised for",
				got.RaisedFor)
		}
		if got.Enrolment != "enr-abc123" {
			t.Errorf("Get returned Enrolment %q; an approval would issue nothing", got.Enrolment)
		}
	})
}
