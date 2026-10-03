package compute

import (
	"context"
	"slices"
	"strings"

	"github.com/jmylchreest/lobslaw/pkg/promptgen"
)

type skillAccessKey struct{}

const retiredConsoleCodeTool string = "console_code"

// SkillAccessAllowed is checked by skill_view after hook parameter rewriting.
func SkillAccessAllowed(ctx context.Context, name string) bool {
	if check, ok := ctx.Value(skillAccessKey{}).(func(string) bool); ok {
		return check(name)
	}
	return true
}

func (a *Agent) withSkillAccess(ctx context.Context, req ProcessMessageRequest) context.Context {
	return context.WithValue(ctx, skillAccessKey{}, func(name string) bool { return a.skillAllowed(ctx, req, name) })
}

func isolatedTask(ctx context.Context, req ProcessMessageRequest) bool {
	scope, ok := ctx.Value(taskExecutionKey{}).(*taskExecution)
	return req.BotID != "" && ok && !(scope.scope.CoordinatorConversation && req.Bot != nil && req.Bot.IsCoordinator)
}

func (a *Agent) pinnedForTask(ctx context.Context, req *ProcessMessageRequest) promptgen.PinnedBlocks {
	if a.cfg.PinnedProvider == nil || req.Bot.IsSpecialist() || isolatedTask(ctx, *req) {
		return promptgen.PinnedBlocks{}
	}
	return a.cfg.PinnedProvider(sessionKeyFor(req), userIDFor(req))
}

func taskMemoryTool(name string) bool {
	return strings.HasPrefix(name, "memory_") || strings.HasPrefix(name, "session_") || name == "dream_recap" || strings.HasPrefix(name, "pinned_")
}

func taskToolRestriction(ctx context.Context, req ProcessMessageRequest, name string) string {
	// Old continuations may still advertise this retired tool. Credentials must
	// never enter model context, including ordinary and coordinator turns.
	if name == retiredConsoleCodeTool {
		return "sign-in credentials are only available through authenticated human login"
	}
	if name == "task_create" && TaskIDFrom(ctx) != "" {
		return "already executing a task; carry out the assigned work instead of queuing it again"
	}
	if req.Bot != nil && len(req.Bot.FilterTools([]Tool{{Name: name}})) == 0 {
		return "tool is excluded by bot tool restrictions"
	}
	if isolatedTask(ctx, req) && taskMemoryTool(name) {
		return "task context is isolated from saved memory"
	}
	return ""
}

func visibleTaskTools(ctx context.Context, req ProcessMessageRequest) []Tool {
	return slices.DeleteFunc(slices.Clone(req.Tools), func(t Tool) bool { return taskToolRestriction(ctx, req, t.Name) != "" })
}

func (a *Agent) visibleSkills(ctx context.Context, req ProcessMessageRequest, index []promptgen.SkillInfo) []promptgen.SkillInfo {
	return slices.DeleteFunc(slices.Clone(index), func(s promptgen.SkillInfo) bool { return !a.skillAllowed(ctx, req, s.Name) })
}

func (a *Agent) skillAllowed(ctx context.Context, req ProcessMessageRequest, name string) bool {
	if name == retiredConsoleCodeTool {
		return false
	}
	if req.Bot != nil && len(req.Bot.FilterTools([]Tool{{Name: name}})) == 0 {
		return false
	}
	return a.cfg.SkillAllowed == nil || a.cfg.SkillAllowed(ctx, name)
}
