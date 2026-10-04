package compute

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"

	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// RemoteBackendProbeTimeout bounds the Ping that sets
// capabilities.compute.available. A hung compute node must not hang
// discovery.
const RemoteBackendProbeTimeout = 2 * time.Second

// RemoteRunner is a turn.Runner that runs turns on a compute node
// over cluster mTLS gRPC. Used when FunctionUIWeb is on and
// FunctionCompute is off.
type RemoteRunner struct {
	client lobslawv1.AgentServiceClient
}

// NewRemoteRunner wraps an already-dialled cluster connection.
func NewRemoteRunner(conn grpc.ClientConnInterface) *RemoteRunner {
	return &RemoteRunner{client: lobslawv1.NewAgentServiceClient(conn)}
}

func (r *RemoteRunner) Run(ctx context.Context, req turn.Request) (*turn.Response, error) {
	resp, err := r.client.RunTurn(ctx, requestToProto(req))
	if err != nil {
		return nil, fmt.Errorf("remote turn: %w", err)
	}
	return responseFromProto(resp), nil
}

func (r *RemoteRunner) Resume(ctx context.Context, req turn.Request, prior []turn.Message) (*turn.Response, error) {
	in := &lobslawv1.ResumeTurnRequest{Request: requestToProto(req)}
	in.ApprovalAction, in.ApprovalResource = turn.TakeApproval(ctx)
	for _, m := range prior {
		in.Prior = append(in.Prior, messageToProto(m))
	}
	resp, err := r.client.ResumeTurn(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("remote resume: %w", err)
	}
	if resp == nil {
		return nil, nil
	}
	return responseFromProto(resp.Response), nil
}

// Available reports whether the compute backend answers Ping.
func (r *RemoteRunner) Available(ctx context.Context) bool {
	if r == nil || r.client == nil {
		return false
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, RemoteBackendProbeTimeout)
		defer cancel()
	}
	_, err := r.client.Ping(ctx, &lobslawv1.PingRequest{})
	return err == nil
}
