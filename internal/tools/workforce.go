package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/workforce"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// RegisterWorkforceBuiltins exposes no approval, claim, owner or project
// override. The service binds every operation to the worker's original claim.
func RegisterWorkforceBuiltins(b *Builtins, s *workforce.Service) error {
	for _, def := range WorkforceToolDefs() {
		name := def.Name
		if e := b.Register(name, func(ctx context.Context, args map[string]string) ([]byte, int, error) {
			out, e := runWorkforceTool(ctx, s, name, args)
			if e != nil {
				return nil, 1, fmt.Errorf("%s: %w", name, e)
			}
			raw, e := json.Marshal(out)
			if e != nil {
				return nil, 1, e
			}
			return raw, 0, nil
		}); e != nil {
			return e
		}
	}
	return nil
}
func WorkforceToolDefs() []*types.ToolDef {
	specs := []struct{ name, description, schema string }{
		{"workforce_project_get", "Read this turn's owned project context and bot roster. Available only inside a running workforce task or project conversation.", `{"type":"object","properties":{},"additionalProperties":false}`},
		{"workforce_task_list", "List this project's recent task summaries, dependencies, progress, result previews and artifact references. Use workforce_task_get for full results.", `{"type":"object","properties":{},"additionalProperties":false}`},
		{"workforce_task_get", "Read a task and its actual result/artifact references in the current project. Omit task_id for your own task.", `{"type":"object","properties":{"task_id":{"type":"string"}},"additionalProperties":false}`},
		{"workforce_task_create", "Create durable work for a bot on the current project's roster. Workers execute it asynchronously. depends_on references existing same-project tasks; their real outputs are delivered at dispatch. Creating a task does not mean it is complete.", `{"type":"object","properties":{"title":{"type":"string"},"instructions":{"type":"string"},"assignee_bot_id":{"type":"string"},"depends_on":{"type":"array","items":{"type":"string"}},"acceptance_criteria":{"type":"array","items":{"type":"string"}}},"required":["title","instructions"],"additionalProperties":false}`},
		{"workforce_task_checkpoint", "Persist concise progress for your own running task. Record verified observations and remaining work; this does not mark the task done.", `{"type":"object","properties":{"progress":{"type":"string"}},"required":["progress"],"additionalProperties":false}`},
		{"workforce_task_block", "Stop your current task and ask the human owner a specific question. Attention will show the blocker. The owner can answer and resume; this grants no approval or authority.", `{"type":"object","properties":{"question":{"type":"string"}},"required":["question"],"additionalProperties":false}`},
	}
	out := make([]*types.ToolDef, 0, len(specs))
	for _, spec := range specs {
		out = append(out, &types.ToolDef{Name: spec.name, Path: compute.BuiltinScheme + spec.name, Description: spec.description, ParametersSchema: []byte(spec.schema), RiskTier: types.RiskReversible})
	}
	return out
}
func runWorkforceTool(ctx context.Context, s *workforce.Service, name string, args map[string]string) (any, error) {
	if s == nil {
		return nil, workforce.ErrUnavailable
	}
	switch name {
	case "workforce_project_get":
		return s.AgentProject(ctx)
	case "workforce_task_list":
		tasks, e := s.AgentTasks(ctx)
		return map[string]any{"tasks": tasks}, e
	case "workforce_task_get":
		return s.AgentTask(ctx, args["task_id"])
	case "workforce_task_checkpoint":
		return s.AgentCheckpoint(ctx, args["progress"])
	case "workforce_task_block":
		return s.AgentBlock(ctx, args["question"])
	case "workforce_task_create":
		t := workforce.Task{Title: args["title"], Instructions: args["instructions"], AssigneeBotID: args["assignee_bot_id"]}
		for key, target := range map[string]*[]string{"depends_on": &t.DependsOn, "acceptance_criteria": &t.AcceptanceCriteria} {
			if value := args[key]; value != "" {
				if e := json.Unmarshal([]byte(value), target); e != nil {
					return nil, fmt.Errorf("%w: %s must be an array of strings", workforce.ErrInvalid, key)
				}
			}
		}
		return s.AgentCreateTask(ctx, t)
	default:
		return nil, workforce.ErrInvalid
	}
}
