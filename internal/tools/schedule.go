package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/scheduler"
	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Shared ULID entropy for schedule IDs. Same monotonic pattern as
// memIDEntropy elsewhere.

// ScheduleHandlerRef is the handler_ref value written on every
// task created via schedule_create. Matches the existing
// node.AgentTurnHandlerRef so scheduler-created tasks dispatch
// through the same agent-turn path as operator-defined ones.
const ScheduleHandlerRef = scheduler.AgentTurnHandlerRef

// ScheduleConfig wires the schedule_* builtins. Store lets list/get
// read directly from the scheduled-tasks bucket without an RPC
// round-trip; Raft lets create/delete propose entries.
type ScheduleConfig struct {
	Store *memory.Store
	Raft  memoryRaftApplier
}

// RegisterScheduleBuiltins installs schedule_create / list / get /
// delete. Nil Store or Raft skips registration — a compute-only
// node without persistence shouldn't offer persistent scheduling.
func RegisterScheduleBuiltins(b *Builtins, cfg ScheduleConfig) error {
	if cfg.Store == nil || cfg.Raft == nil {
		return errors.New("schedule builtins: Store + Raft required")
	}
	if err := b.Register("schedule_create", newScheduleCreateHandler(cfg.Raft)); err != nil {
		return err
	}
	if err := b.Register("schedule_list", newScheduleListHandler(cfg.Store)); err != nil {
		return err
	}
	if err := b.Register("schedule_get", newScheduleGetHandler(cfg.Store)); err != nil {
		return err
	}
	if err := b.Register("schedule_update", newScheduleUpdateHandler(cfg.Store, cfg.Raft)); err != nil {
		return err
	}
	return b.Register("schedule_delete", newScheduleDeleteHandler(cfg.Store, cfg.Raft))
}

