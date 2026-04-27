package node

import (
	"fmt"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// WireStage is one phase of node assembly. Each subsystem (raft,
// soul, compute, gateway, etc.) lives in its own wire_<name>.go file
// and contributes a stage entry to nodeWireStages. The orchestration
// loop in New() walks the slice in order, skips any stage whose
// gate returns false, and bails on the first error.
//
// Adding a subsystem = drop a new file in this package + register a
// stage. The slice is the assembly order; dependencies between
// stages (raft before policy, storage before skills) are encoded by
// position, not by an explicit DAG. Keeping it flat means an operator
// reading the file can trace exactly what fires when.
type WireStage struct {
	// Name appears in error messages as "wire <name>: ...". Keep it
	// short and dash-separated.
	Name string

	// Gate decides whether this stage runs for the current node's
	// configuration. Nil = always run. Non-nil predicates capture the
	// "only when this function is enabled" or "only when raft is up"
	// conditional logic that used to live as inline if-blocks in New.
	Gate func(cfg Config) bool

	// Wire performs the actual setup. It MUST mutate *Node fields
	// rather than returning a value — subsequent stages read those
	// fields. Errors abort the New() call; the caller has already
	// arranged closePartial() to clean up partial wiring.
	Wire func(*Node) error
}

// runWireStages walks the list, gating each stage on its predicate.
// On the first error, the caller is expected to call n.closePartial()
// and return — runWireStages itself only formats the error.
func (n *Node) runWireStages(stages []WireStage) error {
	for _, s := range stages {
		if s.Gate != nil && !s.Gate(n.cfg) {
			continue
		}
		if err := s.Wire(n); err != nil {
			return fmt.Errorf("wire %s: %w", s.Name, err)
		}
	}
	return nil
}

// gateAlways is the explicit "always run" sentinel — equivalent to
// leaving Gate nil but useful when a stage's intent is "explicitly
// runs regardless of node role" rather than "I forgot to set Gate."
func gateAlways(_ Config) bool { return true }

// gateRaft selects stages that need the raft stack — anything that
// touches the FSM or replicates state.
func gateRaft(cfg Config) bool { return needsRaft(cfg.Functions) }

// gateCompute selects stages that need the compute stack — agent,
// builtins, tool registry.
func gateCompute(cfg Config) bool { return has(cfg.Functions, types.FunctionCompute) }

// gateGateway selects stages that need the gateway. Both the
// function bit AND the explicit Enabled toggle must be set: an
// operator running the gateway function for testing might leave it
// disabled in config to bring up the rest of the cluster first.
func gateGateway(cfg Config) bool {
	return has(cfg.Functions, types.FunctionGateway) && cfg.Gateway.Enabled
}

// gateStorage selects stages that need the storage function. Storage
// is sub-conditional on raft (storage uses raft for replicated
// mount config) so callers compose: gateRaft AND gateStorage.
func gateStorage(cfg Config) bool { return has(cfg.Functions, types.FunctionStorage) }

// gateAuth selects stages that need the JWT auth path. Triggered by
// either an HS256 secret being declared OR a JWKS URL configured —
// having neither leaves the node in anonymous-with-default-scope mode.
func gateAuth(cfg Config) bool {
	return cfg.Auth.AllowHS256 || cfg.Auth.JWKSURL != ""
}

// gateBroadcast selects the UDP auto-discovery broadcaster. Off by
// default (operators on isolated networks may not want UDP traffic);
// turning it on opts the node into the multicast announce/listen
// loop.
func gateBroadcast(cfg Config) bool { return cfg.BroadcastEnabled }

// gateRaftAnd composes gateRaft with another predicate. Used for
// stages that need raft AND some specific role (storage, etc.) —
// reads as "raft AND storage."
func gateRaftAnd(other func(Config) bool) func(Config) bool {
	return func(cfg Config) bool { return gateRaft(cfg) && other(cfg) }
}

// nodeWireStages is the canonical assembly order. Each entry maps to
// a method on *Node defined in wire_subsystems.go (or wire_<name>.go
// for the larger subsystems). Adding a subsystem = drop a stage
// method + register it here.
//
// Order matters: a stage that reads another's output must come
// later. The grouping below preserves the dependency chain that
// used to live as nested if-blocks in New:
//
//  1. raft (and everything that needs raft) — only on memory/policy nodes
//  2. audit — runs everywhere, needs nothing
//  3. soul fallback — only when raft path didn't construct an Adjuster
//  4. compute — depends on soul + (optionally) skills/storage from the raft block
//  5. auth — independent of functions, only when a validation method is declared
//  6. gateway — depends on compute + auth
//  7. discovery — always runs, glues NodeService onto the gRPC server
//  8. broadcast — optional UDP auto-discovery
func nodeWireStages() []WireStage {
	return []WireStage{
		// Raft cluster stack. Each sub-stage runs only when this
		// node hosts raft (memory or policy function); they share
		// the gateRaft predicate.
		{Name: "raft", Gate: gateRaft, Wire: (*Node).wireRaftStage},
		{Name: "policy-svc", Gate: gateRaft, Wire: (*Node).wirePolicyService},
		{Name: "memory-svc", Gate: gateRaft, Wire: (*Node).wireMemoryService},
		{Name: "soul-raft", Gate: gateRaft, Wire: (*Node).wireSoulRaft},
		{Name: "plan-svc", Gate: gateRaft, Wire: (*Node).wirePlanService},
		{Name: "scheduler", Gate: gateRaft, Wire: (*Node).wireScheduler},
		{Name: "storage", Gate: gateRaftAnd(gateStorage), Wire: (*Node).wireStorageStage},
		{Name: "skills", Gate: gateRaft, Wire: (*Node).wireSkills},

		// Always-on or function-gated platform stages.
		{Name: "audit", Wire: (*Node).wireAuditStage},
		{Name: "soul-fallback", Wire: (*Node).wireSoulFallback},
		{Name: "compute", Gate: gateCompute, Wire: (*Node).wireComputeStage},
		{Name: "auth", Gate: gateAuth, Wire: (*Node).wireAuthStage},
		{Name: "gateway", Gate: gateGateway, Wire: (*Node).wireGatewayStage},
		{Name: "discovery", Wire: (*Node).wireDiscoveryStage},
		{Name: "broadcast", Gate: gateBroadcast, Wire: (*Node).wireBroadcastStage},
	}
}
