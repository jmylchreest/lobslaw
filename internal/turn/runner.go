package turn

import "context"

// Runner starts and resumes a turn. Channels depend on this and
// nothing in internal/compute, so a local agent and a remote
// backend are two implementations rather than two call shapes.
type Runner interface {
	Run(ctx context.Context, req Request) (*Response, error)
	Resume(ctx context.Context, req Request, prior []Message) (*Response, error)
}
