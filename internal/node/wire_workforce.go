package node

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/internal/workforce"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

const workforceHandler = "workforce:routine"
const workforceApplyTimeout time.Duration = 5 * time.Second

func (n *Node) wireWorkforce() error {
	if n.raft == nil || n.store == nil {
		return nil
	}
	n.workforce = workforce.New(workforce.Config{Repository: &workforce.RaftRepository{Raft: n.raft, Store: n.store}, Bots: n.botSvc, Groups: n.groupSvc, Runner: compute.Adapt(n.agent), Leader: func() bool { return !n.cfg.RestoreMode && n.raft.IsLeader() }, Schedule: n.scheduleWorkforceRoutine, SaveTranscript: n.saveWorkforceTranscript, LoadTranscript: n.loadWorkforceTranscript, AuthorizeStep: n.authorizeWorkforceStep})
	if n.scheduler != nil && !n.cfg.RestoreMode {
		return n.scheduler.Handlers().RegisterTask(workforceHandler, n.runWorkforceSchedule)
	}
	return nil
}
func (n *Node) authorizeWorkforceStep(ctx context.Context, claims *types.Claims, action, project string) error {
	if n.policyEngine == nil || claims == nil {
		return workforce.ErrForbidden
	}
	decision, e := n.policyEngine.Evaluate(ctx, claims, "tool:exec", action)
	if e != nil {
		return e
	}
	switch decision.Effect {
	case types.EffectAllow:
		return nil
	case types.EffectRequireConfirmation:
		if turn.Approved(ctx, "tool:exec", action) {
			return nil
		}
		return &workforce.ApprovalRequired{Action: "tool:exec", Resource: action, Reason: "Approve browser action " + action + " for project " + project}
	default:
		return workforce.ErrForbidden
	}
}

func (n *Node) loadWorkforceTranscript(ctx context.Context, id string) ([]turn.Message, error) {
	svc := memory.NewSessionService(n.raft, n.store, memory.SessionConfig{})
	transcript, e := svc.Load(ctx, memory.SessionRef{Channel: "workforce", ChannelID: id})
	if e != nil {
		return nil, e
	}
	out := make([]turn.Message, 0, len(transcript))
	for _, m := range transcript {
		v := turn.Message{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, c := range m.ToolCalls {
			v.ToolCalls = append(v.ToolCalls, turn.ToolCall{ID: c.ID, Name: c.Name, Arguments: c.Arguments})
		}
		out = append(out, v)
	}
	return out, nil
}
func (n *Node) saveWorkforceTranscript(ctx context.Context, t *workforce.Task, messages []turn.Message) error {
	svc := memory.NewSessionService(n.raft, n.store, memory.SessionConfig{MaxMessages: n.cfg.Gateway.SessionMaxMessages})
	out := make([]memory.TranscriptMessage, 0, len(messages))
	for _, m := range messages {
		v := memory.TranscriptMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, call := range m.ToolCalls {
			v.ToolCalls = append(v.ToolCalls, memory.TranscriptToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
		}
		out = append(out, v)
	}
	_, e := svc.Append(ctx, memory.SessionRef{Channel: "workforce", ChannelID: t.ID, UserID: t.Owner}, t.ID, out)
	return e
}
func (n *Node) scheduleWorkforceRoutine(ctx context.Context, owner string, r *workforce.Routine, claims *types.Claims) error {
	if n.scheduler == nil || n.raft == nil || n.cfg.RestoreMode {
		return workforce.ErrUnavailable
	}
	if r.Schedule == "" {
		id := "workforce:" + r.ID
		data, e := proto.Marshal(&lobslawv1.LogEntry{Op: lobslawv1.LogOp_LOG_OP_DELETE, Id: id, Payload: &lobslawv1.LogEntry_ScheduledTask{ScheduledTask: &lobslawv1.ScheduledTaskRecord{Id: id}}})
		if e != nil {
			return e
		}
		result, e := n.raft.ApplyOrForward(ctx, data, workforceApplyTimeout)
		if e != nil {
			return e
		}
		if e, ok := result.(error); ok {
			return e
		}
		return nil
	}
	schedule, e := cron.ParseStandard(r.Schedule)
	if e != nil {
		return fmt.Errorf("routine schedule: %w", e)
	}
	rawClaims, e := json.Marshal(claims)
	if e != nil {
		return e
	}
	id := "workforce:" + r.ID
	rec := &lobslawv1.ScheduledTaskRecord{Id: id, Owner: owner, Name: r.Name, Schedule: r.Schedule, HandlerRef: workforceHandler, Enabled: r.Status == "approved", CreatedAt: timestamppb.Now(), NextRun: timestamppb.New(schedule.Next(time.Now())), Params: map[string]string{"routine_id": r.ID, "digest": r.ApprovedDigest, "claims": string(rawClaims)}}
	data, e := proto.Marshal(&lobslawv1.LogEntry{Op: lobslawv1.LogOp_LOG_OP_PUT, Id: id, Payload: &lobslawv1.LogEntry_ScheduledTask{ScheduledTask: rec}})
	if e != nil {
		return e
	}
	result, e := n.raft.ApplyOrForward(ctx, data, workforceApplyTimeout)
	if e != nil {
		return e
	}
	if e, ok := result.(error); ok {
		return e
	}
	return nil
}
func (n *Node) runWorkforceSchedule(ctx context.Context, task *lobslawv1.ScheduledTaskRecord) error {
	if n.workforce == nil || n.cfg.RestoreMode || task.NextRun == nil {
		return workforce.ErrUnavailable
	}
	var claims types.Claims
	if e := json.Unmarshal([]byte(task.Params["claims"]), &claims); e != nil {
		return e
	}
	return n.workforce.ScheduledRun(ctx, task.Owner, task.Params["routine_id"], task.NextRun.AsTime().UTC().Format(time.RFC3339Nano), task.Params["digest"], &claims)
}
