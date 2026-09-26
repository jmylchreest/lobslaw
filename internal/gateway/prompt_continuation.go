package gateway

import (
	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Continuation retains the gateway API while task runners share its codec.
type Continuation = turn.Continuation

func continuationRequest(req turn.Request, resp *turn.Response) turn.Request {
	if resp != nil {
		req.Spent = resp.BudgetState
	}
	return req
}

func continuationToProto(c *Continuation) *lobslawv1.Continuation {
	return turn.EncodeContinuation(c)
}

func continuationFromProto(p *lobslawv1.Continuation, caps turn.BudgetCaps) (*Continuation, error) {
	return turn.DecodeContinuation(p, caps)
}

func messageToProto(m turn.Message) *lobslawv1.SessionMessage   { return turn.MessageToProto(m) }
func messageFromProto(m *lobslawv1.SessionMessage) turn.Message { return turn.MessageFromProto(m) }
func claimsToProto(c *types.Claims) *lobslawv1.Claims           { return turn.ClaimsToProto(c) }
func claimsFromProto(c *lobslawv1.Claims) *types.Claims         { return turn.ClaimsFromProto(c) }
