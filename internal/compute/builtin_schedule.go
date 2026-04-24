package compute

import (
	cryptorand "crypto/rand"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Shared ULID entropy for schedule IDs. Same monotonic pattern as
// memIDEntropy elsewhere.
var scheduleIDEntropy = ulid.Monotonic(cryptorand.Reader, 0)

// ScheduleHandlerRef is the handler_ref value written on every
// task created via schedule_create. Matches the existing
// node.AgentTurnHandlerRef so scheduler-created tasks dispatch
// through the same agent-turn path as operator-defined ones.
const ScheduleHandlerRef = "agent:turn"

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
	return b.Register("schedule_delete", newScheduleDeleteHandler(cfg.Raft))
}

func ScheduleToolDefs() []*types.ToolDef {
	return []*types.ToolDef{
		{
			Name:        "schedule_create",
			Path:        BuiltinScheme + "schedule_create",
			Description: "Create a recurring scheduled task. Use when the user asks for a recurring check (\"check my mail every 5 minutes\", \"every morning at 8am tell me the weather\"). Pass name (human-readable), when (cron OR natural language: \"every 5m\", \"every 1h\", \"every 30s\", \"daily 08:00\"), and prompt (the self-instruction the agent executes each tick). Optional notify_on: \"always\" (ping on every tick), \"match\" (let the agent decide per tick, default), \"never\" (silent; memory-only). Returns {id} for the caller to reference. The task runs via agent.self_prompt; each tick fires your prompt through your own agent loop with full tool access.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"name": {"type": "string", "description": "Human-readable name."},
					"when": {"type": "string", "description": "Cron expression OR natural language: 'every 5m', 'every 1h', 'daily 08:00', 'hourly'."},
					"prompt": {"type": "string", "description": "Self-instruction fired each tick."},
					"notify_on": {"type": "string", "enum": ["always", "match", "never"], "description": "Notification policy. Default: match."}
				},
				"required": ["name", "when", "prompt"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name:        "schedule_list",
			Path:        BuiltinScheme + "schedule_list",
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
			Path:        BuiltinScheme + "schedule_get",
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
			Path:        BuiltinScheme + "schedule_delete",
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

func newScheduleCreateHandler(raft memoryRaftApplier) BuiltinFunc {
	return func(_ context.Context, args map[string]string) ([]byte, int, error) {
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
			notifyOn = "match"
		}
		if notifyOn != "always" && notifyOn != "match" && notifyOn != "never" {
			return nil, 2, fmt.Errorf("schedule_create: notify_on must be always|match|never, got %q", notifyOn)
		}

		cron, err := normaliseToCron(when)
		if err != nil {
			return nil, 2, fmt.Errorf("schedule_create: %w", err)
		}

		id := ulid.MustNew(ulid.Now(), scheduleIDEntropy).String()
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
		if _, err := raft.Apply(data, 5*time.Second); err != nil {
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

func newScheduleListHandler(store *memory.Store) BuiltinFunc {
	return func(_ context.Context, _ map[string]string) ([]byte, int, error) {
		type view struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Schedule string `json:"schedule"`
			Enabled  bool   `json:"enabled"`
			NextRun  string `json:"next_run,omitempty"`
			LastRun  string `json:"last_run,omitempty"`
			Prompt   string `json:"prompt,omitempty"`
		}
		var tasks []view
		err := store.ForEach(memory.BucketScheduledTasks, func(_ string, raw []byte) error {
			var t lobslawv1.ScheduledTaskRecord
			if err := proto.Unmarshal(raw, &t); err != nil {
				return nil
			}
			v := view{
				ID:       t.Id,
				Name:     t.Name,
				Schedule: t.Schedule,
				Enabled:  t.Enabled,
				Prompt:   t.Params["prompt"],
			}
			if t.NextRun != nil {
				v.NextRun = t.NextRun.AsTime().Format(time.RFC3339)
			}
			if t.LastRun != nil {
				v.LastRun = t.LastRun.AsTime().Format(time.RFC3339)
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

func newScheduleGetHandler(store *memory.Store) BuiltinFunc {
	return func(_ context.Context, args map[string]string) ([]byte, int, error) {
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

func newScheduleDeleteHandler(raft memoryRaftApplier) BuiltinFunc {
	return func(_ context.Context, args map[string]string) ([]byte, int, error) {
		id := strings.TrimSpace(args["id"])
		if id == "" {
			return nil, 2, errors.New("schedule_delete: id is required")
		}
		// Payload shape disambiguates the bucket for the FSM's
		// applyDelete — same pattern memory.Service uses for its
		// ForgetRequest handling.
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
		if _, err := raft.Apply(data, 5*time.Second); err != nil {
			return nil, 1, fmt.Errorf("schedule_delete: raft apply: %w", err)
		}
		out, _ := json.Marshal(map[string]any{"id": id, "deleted": true})
		return out, 0, nil
	}
}

// normaliseToCron accepts either a cron expression (5 or 6 fields)
// or a natural-language phrase. Natural forms supported:
//
//	"every 30s"  → cron can't express sub-minute — rejected
//	"every 5m"   → "*/5 * * * *"
//	"every 1h"   → "0 */1 * * *"
//	"hourly"     → "0 * * * *"
//	"daily HH:MM"→ "M H * * *"
//	"every day HH:MM" → same
//
// Unknown forms pass through to cron parsing (which will reject
// bad input at scheduler-level).
var everyRe = regexp.MustCompile(`^every\s+(\d+)\s*(s|sec|seconds?|m|min|minutes?|h|hr|hours?)$`)
var dailyRe = regexp.MustCompile(`^(?:daily|every\s+day)\s+(\d{1,2}):(\d{2})$`)

func normaliseToCron(when string) (string, error) {
	w := strings.TrimSpace(strings.ToLower(when))
	// Already looks like a cron expression (5+ fields separated by whitespace)?
	if fields := strings.Fields(w); len(fields) >= 5 {
		return when, nil // trust operator input
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
