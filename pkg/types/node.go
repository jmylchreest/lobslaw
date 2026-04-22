package types

import "time"

// NodeID uniquely identifies a cluster node (UUID, assigned at
// first start, persisted).
type NodeID string

// NodeFunction is one of the four roles a lobslaw binary can
// enable. A single binary can enable any subset.
type NodeFunction string

const (
	FunctionMemory  NodeFunction = "memory"
	FunctionPolicy  NodeFunction = "policy"
	FunctionCompute NodeFunction = "compute"
	FunctionGateway NodeFunction = "gateway"
)

func (f NodeFunction) IsValid() bool {
	switch f {
	case FunctionMemory, FunctionPolicy, FunctionCompute, FunctionGateway:
		return true
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

type HealthStatus struct {
	NodeID     NodeID            `json:"node_id"`
	Status     HealthLevel       `json:"status"`
	LastSeen   time.Time         `json:"last_seen"`
	Components []ComponentHealth `json:"components,omitempty"`
}

type HealthLevel string

const (
	HealthHealthy   HealthLevel = "healthy"
	HealthDegraded  HealthLevel = "degraded"
	HealthUnhealthy HealthLevel = "unhealthy"
)

type ComponentHealth struct {
	Name   string      `json:"name"`
	Status HealthLevel `json:"status"`
	Error  string      `json:"error,omitempty"`
}