func ScheduleToolDefs() []*types.ToolDef {
	return []*types.ToolDef{
		{
			Name:        "schedule_create",
			Path:        compute.BuiltinScheme + "schedule_create",
			Description: "Create persistent recurring work for yourself. Use for ongoing responsibilities and daily reports. Each named-bot occurrence is a durable task in your own inbox with visible results and approvals; it runs without an open chat. Supply a self-contained prompt describing the work and what counts as a meaningful change. Do not create another schedule when executing an existing occurrence. Use notify_on=always only when the user asks for every outcome, match (default) for attention only, or never for silent outcomes. For match, call notify only when the user needs attention. Use schedule_update checkpoint to remember progress across runs; previous result and checkpoint are provided on the next run. Schedule tools return an id: confirm that receipt rather than promising unsaved work.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"name": {"type": "string", "description": "Human-readable name."},
					"when": {"type": "string", "description": "Cron expression OR natural language: 'every 5m', 'every 1h', 'daily 08:00', 'hourly'."},
					"prompt": {"type": "string", "description": "Self-instruction fired each tick."},
					"timezone": {"type": "string", "description": "IANA timezone, e.g. Europe/London. Defaults to the caller's timezone or UTC."},
					"notify_on": {"type": "string", "enum": ["always", "match", "never"], "description": "Notification policy. Default: match."}
				},
				"required": ["name", "when", "prompt"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name: "schedule_update", Path: compute.BuiltinScheme + "schedule_update",
			Description:      "Edit your recurring work: pause/resume with enabled, change when/prompt/notify_on, or save a bounded checkpoint of what you last processed. Checkpoints survive restarts and are provided to subsequent runs. Pausing stops future occurrences, not work already queued. Use schedule_get/list to inspect results and dispatch errors.",
			ParametersSchema: []byte(`{"type":"object","properties":{"id":{"type":"string"},"enabled":{"type":"boolean"},"when":{"type":"string"},"timezone":{"type":"string"},"prompt":{"type":"string"},"notify_on":{"type":"string","enum":["always","match","never"]},"checkpoint":{"type":"string","description":"Progress to preserve across runs; maximum 4000 characters."}},"required":["id"],"additionalProperties":false}`),
			RiskTier:         types.RiskReversible,
		},
		{
			Name:        "schedule_list",
			Path:        compute.BuiltinScheme + "schedule_list",
			Description: "List all scheduled tasks this node owns. Returns {id, name, schedule, enabled, next_run, last_run} per task. Present as a markdown table — this is fact-dense enumerable content.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {},
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name:        "schedule_get",
			Path:        compute.BuiltinScheme + "schedule_get",
			Description: "Fetch a single scheduled task by id. Returns the full record including the agent prompt that fires each tick.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"id": {"type": "string", "description": "Scheduled task ULID."}
				},
				"required": ["id"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name:        "schedule_delete",
			Path:        compute.BuiltinScheme + "schedule_delete",
			Description: "Delete a scheduled task by id. Use when the user asks to cancel, stop, or remove a recurring check. Reversible in the sense that it's audit-logged but the task stops firing immediately.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"id": {"type": "string", "description": "Scheduled task ULID."}
				},
				"required": ["id"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
	}
}

func newScheduleCreateHandler(raft memoryRaftApplier) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		name := strings.TrimSpace(args["name"])
		if name == "" {
			return nil, 2, errors.New("schedule_create: name is required")
		}
		when := strings.TrimSpace(args["when"])
		if when == "" {
			return nil, 2, errors.New("schedule_create: when is required (cron or natural language)")
		}
		prompt := strings.TrimSpace(args["prompt"])
		if prompt == "" {
			return nil, 2, errors.New("schedule_create: prompt is required")
		}
		notifyOn := strings.TrimSpace(strings.ToLower(args["notify_on"]))
		if notifyOn == "" {
			notifyOn = DefaultScheduleNotifyOn
		}
		if notifyOn != "always" && notifyOn != "match" && notifyOn != "never" {
			return nil, 2, fmt.Errorf("schedule_create: notify_on must be always|match|never, got %q", notifyOn)
		}

		caller, ok := turn.IdentityFrom(ctx)
		if !ok || caller.Principal.IsZero() {
			return nil, 2, errors.New("schedule_create: authenticated identity required")
		}
		cron, err := scheduleCron(ctx, when, args["timezone"])
		if err != nil {
			return nil, 2, fmt.Errorf("schedule_create: %w", err)
		}

		parsed, err := scheduler.ParseAgentSchedule(cron)
		if err != nil {
			return nil, 2, fmt.Errorf("schedule_create: %w", err)
		}
		if len(prompt) > 8000 || len(name) > 200 {
			return nil, 2, errors.New("schedule_create: name or prompt too long")
		}

		id := ids.New()
		task := &lobslawv1.ScheduledTaskRecord{
			Id:         id,
			Name:       name,
			Schedule:   cron,
			HandlerRef: ScheduleHandlerRef,
			Params: map[string]string{
				"prompt":    prompt,
				"notify_on": notifyOn,
			},
			Enabled:   true,
			CreatedAt: timestamppb.Now(),
			Owner:     identityOwner(ctx),
			NextRun:   timestamppb.New(parsed.Next(time.Now())),
		}
		if caller.BotID != "" {
			owner := requesterLabel(caller)
			if !strings.HasPrefix(owner, "user:") {
				return nil, 2, errors.New("schedule_create: bot must have an authenticated human owner")
			}
			task.Owner = "bot:" + caller.BotID
			task.Params["requested_by"] = owner
			task.Params["scope"] = caller.Scope
			roles, _ := json.Marshal(caller.Roles)
			task.Params["roles"] = string(roles)
		}
		entry := &lobslawv1.LogEntry{
			Op: lobslawv1.LogOp_LOG_OP_PUT,
			Id: id,
			Payload: &lobslawv1.LogEntry_ScheduledTask{
				ScheduledTask: task,
			},
		}
		data, err := proto.Marshal(entry)
		if err != nil {
			return nil, 1, fmt.Errorf("schedule_create: marshal: %w", err)
		}
		if _, err := raft.Apply(data, writeApplyTimeout); err != nil {
			return nil, 1, fmt.Errorf("schedule_create: raft apply: %w", err)
		}
		out, _ := json.Marshal(map[string]any{
			"id":       id,
			"name":     name,
			"schedule": cron,
		})
		return out, 0, nil
	}
}

