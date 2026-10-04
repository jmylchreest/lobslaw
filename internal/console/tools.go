package console

import (
	"context"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

type ToolCatalogue interface {
	List() []ToolInfo
}

// ToolInfo is the wire shape for one selectable tool.
type ToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

func (s *Service) Tools(ctx context.Context, claims *types.Claims) ([]ToolInfo, error) {
	if _, err := caller(ctx, claims); err != nil {
		return nil, err
	}
	if s.cfg.Tools == nil {
		return nil, ErrUnavailable
	}
	rows := s.cfg.Tools.List()
	if rows == nil {
		rows = []ToolInfo{}
	}
	return rows, nil
}
