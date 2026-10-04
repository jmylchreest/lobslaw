package types

import "time"

// NodeID uniquely identifies a cluster node (UUID, assigned at
// first start, persisted).
type NodeID string

// NodeFunction is one of the roles a lobslaw binary can enable. A
// single binary can enable any subset; a single-node deployment
// enables all of them.
type NodeFunction string

// The available node functions. These are the wire values used in
// [cluster].functions and advertised in NodeInfo.
const (
	// FunctionMemory serves the vector + episodic memory store and
	// participates in the raft group backing it.
	FunctionMemory NodeFunction = "memory"
	// FunctionPolicy is DEPRECATED and normalised away to
	// FunctionMemory. It never selected anything of its own: both the
	// policy and memory gRPC services are gated on "this node hosts
	// raft", which memory already implies, and rule enforcement is
	// wired from the store rather than from this bit. An operator
	// declaring it got a memory node and no warning.
	//
	// Accepted so existing configs keep booting. See R25.
	FunctionPolicy NodeFunction = "policy"
	// FunctionCompute runs the agent loop, tool registry and builtins.
	FunctionCompute NodeFunction = "compute"
	// FunctionGateway is DEPRECATED and normalised to FunctionCompute.
	// It was a second switch for one decision: the gateway also needs
	// [gateway].enabled, and it cannot run without an agent, so the
	// function bit added nothing that the enable flag and the compute
	// function did not already say.
	//
	// Accepted so existing configs keep booting. See R25.
	FunctionGateway NodeFunction = "gateway"
	// FunctionStorage serves the object store and its sandboxed
	// nested filesystem mounts.
	FunctionStorage NodeFunction = "storage"
	// FunctionComputeTeams is coordinator selection, specialist
	// delegation and durable team work queues. Enabled explicitly or by --all.
	// Requires compute (normalised in). Does not require ui-web.
	FunctionComputeTeams NodeFunction = "compute-teams"
	// FunctionUIWeb is the browser console. Enabled explicitly or by --all.
	// Does NOT rewrite to compute — a web node may use remote compute.
	FunctionUIWeb NodeFunction = "ui-web"
)

// nodeFunctionDefinition records the canonical functions and their default
// selection in one place. Deprecated aliases are accepted by IsValid and
// NormalizeFunctions but never advertised as additional functions by --all.
type nodeFunctionDefinition struct {
	function       NodeFunction
	defaultEnabled bool
}

var nodeFunctions = [...]nodeFunctionDefinition{
	{function: FunctionMemory, defaultEnabled: true},
	{function: FunctionCompute, defaultEnabled: true},
	{function: FunctionStorage, defaultEnabled: true},
	{function: FunctionComputeTeams},
	{function: FunctionUIWeb},
}

// AllFunctions returns every canonical node function. Each call returns a fresh
// slice so callers cannot change the catalogue used by later selections.
func AllFunctions() []NodeFunction {
	functions := make([]NodeFunction, 0, len(nodeFunctions))
	for _, definition := range nodeFunctions {
		functions = append(functions, definition.function)
	}
	return functions
}

// DefaultFunctions returns the functions selected when neither flags nor config
// selects any functions. Explicit --all uses AllFunctions instead.
func DefaultFunctions() []NodeFunction {
	var functions []NodeFunction
	for _, definition := range nodeFunctions {
		if definition.defaultEnabled {
			functions = append(functions, definition.function)
		}
	}
	return functions
}

// IsValid reports whether f is a known function, so an unrecognised
// entry in [cluster].functions fails at boot rather than starting a
// node that silently serves nothing.
func (f NodeFunction) IsValid() bool {
	if f == FunctionPolicy || f == FunctionGateway {
		return true
	}
	for _, definition := range nodeFunctions {
		if f == definition.function {
			return true
		}
	}
	return false
}

// NodeInfo is advertised on registration and heartbeat. Peer
// identity for security comes from the mTLS cert SAN — ID is
// advisory.
type NodeInfo struct {
	ID           NodeID         `json:"id"`
	Functions    []NodeFunction `json:"functions"`
	Address      string         `json:"address"`
	Capabilities []string       `json:"capabilities,omitempty"`
	RaftMember   bool           `json:"raft_member"`
}

// HealthStatus is one node's self-reported health, as returned by
// the health endpoint and gossiped on heartbeat. Status is the
// rolled-up view; Components carries the per-subsystem detail behind
// it.
type HealthStatus struct {
	NodeID     NodeID            `json:"node_id"`
	Status     HealthLevel       `json:"status"`
	LastSeen   time.Time         `json:"last_seen"`
	Components []ComponentHealth `json:"components,omitempty"`
}

// HealthLevel grades a node or one of its components.
type HealthLevel string

// The health levels, best to worst.
const (
	// HealthHealthy means every component is serving normally.
	HealthHealthy HealthLevel = "healthy"
	// HealthDegraded means the node is still serving but at least one
	// component is impaired — it should not be taken out of rotation.
	HealthDegraded HealthLevel = "degraded"
	// HealthUnhealthy means the node cannot serve its functions.
	HealthUnhealthy HealthLevel = "unhealthy"
)

// ComponentHealth is one subsystem's contribution to a node's
// HealthStatus. Error carries the reason whenever Status is not
// healthy.
type ComponentHealth struct {
	Name   string      `json:"name"`
	Status HealthLevel `json:"status"`
	Error  string      `json:"error,omitempty"`
}

// NormalizeFunctions resolves deprecated aliases and removes
// duplicates, preserving declaration order.
//
// It reports the aliases it rewrote so the caller can warn once,
// rather than every consumer of the list rediscovering that policy
// and memory are the same thing.
func NormalizeFunctions(fns []NodeFunction) (out []NodeFunction, rewrote []NodeFunction) {
	seen := make(map[NodeFunction]bool, len(fns))
	add := func(f NodeFunction) {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	for _, f := range fns {
		switch f {
		case FunctionPolicy:
			rewrote = append(rewrote, f)
			add(FunctionMemory)
		case FunctionGateway:
			// The gateway needs an agent to hand turns to, and
			// [gateway].enabled already decides whether it listens.
			rewrote = append(rewrote, f)
			add(FunctionCompute)
		default:
			add(f)
		}
	}
	// memory and storage require each other: the storage stage is
	// gated behind raft, which only memory provides, and a memory node
	// without storage is rejected outright. Expanding each into both
	// states the coupling once, here, instead of making an operator
	// discover it from a validation error telling them to add a
	// function they never had a choice about.
	if seen[FunctionMemory] || seen[FunctionStorage] {
		add(FunctionMemory)
		add(FunctionStorage)
	}
	// Teams cannot run without an agent. Unlike gateway, the function
	// bit is the real switch — [compute-teams].enabled maps onto it —
	// so we add compute rather than rewriting the name away.
	if seen[FunctionComputeTeams] {
		add(FunctionCompute)
	}
	return out, rewrote
}
