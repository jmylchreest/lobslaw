package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jmylchreest/lobslaw/internal/computer"
	"github.com/jmylchreest/lobslaw/internal/workforce"
)

type workforceComputerAuthorizer struct{ service *workforce.Service }

func (a workforceComputerAuthorizer) AuthorizeProject(ctx context.Context, owner, project string) error {
	err := a.service.AuthorizeProject(ctx, owner, project)
	switch {
	case errors.Is(err, workforce.ErrNotFound):
		return computer.ErrNotFound
	case errors.Is(err, workforce.ErrForbidden):
		return computer.ErrForbidden
	default:
		return err
	}
}

func (n *Node) wireWorkforceComputer() error {
	if n.workforce == nil {
		return nil
	}
	n.wireComputer(workforceComputerAuthorizer{service: n.workforce})
	if n.computer == nil {
		return nil
	}
	n.workforce.SetStepExecutor(func(ctx context.Context, owner, project string, step workforce.RoutineStep) error {
		err := n.computer.ExecuteStep(ctx, owner, project, computer.RoutineStep{
			Action: step.Action, Selector: step.Selector, Value: step.Value, URL: step.URL,
			Description: step.Description, Sensitive: step.Sensitive, InputMode: step.InputMode,
		})
		switch {
		case errors.Is(err, computer.ErrTakeover):
			return fmt.Errorf("%w: %v", workforce.ErrBlocked, err)
		case errors.Is(err, computer.ErrManual):
			return fmt.Errorf("%w: %v", workforce.ErrManualStep, err)
		default:
			return err
		}
	})
	n.workforce.SetAttentionSource(func(ctx context.Context, owner string) ([]workforce.AttentionItem, error) {
		takeovers, err := n.computer.Takeovers(ctx, owner)
		if err != nil {
			return nil, err
		}
		items := make([]workforce.AttentionItem, 0, len(takeovers))
		for _, takeover := range takeovers {
			at, err := time.Parse(time.RFC3339Nano, takeover.CreatedAt)
			if err != nil {
				return nil, fmt.Errorf("browser takeover timestamp: %w", err)
			}
			items = append(items, workforce.AttentionItem{
				ID: "computer:" + takeover.ProjectID, Kind: "takeover", ProjectID: takeover.ProjectID,
				Title: "Browser under your control", Detail: "Return control when you have finished so browser tasks can continue.",
				CreatedAt: at, Href: "/projects/" + takeover.ProjectID + "/computer",
			})
		}
		return items, nil
	})
	return n.registerComputerTools(func(ctx context.Context) (string, string, error) {
		project, err := n.workforce.AgentProject(ctx)
		if err != nil {
			return "", "", err
		}
		return project.Owner, project.ID, nil
	})
}
