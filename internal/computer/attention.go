package computer

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Takeover struct {
	ProjectID string
	CreatedAt string
}

// Takeovers is a derived view of owner control fences, including after restart.
// It reads only small local control records, never browser profiles or cookies.
func (s *Service) Takeovers(ctx context.Context, principal string) ([]Takeover, error) {
	if s == nil || s.auth == nil || s.cfg.Root == "" {
		return nil, ErrUnavailable
	}
	if principal == "" {
		return nil, ErrForbidden
	}
	entries, err := os.ReadDir(s.cfg.Root)
	if errors.Is(err, os.ErrNotExist) {
		return []Takeover{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("computer attention: %w", err)
	}
	out := []Takeover{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.cfg.Root, entry.Name(), "control.json"))
		if err != nil {
			continue
		}
		var state State
		if json.Unmarshal(data, &state) != nil || state.Control != "human" {
			continue
		}
		key := fmt.Sprintf("%x", sha256.Sum256([]byte(principal+"\x00"+state.ProjectID)))
		if key != entry.Name() || s.AuthorizeProject(ctx, principal, state.ProjectID) != nil {
			continue
		}
		out = append(out, Takeover{ProjectID: state.ProjectID, CreatedAt: state.UpdatedAt.Format(time.RFC3339)})
	}
	return out, nil
}
