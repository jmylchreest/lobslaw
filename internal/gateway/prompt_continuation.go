package gateway

import (
	"github.com/jmylchreest/lobslaw/internal/compute"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Continuation retains the gateway API while task runners share its codec.
type Continuation = compute.TaskContinuation

func continuationToProto(c *Continuation) *lobslawv1.Continuation {
	return compute.EncodeContinuation(c)
}
func continuationFromProto(p *lobslawv1.Continuation, caps compute.BudgetCaps) (*Continuation, error) {
	return compute.DecodeContinuation(p, caps)
}
