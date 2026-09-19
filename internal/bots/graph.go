// Package bots holds the rules about a team of agents that are
// neither storage nor turn execution: who may talk to whom.
//
// Its own package because the message graph is consulted from both
// sides — the registry validates an edge on write, and the messaging
// tools check one on use — and putting it in either would make the
// other import it for a reason that has nothing to do with what it is.
package bots

import (
	"fmt"
	"sort"
	"strings"
)

// Graph is the declared may_message edge list: bot id → the bots it
// may reach.
type Graph map[string][]string

// ErrCycle names a cycle the proposed edges would create.
//
// A distinct error rather than a bare string because the caller —
// bot_create, bot_update, the GUI's save button — wants to show the
// path, and a user told only "invalid" has to guess which of several
// edges was the problem.
type ErrCycle struct {
	Path []string
}

func (e *ErrCycle) Error() string {
	return fmt.Sprintf("that would create a messaging loop: %s", strings.Join(e.Path, " → "))
}

// Validate reports whether the graph is acyclic.
//
// Checked on WRITE rather than enforced at call time, which is the
// whole reason may_message is an explicit declaration instead of a
// free-for-all. A cycle detected when the edge is proposed is one
// error message to one person about one decision they just made; a
// cycle detected at call time is a depth counter, a mid-conversation
// refusal, and a bill for however many turns ran before it tripped.
//
// It also means the cycle rule is checked ONCE, over a small declared
// structure, rather than on every message forever.
func (g Graph) Validate() error {
	// Iteration order over a map is random, and a validation error
	// that names a different path each run is one nobody can act on or
	// write a test for.
	ids := make([]string, 0, len(g))
	for id := range g {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	const (
		unvisited = 0
		onStack   = 1
		done      = 2
	)
	state := make(map[string]int, len(g))
	var stack []string

	var walk func(id string) error
	walk = func(id string) error {
		state[id] = onStack
		stack = append(stack, id)
		targets := append([]string(nil), g[id]...)
		sort.Strings(targets)
		for _, target := range targets {
			target = strings.TrimSpace(target)
			if target == "" {
				continue
			}
			switch state[target] {
			case onStack:
				// The cycle is the tail of the stack from the first
				// sighting of this node, plus the node again so the
				// loop closes visibly.
				start := 0
				for i, s := range stack {
					if s == target {
						start = i
						break
					}
				}
				path := append(append([]string(nil), stack[start:]...), target)
				return &ErrCycle{Path: path}
			case unvisited:
				if err := walk(target); err != nil {
					return err
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = done
		return nil
	}

	for _, id := range ids {
		if state[id] == unvisited {
			if err := walk(id); err != nil {
				return err
			}
		}
	}
	return nil
}

// WithEdges returns a copy of the graph with one bot's edge list
// replaced — what a create or update proposes, before it is written.
func (g Graph) WithEdges(id string, targets []string) Graph {
	out := make(Graph, len(g)+1)
	for k, v := range g {
		out[k] = v
	}
	out[id] = append([]string(nil), targets...)
	return out
}
