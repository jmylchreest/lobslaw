package workforce

import (
	"encoding/json"
	"fmt"
	"strings"
)

type dependencyContext struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Result    string     `json:"result"`
	Artifacts []Artifact `json:"artifacts"`
	Truncated bool       `json:"truncated,omitempty"`
}

func taskContext(st *State, t *Task) (string, error) {
	deps := make([]dependencyContext, 0, len(t.DependsOn))
	remaining := MaxDependencyContextBytes
	for _, id := range t.DependsOn {
		d := st.Tasks[id]
		if d == nil || d.Status != StatusDone {
			return "", ErrBlocked
		}
		result := d.Result
		limit := min(MaxDependencyResultBytes, remaining)
		truncated := len(result) > limit
		if truncated {
			result = strings.ToValidUTF8(result[:limit], "�")
		}
		remaining -= min(len(result), remaining)
		deps = append(deps, dependencyContext{ID: id, Title: preview(d.Title), Result: result, Artifacts: publicArtifacts(d), Truncated: truncated})
	}
	body, e := json.Marshal(struct {
		Project      Project             `json:"project"`
		TaskID       string              `json:"task_id"`
		ParentID     string              `json:"parent_id,omitempty"`
		Criteria     []string            `json:"acceptance_criteria"`
		Progress     string              `json:"progress,omitempty"`
		Dependencies []dependencyContext `json:"dependencies"`
	}{st.Project, t.ID, t.ParentID, t.AcceptanceCriteria, t.Progress, deps})
	if e != nil {
		return "", fmt.Errorf("workforce context: %w", e)
	}
	if len(body) > MaxTaskContextBytes {
		return "", fmt.Errorf("%w: project context exceeds dispatch limit", ErrInvalid)
	}
	return "<untrusted:workforce-context>\n" + string(body) + "\n</untrusted:workforce-context>", nil
}

func preview(text string) string {
	if len(text) <= MaxAgentPreviewBytes {
		return text
	}
	return strings.ToValidUTF8(text[:MaxAgentPreviewBytes], "�") + "…"
}
