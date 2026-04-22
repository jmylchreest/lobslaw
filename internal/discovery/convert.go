package discovery

import (
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// fromProto converts the wire-format NodeInfo into the typed
// internal form.
func fromProto(p *lobslawv1.NodeInfo) types.NodeInfo {
	if p == nil {
		return types.NodeInfo{}
	}
	funcs := make([]types.NodeFunction, 0, len(p.Functions))
	for _, f := range p.Functions {
		funcs = append(funcs, types.NodeFunction(f))
	}
	return types.NodeInfo{
		ID:           types.NodeID(p.Id),
		Functions:    funcs,
		Address:      p.Address,
		Capabilities: p.Capabilities,
		RaftMember:   p.RaftMember,
	}
}

// toProto is the reverse — used when responding with the peer list.
func toProto(n types.NodeInfo) *lobslawv1.NodeInfo {
	funcs := make([]string, 0, len(n.Functions))
	for _, f := range n.Functions {
		funcs = append(funcs, string(f))
	}
	return &lobslawv1.NodeInfo{
		Id:           string(n.ID),
		Functions:    funcs,
		Address:      n.Address,
		Capabilities: n.Capabilities,
		RaftMember:   n.RaftMember,
	}
}
