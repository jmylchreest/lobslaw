// Package research implements the deep-research workflow: a
// question is decomposed into sub-questions by a planner LLM call,
// each sub-question runs as a self-contained agent loop with web
// search + fetch + memory tools, and a synthesiser LLM call merges
// the worker outputs into a single report. The result is written
// to memory tagged "research:<id>" and the originator is notified.
//
// Async dispatch is via the existing scheduler commitment pipeline:
// `research_start` builtin creates a commitment with HandlerRef
// "compute:research" + Params{question, depth, originator_chat_id};
// the commitment fires (leader-only) into the Coordinator's Run
// method.
package research

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Coordinator orchestrates one research run end-to-end. Constructed
// once per node; Run is called per commitment fire.
type Coordinator struct {
	agent  ResearchAgent
	memory MemoryWriter
	notify Notifier
	tools  []compute.Tool // worker tool list — provided at construction
	log    *slog.Logger
}

// ResearchAgent is the slice of compute.Agent the coordinator uses
// for the planner / worker / synth LLM calls. Matches the live
// Agent.RunToolCallLoop signature (returns a pointer + error).
type ResearchAgent interface {
	RunToolCallLoop(ctx context.Context, req compute.ProcessMessageRequest) (*compute.ProcessMessageResponse, error)
}

// MemoryWriter persists the research findings + final report. The
// real implementation is internal/memory.Service; tests inject a
// fake. Records are tagged "research:<task_id>" so the synthesiser
// can recall workers' findings via memory_search.
type MemoryWriter interface {
	WriteEpisodic(ctx context.Context, content string, tags []string) (id string, err error)
}

// Notifier delivers the final report to the user that started the
// research. For Telegram-originated runs this is notify_telegram;
// for REST it's a webhook callback. Both are existing builtins;
// the coordinator just builds + dispatches the right invocation.
type Notifier interface {
	Notify(ctx context.Context, channel, channelID, body string) error
}

// Config wires the coordinator's dependencies. All fields required.
type Config struct {
	Agent  ResearchAgent
	Memory MemoryWriter
	Notify Notifier
	// WorkerTools is the tool slice each worker turn gets. Should
	// include web_search, fetch_url, memory_search/write, and any
	// MCP tools the operator wants research workers to use (image
	// understanding, transcription, etc.). Planner + synth turns
	// run with empty Tools regardless.
	WorkerTools []compute.Tool
	Logger      *slog.Logger
}

// NewCoordinator constructs a coordinator from injected deps.
func NewCoordinator(cfg Config) *Coordinator {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Coordinator{
		agent:  cfg.Agent,
		memory: cfg.Memory,
		notify: cfg.Notify,
		tools:  cfg.WorkerTools,
		log:    log,
	}
}

// Request is the input to one research run.
type Request struct {
	// TaskID is the originating commitment's id; used to tag memory
	// records so synth can scope its memory_search calls.
	TaskID string

	// Question is the user-supplied research topic. Free-form text.
	Question string

	// Depth caps how much work the planner can authorise. 1 is
	// "single web_search + summary"; 5 is "decompose into 5+
	// sub-questions, each with its own worker turn".
	Depth int

	// Originator{Channel,ChannelID} routes the final notification
	// back to the user. Empty channel skips the notification step
	// (memory record still written; useful for "save this for
	// later" workflows).
	OriginatorChannel string
	OriginatorChatID  string

	// Claims attribute the work for policy + audit. Required.
	Claims *types.Claims
}

// Result is the synthesised output. Saved to memory as a single
// episodic record tagged "research:<task_id>" + "report".
type Result struct {
	TaskID    string
	Question  string
	Report    string
	MemoryID  string
	Subqueries []string
	Duration  time.Duration
}

