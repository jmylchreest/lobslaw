package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// WatchHandlerRef is the HandlerRef a watch commitment dispatches
// through. Its handler lives in internal/node beside the agent-turn
// one; the constant is here so the tool that creates the record and
// the wiring that runs it cannot disagree about the name.
const WatchHandlerRef = "agent:watch"

// Defaults for a watch the model created without saying otherwise.
//
// The expiry is the load-bearing one. A watch with no end runs until
// somebody remembers to cancel it, and the thing about a watch that is
// working correctly is that it says nothing — so the failure mode of
// an unbounded default is a fleet of forgotten probes billing a
// provider for questions nobody is still asking.
const (
	DefaultWatchInterval    = time.Hour
	DefaultWatchMaxInterval = 24 * time.Hour
	DefaultWatchExpiry      = 30 * 24 * time.Hour
	DefaultWatchMaxFailures = 5
)

// MaxWatchStateChars bounds one reported state.
//
// A state is "the shortest canonical form of the fact", so a long one
// is evidence the model reported prose instead — and prose is exactly
// what the digest cannot compare. The bound is also the reason the
// prompt cannot grow without limit: every state is replayed into the
// next probe, so an unbounded one compounds on every check, forever.
const MaxWatchStateChars = 512

// WatchConfig wires the watch_* builtins. Same Store + Raft pair as
// the commitment and schedule families, because a watch is a
// commitment.
type WatchConfig struct {
	Store *memory.Store
	Raft  memoryRaftApplier
}

// RegisterWatchBuiltins installs watch_create / list / cancel, and
// watch_report.
func RegisterWatchBuiltins(b *Builtins, cfg WatchConfig) error {
	if cfg.Store == nil || cfg.Raft == nil {
		return errors.New("watch builtins: Store + Raft required")
	}
	if err := b.Register("watch_create", newWatchCreateHandler(cfg.Raft)); err != nil {
		return err
	}
	if err := b.Register("watch_list", newWatchListHandler(cfg.Store)); err != nil {
		return err
	}
	if err := b.Register("watch_cancel", newWatchCancelHandler(cfg.Store, cfg.Raft)); err != nil {
		return err
	}
	return b.Register("watch_report", newWatchReportHandler())
}

func WatchToolDefs() []*types.ToolDef {
	return []*types.ToolDef{
		{
			Name:        "watch_create",
			Path:        compute.BuiltinScheme + "watch_create",
			Description: "Watch something and speak only when it CHANGES. Use for 'tell me when', 'let me know if', 'keep an eye on' — a fare that might drop, a page that might update, a service that might come back. The check runs on its own schedule and stays silent while the answer is the same, so it costs the user nothing until there is something to say. Pass what (the thing to check, phrased as an instruction: 'check the price of BA117 on 3 March'). Optional interval (how often at first, default 1h — it widens automatically while nothing changes), max_interval (the widest it will go, default 24h) and expires (when to give up, default 30d). For a one-off 'remind me at 9am' use commitment_create; for a job that must run on a fixed cadence whether or not anything changed, use schedule_create.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"what":         {"type": "string", "description": "What to check, as an instruction the agent runs each time."},
					"interval":     {"type": "string", "description": "Starting gap between checks ('15m', '1h', '6h'). Default 1h."},
					"max_interval": {"type": "string", "description": "Widest the gap may become while nothing changes. Default 24h."},
					"expires":      {"type": "string", "description": "Duration ('7d' is not valid — use '168h') or RFC3339 timestamp. Default 30 days."}
				},
				"required": ["what"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name:        "watch_list",
			Path:        compute.BuiltinScheme + "watch_list",
			Description: "List active watches with what each is watching, what it last saw, when it last changed, and when it next checks. Use when the user asks what you are keeping an eye on, or whether a watch is still running — the answer comes from the record rather than from memory of having set one up.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"include_finished": {"type": "boolean", "description": "Include watches that expired or suspended themselves. Default false. Cancelled watches are deleted and cannot be listed."}
				},
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name:        "watch_cancel",
			Path:        compute.BuiltinScheme + "watch_cancel",
			Description: "Stop a watch by id. Use when the user has lost interest ('stop watching that fare'). Get the id from watch_list.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"id": {"type": "string", "description": "Watch id, from watch_list."}
				},
				"required": ["id"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			// Unlisted: meaningless outside a watch check, where the
			// handler puts it in the turn's tool list explicitly.
			Name:        "watch_report",
			Path:        compute.BuiltinScheme + "watch_report",
			Unlisted:    true,
			Description: "Report what this watch check found. Call this exactly once, at the end of the check, ALWAYS — including when nothing has changed. Pass state as the shortest canonical form of the fact you are watching, in the SAME shape you were shown as the previous state: 'price: 212.00 GBP', 'status: out of stock', 'latest post: 2026-03-04'. No prose, no timestamps of when you looked, no hedging — state is compared character by character against last time, so any rewording of an unchanged fact will be read as a change. Pass summary as one sentence for the user, used only when the state actually changed.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"state":   {"type": "string", "description": "Canonical fact, matching the shape of the previous state exactly."},
					"summary": {"type": "string", "description": "One sentence for the user, used only if the state changed."}
				},
				"required": ["state"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
	}
}