func newScheduleListHandler(store *memory.Store) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		type view struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Schedule  string `json:"schedule"`
			Enabled   bool   `json:"enabled"`
			NextRun   string `json:"next_run,omitempty"`
			LastRun   string `json:"last_run,omitempty"`
			Prompt    string `json:"prompt,omitempty"`
			LastError string `json:"last_error,omitempty"`
			LastItem  string `json:"last_item_id,omitempty"`
		}
		var tasks []view
		err := store.ForEach(memory.BucketScheduledTasks, func(_ string, raw []byte) error {
			var t lobslawv1.ScheduledTaskRecord
			if err := proto.Unmarshal(raw, &t); err != nil {
				return nil
			}
			if !ownsSchedule(ctx, &t) {
				return nil
			}
			v := view{
				ID:        t.Id,
				Name:      t.Name,
				Schedule:  t.Schedule,
				Enabled:   t.Enabled,
				Prompt:    t.Params["prompt"],
				LastError: t.Params["last_error"], LastItem: t.Params["last_item_id"],
			}
			if t.NextRun != nil {
				v.NextRun = formatTimeForUser(ctx, t.NextRun.AsTime())
			}
			if t.LastRun != nil {
				v.LastRun = formatTimeForUser(ctx, t.LastRun.AsTime())
			}
			tasks = append(tasks, v)
			return nil
		})
		if err != nil {
			return nil, 1, fmt.Errorf("schedule_list: %w", err)
		}
		sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
		out, _ := json.Marshal(map[string]any{"count": len(tasks), "tasks": tasks})
		return out, 0, nil
	}
}

func newScheduleGetHandler(store *memory.Store) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		id := strings.TrimSpace(args["id"])
		if id == "" {
			return nil, 2, errors.New("schedule_get: id is required")
		}
		raw, err := store.Get(memory.BucketScheduledTasks, id)
		if err != nil {
			return nil, 1, fmt.Errorf("schedule_get: %w", err)
		}
		if raw == nil {
			return nil, 2, fmt.Errorf("schedule_get: task %q not found", id)
		}
		var t lobslawv1.ScheduledTaskRecord
		if err := proto.Unmarshal(raw, &t); err != nil {
			return nil, 1, fmt.Errorf("schedule_get: decode: %w", err)
		}
		// Same answer for "not yours" as for "not there", so an id
		// probe cannot map another person's schedule.
		if !ownsSchedule(ctx, &t) {
			return nil, 2, fmt.Errorf("schedule_get: task %q not found", id)
		}
		view := map[string]any{
			"id":       t.Id,
			"name":     t.Name,
			"schedule": t.Schedule,
			"enabled":  t.Enabled,
			"params":   t.Params,
		}
		if t.NextRun != nil {
			view["next_run"] = t.NextRun.AsTime().Format(time.RFC3339)
		}
		if t.LastRun != nil {
			view["last_run"] = t.LastRun.AsTime().Format(time.RFC3339)
		}
		out, _ := json.Marshal(view)
		return out, 0, nil
	}
}