// Run executes the full research pipeline:
//   - plan: ask the LLM to decompose Question into sub-questions.
//   - workers: per sub-question, spawn an agent loop with web_search +
//     fetch_url + memory tools, time-budgeted; capture the worker's
//     summary as an episodic memory record tagged
//     "research:<task>+sub:<n>".
//   - synth: ask the LLM to consolidate the workers' findings into a
//     coherent report, citing sub-questions.
//   - notify: if OriginatorChannel set, push the report to the user.
//
// Returns the Result so the commitment handler can log it.
func (c *Coordinator) Run(ctx context.Context, req Request) (*Result, error) {
	start := time.Now()
	if req.Question == "" {
		return nil, fmt.Errorf("research: question is required")
	}
	if req.Depth <= 0 {
		req.Depth = 3
	}
	if req.Depth > 10 {
		req.Depth = 10 // safety cap; planner can otherwise produce 50+ subqs
	}

	c.log.Info("research: starting",
		"task_id", req.TaskID,
		"question_len", len(req.Question),
		"depth", req.Depth)

	subqs, err := c.plan(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	c.log.Info("research: plan complete",
		"task_id", req.TaskID,
		"subq_count", len(subqs))

	findings, err := c.runWorkers(ctx, req, subqs)
	if err != nil {
		return nil, fmt.Errorf("workers: %w", err)
	}

	report, err := c.synth(ctx, req, subqs, findings)
	if err != nil {
		return nil, fmt.Errorf("synth: %w", err)
	}

	tags := []string{
		"research:" + req.TaskID,
		"report",
	}
	memID, err := c.memory.WriteEpisodic(ctx, report, tags)
	if err != nil {
		c.log.Warn("research: failed to persist report", "task_id", req.TaskID, "err", err)
	}

	if req.OriginatorChannel != "" && c.notify != nil {
		body := buildNotification(req.Question, report, memID)
		if err := c.notify.Notify(ctx, req.OriginatorChannel, req.OriginatorChatID, body); err != nil {
			c.log.Warn("research: failed to notify originator",
				"task_id", req.TaskID,
				"channel", req.OriginatorChannel,
				"err", err)
		}
	}

	dur := time.Since(start)
	c.log.Info("research: complete",
		"task_id", req.TaskID,
		"duration", dur,
		"subq_count", len(subqs),
		"report_bytes", len(report),
		"memory_id", memID)

	return &Result{
		TaskID:     req.TaskID,
		Question:   req.Question,
		Report:     report,
		MemoryID:   memID,
		Subqueries: subqs,
		Duration:   dur,
	}, nil
}

// plan asks the LLM to decompose the question into Depth sub-
// questions. Output format: a JSON array of strings. The planner
// runs WITHOUT tools — it's a pure transformation, not a research
// turn itself.
func (c *Coordinator) plan(ctx context.Context, req Request) ([]string, error) {
	prompt := fmt.Sprintf(plannerPrompt, req.Depth)
	resp, err := c.agent.RunToolCallLoop(ctx, compute.ProcessMessageRequest{
		Message:      req.Question,
		Claims:       req.Claims,
		TurnID:       req.TaskID + "/plan",
		SystemPrompt: prompt,
		// No tools — pure decomposition.
		Tools:  nil,
		Budget: mustBudget(0),
	})
	if err != nil {
		return nil, err
	}
	out := strings.TrimSpace(resp.Reply)
	// Extract JSON array — model may wrap in prose. Find first '['.
	if i := strings.Index(out, "["); i >= 0 {
		out = out[i:]
	}
	if i := strings.LastIndex(out, "]"); i >= 0 {
		out = out[:i+1]
	}
	var subqs []string
	if err := json.Unmarshal([]byte(out), &subqs); err != nil {
		return nil, fmt.Errorf("planner output not a JSON array: %w (raw: %q)", err, resp.Reply)
	}
	cap := req.Depth
	if len(subqs) > cap {
		subqs = subqs[:cap]
	}
	return subqs, nil
}

// runWorkers fires one agent turn per sub-question, sequentially
// (parallel later). Each worker has the stdlib tools (web_search,
// fetch_url, memory_search/write) and a tight per-worker budget so
// a single bad worker can't eat the whole spend cap. Returns the
// list of worker findings text, in sub-question order.
func (c *Coordinator) runWorkers(ctx context.Context, req Request, subqs []string) ([]string, error) {
	tools := c.tools
	findings := make([]string, len(subqs))
	for i, q := range subqs {
		wctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		resp, err := c.agent.RunToolCallLoop(wctx, compute.ProcessMessageRequest{
			Message:      q,
			Claims:       req.Claims,
			TurnID:       fmt.Sprintf("%s/worker/%d", req.TaskID, i),
			SystemPrompt: workerPrompt,
			Tools:        tools,
			Budget:       mustBudget(8),
		})
		cancel()
		if err != nil {
			c.log.Warn("research: worker failed",
				"task_id", req.TaskID,
				"subq", q,
				"err", err)
			findings[i] = fmt.Sprintf("(worker for %q failed: %v)", q, err)
			continue
		}
		findings[i] = resp.Reply
		// Persist the per-subq finding so the synthesiser (and
		// future memory_search calls) can recall them. Failure here
		// is logged but not fatal — the in-memory `findings` slice
		// still feeds the synth step, so the user gets the answer
		// even if memory write times out.
		if _, werr := c.memory.WriteEpisodic(ctx, resp.Reply, []string{
			"research:" + req.TaskID,
			fmt.Sprintf("sub:%d", i),
			"finding",
		}); werr != nil {
			c.log.Warn("research: episodic write failed",
				"task_id", req.TaskID, "sub", i, "err", werr)
		}
	}
	return findings, nil
}

// synth merges the workers' findings into a coherent report. Like
// plan, runs without tools — the inputs are already in-prompt.
func (c *Coordinator) synth(ctx context.Context, req Request, subqs, findings []string) (string, error) {
	var b strings.Builder
	b.WriteString("Original question: ")
	b.WriteString(req.Question)
	b.WriteString("\n\nSub-questions and findings:\n\n")
	for i, q := range subqs {
		f := "(no finding)"
		if i < len(findings) {
			f = findings[i]
		}
		fmt.Fprintf(&b, "## %d. %s\n\n%s\n\n", i+1, q, f)
	}
	resp, err := c.agent.RunToolCallLoop(ctx, compute.ProcessMessageRequest{
		Message:      b.String(),
		Claims:       req.Claims,
		TurnID:       req.TaskID + "/synth",
		SystemPrompt: synthPrompt,
		Tools:        nil,
		Budget:       mustBudget(0),
	})
	if err != nil {
		return "", err
	}
	return resp.Reply, nil
}

// mustBudget builds a TurnBudget with a tool-call cap. Zero =
// unlimited (used for planner/synth turns that have no tools).
// Wraps the (caps, err) shape so the call site stays one line —
// caps with non-negative values can't fail in practice.
func mustBudget(maxToolCalls int) *compute.TurnBudget {
	b, _ := compute.NewTurnBudget(compute.BudgetCaps{MaxToolCalls: maxToolCalls})
	return b
}

func buildNotification(question, report, memID string) string {
	var b strings.Builder
	b.WriteString("Research complete: ")
	b.WriteString(question)
	b.WriteString("\n\n")
	// Truncate the body for chat — full report is in memory.
	max := 3500
	if len(report) > max {
		b.WriteString(report[:max])
		b.WriteString("\n\n…(truncated; full report in memory")
		if memID != "" {
			fmt.Fprintf(&b, " id=%s", memID)
		}
		b.WriteString(")")
	} else {
		b.WriteString(report)
	}
	return b.String()
}

const plannerPrompt = `You decompose a research question into independent sub-questions.

Output a JSON array of %d short, self-contained questions, each
addressable by a single web search + fetch round. Cover distinct
angles of the user's question — facts, context, contrasting views,
recent developments. No prose, no commentary, no markdown — JUST
the JSON array.`

const workerPrompt = `You are a research worker. Answer the user's
sub-question using the available tools (web_search, fetch_url,
memory_search). Keep your answer tight: a paragraph or two of
findings + 2-4 source URLs. Don't re-state the question. Don't
speculate beyond what your sources support. If a search returns
nothing useful, say so plainly rather than padding.`

const synthPrompt = `You are a research synthesiser. The user's
input contains an original question + sub-questions with their
findings. Produce a single coherent report:

- Lead with a 1-paragraph executive summary.
- Then a structured body with section headers.
- Cite sources inline as [n] referring to URLs in the findings.
- End with a "Sources" section listing every distinct URL referenced.
- If the findings contradict, flag the contradiction explicitly.
- If a sub-question yielded nothing, omit it from the report
  rather than padding with filler.

Output is markdown. Aim for thoroughness over brevity but don't
restate the same fact in multiple sections.`