func newWatchCreateHandler(raft memoryRaftApplier) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		what := strings.TrimSpace(args["what"])
		if what == "" {
			return nil, 2, errors.New("watch_create: what is required")
		}
		interval, err := parseWatchDuration(args["interval"], DefaultWatchInterval)
		if err != nil {
			return nil, 2, fmt.Errorf("watch_create: interval: %w", err)
		}
		maxInterval, err := parseWatchDuration(args["max_interval"], DefaultWatchMaxInterval)
		if err != nil {
			return nil, 2, fmt.Errorf("watch_create: max_interval: %w", err)
		}
		if maxInterval < interval {
			// Not an error worth refusing over: a max below the base
			// just means "do not back off", which is a coherent thing
			// to want.
			maxInterval = interval
		}
		userTZ := identityTimezone(ctx)
		expires := time.Now().Add(DefaultWatchExpiry)
		if raw := strings.TrimSpace(args["expires"]); raw != "" {
			expires, err = parseWhen(raw, userTZ)
			if err != nil {
				return nil, 2, fmt.Errorf("watch_create: expires: %w", err)
			}
			if !expires.After(time.Now()) {
				return nil, 2, fmt.Errorf("watch_create: expires %s is in the past", formatTimeForUser(ctx, expires))
			}
		}

		identity, _ := turn.IdentityFrom(ctx)
		params := map[string]string{"what": what}
		if identity.Channel != "" {
			params["channel"] = identity.Channel
		}
		if identity.ChannelID != "" {
			params["chat_id"] = identity.ChannelID
		}
		if identity.UserID != "" {
			params["user_id"] = identity.UserID
		}

		id := ids.New()
		c := &lobslawv1.AgentCommitment{
			Id: id,
			// Due immediately: the first check establishes the baseline
			// and says nothing, so running it now rather than one
			// interval from now is what makes "watching" true as soon
			// as the user is told it is.
			DueAt:      timestamppb.New(time.Now()),
			Trigger:    "time",
			Reason:     "watch: " + what,
			Status:     "pending",
			HandlerRef: WatchHandlerRef,
			Params:     params,
			CreatedFor: identity.UserID,
			Owner:      identityOwner(ctx),
			Watch: &lobslawv1.WatchState{
				Interval:     durationpb.New(interval),
				BaseInterval: durationpb.New(interval),
				MaxInterval:  durationpb.New(maxInterval),
				ExpiresAt:    timestamppb.New(expires),
				MaxFailures:  DefaultWatchMaxFailures,
			},
		}
		entry := &lobslawv1.LogEntry{
			Op:      lobslawv1.LogOp_LOG_OP_PUT,
			Id:      id,
			Payload: &lobslawv1.LogEntry_Commitment{Commitment: c},
		}
		data, err := proto.Marshal(entry)
		if err != nil {
			return nil, 1, fmt.Errorf("watch_create: marshal: %w", err)
		}
		if _, err := raft.Apply(data, raftApplyTimeout); err != nil {
			return nil, 1, fmt.Errorf("watch_create: raft apply: %w", err)
		}
		out, _ := json.Marshal(map[string]any{
			"id":         id,
			"what":       what,
			"interval":   interval.String(),
			"expires_at": formatTimeForUser(ctx, expires),
			"note":       "First check runs now and establishes the baseline silently. You will only hear from this watch when the answer changes.",
		})
		return out, 0, nil
	}
}

func newWatchListHandler(store *memory.Store) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		includeFinished := false
		if raw := strings.TrimSpace(args["include_finished"]); raw != "" {
			b, err := strconv.ParseBool(raw)
			if err != nil {
				return nil, 2, fmt.Errorf("watch_list: include_finished must be a boolean: %w", err)
			}
			includeFinished = b
		}
		type view struct {
			ID          string `json:"id"`
			What        string `json:"what"`
			Status      string `json:"status"`
			LastSeen    string `json:"last_seen,omitempty"`
			LastChecked string `json:"last_checked,omitempty"`
			LastChanged string `json:"last_changed,omitempty"`
			NextCheck   string `json:"next_check,omitempty"`
			Interval    string `json:"interval,omitempty"`
			ExpiresAt   string `json:"expires_at,omitempty"`
			Suspended   string `json:"suspended_reason,omitempty"`
		}
		var out []view
		hidden := 0
		err := store.ForEach(memory.BucketCommitments, func(_ string, raw []byte) error {
			var c lobslawv1.AgentCommitment
			if err := proto.Unmarshal(raw, &c); err != nil {
				return nil
			}
			// A commitment without watch state is a reminder, and
			// belongs to commitment_list rather than here.
			if c.Watch == nil {
				return nil
			}
			if !ownedByCaller(ctx, c.Owner) {
				return nil
			}
			if !includeFinished && c.Status != "pending" {
				hidden++
				return nil
			}
			v := view{
				ID:        c.Id,
				What:      c.Params["what"],
				Status:    c.Status,
				LastSeen:  c.Watch.Observation,
				Suspended: c.Watch.SuspendedReason,
			}
			if c.Watch.LastChecked != nil {
				v.LastChecked = formatTimeForUser(ctx, c.Watch.LastChecked.AsTime())
			}
			if c.Watch.LastChanged != nil {
				v.LastChanged = formatTimeForUser(ctx, c.Watch.LastChanged.AsTime())
			}
			if c.DueAt != nil && c.Status == "pending" {
				v.NextCheck = formatTimeForUser(ctx, c.DueAt.AsTime())
			}
			if c.Watch.Interval != nil {
				v.Interval = c.Watch.Interval.AsDuration().String()
			}
			if c.Watch.ExpiresAt != nil {
				v.ExpiresAt = formatTimeForUser(ctx, c.Watch.ExpiresAt.AsTime())
			}
			out = append(out, v)
			return nil
		})
		if err != nil {
			return nil, 1, fmt.Errorf("watch_list: %w", err)
		}
		payload, _ := json.Marshal(map[string]any{
			"count":        len(out),
			"hidden_count": hidden,
			"watches":      out,
		})
		return payload, 0, nil
	}
}

