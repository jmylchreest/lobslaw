package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// ResearchHandlerRef is the well-known commitment HandlerRef for
// the deep-research workflow. node.go wires the actual handler;
// the builtin just creates a commitment carrying this ref + the
// research params.
const ResearchHandlerRef = "research:run"

// ResearchConfig wires the research_start builtin. Same Raft
// applier pattern as commitment_create. Default ToolCalls budget
// for fire-time is enforced inside the research coordinator, not
// here — research_start itself only writes the commitment record.
type ResearchConfig struct {
	Raft memoryRaftApplier
}

// RegisterResearchBuiltins installs the research_start builtin.
// Default policy is DENY (seeded by node.seedDefaultPolicyRules at
// priority>1) — agents can't kick off research without an explicit
// operator allow rule. Read-only research_status / research_list
// would be safe to default-allow but aren't built yet.
func RegisterResearchBuiltins(b *Builtins, cfg ResearchConfig) error {
	if cfg.Raft == nil {
		return errors.New("research builtin: Raft required")
	}
	return b.Register("research_start", newResearchStartHandler(cfg.Raft))
}

// ResearchToolDefs is the public ToolDef slice for research builtins.
// Tagged RiskCommunicating (writes commitments + spends LLM tokens
// asynchronously) so policy treatment matches commitment_create.
func ResearchToolDefs() []*types.ToolDef {
	return []*types.ToolDef{
		{
			Name:        "research_start",
			Path:        BuiltinScheme + "research_start",
			Description: "Kick off a deep-research run as a background async task. Use when the user asks for thorough investigation that warrants multiple searches + cross-referencing — 'find me everything about X', 'compare these approaches', 'what's the current state of Y'. NOT for quick lookups (use web_search) or single-page reads (use fetch_url). The research runs out-of-band: planner decomposes the question into sub-questions, workers run web_search + fetch_url per sub-question, a synthesiser merges into a report. Result is written to memory (tagged research:<id>) and the user is notified via their originating channel when complete (typically 1-3 minutes). Pass question (the topic) and optional depth (1-10, default 3 — controls sub-question count + total tool budget). Returns the task id for status tracking.\n\nDEFAULT-DENY: this builtin requires an explicit operator allow rule. If you get a policy denial, tell the user 'research is operator-gated; ask them to add an allow rule for research_start' rather than retrying.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"question": {"type": "string", "description": "The research topic or question. Free-form text."},
					"depth": {"type": "integer", "minimum": 1, "maximum": 10, "description": "Number of sub-questions the planner decomposes into. Default 3."}
				},
				"required": ["question"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskCommunicating,
		},
	}
}

func newResearchStartHandler(raft memoryRaftApplier) BuiltinFunc {
	return func(_ context.Context, args map[string]string) ([]byte, int, error) {
		question := strings.TrimSpace(args["question"])
		if question == "" {
			return nil, 2, errors.New("research_start: question is required")
		}
		depth := 3
		if rawDepth := strings.TrimSpace(args["depth"]); rawDepth != "" {
			n, err := strconv.Atoi(rawDepth)
			if err != nil || n < 1 || n > 10 {
				return nil, 2, fmt.Errorf("research_start: depth must be an integer 1-10, got %q", rawDepth)
			}
			depth = n
		}

		channel := strings.TrimSpace(args["__channel"])
		chatID := strings.TrimSpace(args["__chat_id"])

		id := ulid.MustNew(ulid.Now(), commitmentIDEntropy).String()
		params := map[string]string{
			"question": question,
			"depth":    strconv.Itoa(depth),
		}
		if channel != "" {
			params["originator_channel"] = channel
		}
		if chatID != "" {
			params["originator_chat_id"] = chatID
		}

		// Fire ~immediately — research is async-by-runtime not
		// async-by-scheduling. dueAt = now+1s so the scheduler
		// picks it up next tick.
		c := &lobslawv1.AgentCommitment{
			Id:         id,
			DueAt:      timestamppb.New(time.Now().Add(time.Second)),
			Trigger:    "time",
			Reason:     "deep-research run for: " + question,
			Status:     "pending",
			HandlerRef: ResearchHandlerRef,
			Params:     params,
		}
		entry := &lobslawv1.LogEntry{
			Op:      lobslawv1.LogOp_LOG_OP_PUT,
			Id:      id,
			Payload: &lobslawv1.LogEntry_Commitment{Commitment: c},
		}
		data, err := proto.Marshal(entry)
		if err != nil {
			return nil, 1, fmt.Errorf("research_start: marshal: %w", err)
		}
		if _, err := raft.Apply(data, 5*time.Second); err != nil {
			return nil, 1, fmt.Errorf("research_start: raft apply: %w", err)
		}
		out, _ := json.Marshal(map[string]any{
			"id":       id,
			"question": question,
			"depth":    depth,
			"status":   "queued",
		})
		return out, 0, nil
	}
}
