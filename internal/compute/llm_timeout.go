package compute

import (
	"context"
	"errors"
	"time"
)

// WithDefaultLLMTimeout bounds calls that have no caller deadline. An
// explicit deadline, including one longer than the default, is preserved.
func WithDefaultLLMTimeout(ctx context.Context, fallback time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, orDefault(fallback, DefaultLLMTimeout))
}

func chatWithTimeout(ctx context.Context, p LLMProvider, req ChatRequest, timeout time.Duration) (*ChatResponse, error) {
	var (
		callCtx context.Context
		cancel  context.CancelFunc
	)
	if timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		callCtx, cancel = WithDefaultLLMTimeout(ctx, DefaultLLMTimeout)
	}
	defer cancel()
	resp, err := p.Chat(callCtx, req)
	// A provider attempt expiring must still allow a fresh backup attempt.
	// Parent cancellation remains permanent and stops the chain.
	if err != nil && ctx.Err() == nil && errors.Is(callCtx.Err(), context.DeadlineExceeded) && errors.Is(err, context.DeadlineExceeded) {
		return nil, Transient(err)
	}
	return resp, err
}

type roleTimeoutProvider struct {
	provider LLMProvider
	timeout  time.Duration
}

func (p *roleTimeoutProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	return chatWithTimeout(ctx, p.provider, req, p.timeout)
}