func newWatchCancelHandler(store *memory.Store, raft memoryRaftApplier) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		id := strings.TrimSpace(args["id"])
		if id == "" {
			return nil, 2, errors.New("watch_cancel: id is required")
		}
		// Ownership AND watch-ness. commitmentActionable alone would
		// let this delete a plain reminder whose id the model confused
		// for a watch's — a tool doing something other than what its
		// description says, silently and irreversibly.
		//
		// Same refusal shape as commitment_cancel: "not yours", "not a
		// watch" and "not there" are one answer, so ids cannot be
		// probed for existence.
		ok, err := watchActionable(ctx, store, id)
		if err != nil {
			return nil, 1, fmt.Errorf("watch_cancel: %w", err)
		}
		if !ok {
			return nil, 2, errors.New("watch_cancel: no watch with that id")
		}
		entry := &lobslawv1.LogEntry{
			Op:      lobslawv1.LogOp_LOG_OP_DELETE,
			Id:      id,
			Payload: &lobslawv1.LogEntry_Commitment{Commitment: &lobslawv1.AgentCommitment{Id: id}},
		}
		data, err := proto.Marshal(entry)
		if err != nil {
			return nil, 1, fmt.Errorf("watch_cancel: marshal: %w", err)
		}
		if _, err := raft.Apply(data, raftApplyTimeout); err != nil {
			return nil, 1, fmt.Errorf("watch_cancel: raft apply: %w", err)
		}
		out, _ := json.Marshal(map[string]any{"id": id, "cancelled": true})
		return out, 0, nil
	}
}

func newWatchReportHandler() compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		state := strings.TrimSpace(args["state"])
		if state == "" {
			return nil, 2, errors.New("watch_report: state is required")
		}
		if len(state) > MaxWatchStateChars {
			// Exit 2 so the model can shorten and call again inside
			// this same turn, rather than the check counting as
			// silent and pushing the watch toward suspension.
			return nil, 2, fmt.Errorf(
				"watch_report: state is %d characters and the limit is %d — report the bare fact "+
					"(a label and a value), not a description of it",
				len(state), MaxWatchStateChars)
		}
		ok := compute.ReportWatch(ctx, compute.WatchReport{
			State:   state,
			Summary: strings.TrimSpace(args["summary"]),
		})
		if !ok {
			// Registered but unlisted, so reaching this means something
			// put it in a turn's tool list that should not have. Say so
			// plainly rather than accepting a report nothing will read.
			return nil, 2, errors.New("watch_report: this turn is not a watch check, so there is nothing to report to")
		}
		out, _ := json.Marshal(map[string]any{"recorded": true})
		return out, 0, nil
	}
}

// watchActionable loads a commitment and reports whether this turn may
// cancel it as a watch: owned by the caller, and carrying watch state.
// A missing record, someone else's, and a plain reminder all report
// false, so every refusal is the same answer.
func watchActionable(ctx context.Context, store *memory.Store, id string) (bool, error) {
	if store == nil {
		return false, nil
	}
	raw, err := store.Get(memory.BucketCommitments, id)
	if err != nil {
		if memory.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	var c lobslawv1.AgentCommitment
	if err := proto.Unmarshal(raw, &c); err != nil {
		return false, fmt.Errorf("unmarshal commitment %q: %w", id, err)
	}
	if c.Watch == nil {
		return false, nil
	}
	return ownedByCaller(ctx, c.Owner), nil
}

// parseWatchDuration accepts a Go duration and falls back to def when
// empty. Deliberately narrower than parseWhen: an interval is never an
// absolute time, and accepting one would silently produce a watch that
// checks once.
func parseWatchDuration(raw string, def time.Duration) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration (try '30m', '2h'): %w", raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%q must be positive", raw)
	}
	return d, nil
}