func newScheduleDeleteHandler(store *memory.Store, raft memoryRaftApplier) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		id := strings.TrimSpace(args["id"])
		if id == "" {
			return nil, 2, errors.New("schedule_delete: id is required")
		}
		// Payload shape disambiguates the bucket for the FSM's
		// applyDelete — same pattern memory.Service uses for its
		// ForgetRequest handling.
		ok, err := scheduleActionable(ctx, store, id)
		if err != nil {
			return nil, 1, fmt.Errorf("schedule_delete: %w", err)
		}
		if !ok {
			return nil, 2, fmt.Errorf("schedule_delete: task %q not found", id)
		}
		entry := &lobslawv1.LogEntry{
			Op: lobslawv1.LogOp_LOG_OP_DELETE,
			Id: id,
			Payload: &lobslawv1.LogEntry_ScheduledTask{
				ScheduledTask: &lobslawv1.ScheduledTaskRecord{Id: id},
			},
		}
		data, err := proto.Marshal(entry)
		if err != nil {
			return nil, 1, fmt.Errorf("schedule_delete: marshal: %w", err)
		}
		if _, err := raft.Apply(data, writeApplyTimeout); err != nil {
			return nil, 1, fmt.Errorf("schedule_delete: raft apply: %w", err)
		}
		out, _ := json.Marshal(map[string]any{"id": id, "deleted": true})
		return out, 0, nil
	}
}

// normaliseToCron accepts either a cron expression (five fields, optionally
// prefixed with TZ or CRON_TZ)
// or a natural-language phrase. Natural forms supported:
//
//	"every 30s"  → cron can't express sub-minute — rejected
//	"every 5m"   → "*/5 * * * *"
//	"every 1h"   → "0 */1 * * *"
//	"hourly"     → "0 * * * *"
//	"daily HH:MM"→ "M H * * *"
//	"every day HH:MM" → same
//
// Cron-looking input is validated by ParseAgentSchedule before saving.
var everyRe = regexp.MustCompile(`^every\s+(\d+)\s*(s|sec|seconds?|m|min|minutes?|h|hr|hours?)$`)
var dailyRe = regexp.MustCompile(`^(?:daily|every\s+day)\s+(\d{1,2}):(\d{2})$`)

func normaliseToCron(when string) (string, error) {
	w := strings.TrimSpace(strings.ToLower(when))
	// Already looks like a cron expression (5+ fields separated by whitespace)?
	if fields := strings.Fields(w); len(fields) >= 5 {
		return when, nil // validated before saving
	}
	if w == "hourly" {
		return "0 * * * *", nil
	}
	if m := everyRe.FindStringSubmatch(w); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 {
			return "", fmt.Errorf("invalid interval %q", when)
		}
		unit := m[2]
		switch {
		case strings.HasPrefix(unit, "s"):
			return "", fmt.Errorf("sub-minute schedules not supported (got %q); use minute granularity", when)
		case strings.HasPrefix(unit, "m"):
			if n >= 60 {
				return "", fmt.Errorf("minute count %d too large; use hours", n)
			}
			return fmt.Sprintf("*/%d * * * *", n), nil
		case strings.HasPrefix(unit, "h"):
			if n >= 24 {
				return "", fmt.Errorf("hour count %d too large; use days", n)
			}
			return fmt.Sprintf("0 */%d * * *", n), nil
		}
	}
	if m := dailyRe.FindStringSubmatch(w); m != nil {
		h, _ := strconv.Atoi(m[1])
		min, _ := strconv.Atoi(m[2])
		if h > 23 || min > 59 {
			return "", fmt.Errorf("invalid time in %q", when)
		}
		return fmt.Sprintf("%d %d * * *", min, h), nil
	}
	return "", fmt.Errorf("don't know how to parse %q as a schedule; pass a cron expression or 'every Nm', 'every Nh', 'daily HH:MM', 'hourly'", when)
}

// scheduleActionable mirrors commitmentActionable for scheduled tasks:
// a missing record and someone else's record give the same answer, so
// deletion cannot be used to probe for what exists.
func scheduleActionable(ctx context.Context, store *memory.Store, id string) (bool, error) {
	if store == nil {
		return true, nil
	}
	raw, err := store.Get(memory.BucketScheduledTasks, id)
	if err != nil {
		if memory.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	var t lobslawv1.ScheduledTaskRecord
	if err := proto.Unmarshal(raw, &t); err != nil {
		return false, fmt.Errorf("unmarshal scheduled task %q: %w", id, err)
	}
	return ownsSchedule(ctx, &t), nil
}
