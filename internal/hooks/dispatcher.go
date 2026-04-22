package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// defaultTimeout caps a single hook subprocess. Hooks that need
// longer should set their own timeout_seconds in config.
const defaultTimeout = 5 * time.Second

// Payload is the JSON body sent to each hook over stdin. The
// hook_event_name and event-specific fields (tool_name, tool_input,
// etc.) come in here; hooks return a Response on stdout.
//
// Shape matches Claude Code's hook JSON so hooks (including RTK)
// written for Claude Code work unchanged under lobslaw.
type Payload map[string]any

// Response is parsed from each hook's stdout. All fields are
// optional — an empty Response is equivalent to "proceed as normal".
type Response struct {
	Decision           types.HookDecision `json:"decision,omitempty"`
	Reason             string             `json:"reason,omitempty"`
	HookSpecificOutput map[string]any     `json:"hookSpecificOutput,omitempty"`
}

// Dispatcher fires subprocess hooks for each registered event.
// Concurrent-safe; reads the hook map without mutation.
type Dispatcher struct {
	hooks  map[types.HookEvent][]types.HookConfig
	logger *slog.Logger
}

// NewDispatcher constructs a dispatcher. The hooks map is stored
// by reference — callers should not mutate it after construction.
// For hot-reload, rebuild the map and construct a new Dispatcher.
func NewDispatcher(hooks map[types.HookEvent][]types.HookConfig, logger *slog.Logger) *Dispatcher {
	if hooks == nil {
		hooks = make(map[types.HookEvent][]types.HookConfig)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{hooks: hooks, logger: logger}
}

// Dispatch runs every hook registered for event whose Match predicate
// applies to payload. Hooks run sequentially in config order.
//
// The chain aborts on the first hook that blocks (explicit
// decision="block" or non-zero exit). Otherwise the returned Response
// is the LAST non-nil hook response; nil when no hook fired or all
// returned empty responses.
//
// Returns ErrHookBlocked (from pkg/types) wrapping the hook's reason
// when any hook blocks.
func (d *Dispatcher) Dispatch(ctx context.Context, event types.HookEvent, payload Payload) (*Response, error) {
	hooks := d.matchingHooks(event, payload)
	if len(hooks) == 0 {
		return nil, nil
	}

	if payload == nil {
		payload = Payload{}
	}
	payload["hook_event_name"] = string(event)

	var last *Response
	for i, cfg := range hooks {
		resp, err := d.runHook(ctx, cfg, payload)
		if err != nil {
			d.logger.Error("hook error",
				"event", event, "command", cfg.Command, "index", i, "err", err)
			return nil, err
		}
		if resp != nil && resp.Decision == types.HookBlock {
			d.logger.Info("hook blocked",
				"event", event, "command", cfg.Command, "reason", resp.Reason)
			return resp, fmt.Errorf("%w: %s", types.ErrHookBlocked, resp.Reason)
		}
		if resp != nil {
			last = resp
		}
	}
	return last, nil
}

// matchingHooks returns hooks registered for event whose Match
// predicate is satisfied by payload. An empty Match map means the
// hook always fires for that event.
func (d *Dispatcher) matchingHooks(event types.HookEvent, payload Payload) []types.HookConfig {
	configs := d.hooks[event]
	if len(configs) == 0 {
		return nil
	}
	out := make([]types.HookConfig, 0, len(configs))
	for _, cfg := range configs {
		if matches(cfg.Match, payload) {
			out = append(out, cfg)
		}
	}
	return out
}

// matches returns true when every key in predicate has an equal
// value in payload. An empty predicate matches any payload.
func matches(predicate map[string]string, payload Payload) bool {
	for k, want := range predicate {
		got, ok := payload[k]
		if !ok {
			return false
		}
		gotStr, ok := got.(string)
		if !ok {
			return false
		}
		if gotStr != want {
			return false
		}
	}
	return true
}

// runHook spawns one hook subprocess with the JSON payload piped on
// stdin, reads JSON from stdout, and parses it into a Response.
//
// Exit code semantics (matches Claude Code):
//
//	0   — proceed; stdout is the Response JSON (may be empty)
//	2   — block; stderr is the reason
//	non-zero other — error; stderr is the reason
func (d *Dispatcher) runHook(ctx context.Context, cfg types.HookConfig, payload Payload) (*Response, error) {
	timeout := defaultTimeout
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	input, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal hook payload: %w", err)
	}

	cmd := exec.CommandContext(runCtx, cfg.Command, cfg.Args...)
	cmd.Stdin = bytes.NewReader(input)
	// WaitDelay closes stdout/stderr pipes shortly after the process
	// is killed by ctx cancel. Without this, Wait blocks for the
	// lifetime of any grandchild process that inherited our pipes
	// (e.g. a sleep child of a shell hook).
	cmd.WaitDelay = 500 * time.Millisecond
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()

	if runCtx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%w after %s", types.ErrHookTimeout, timeout)
	}

	if err != nil {
		exitCode := -1
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			exitCode = exitErr.ExitCode()
		}
		if exitCode == 2 {
			return &Response{
				Decision: types.HookBlock,
				Reason:   stderr.String(),
			}, nil
		}
		return nil, fmt.Errorf("hook %q exit=%d: %s", cfg.Command, exitCode, stderr.String())
	}

	if stdout.Len() == 0 {
		return nil, nil
	}
	var resp Response
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("hook %q response parse: %w (stdout=%q)", cfg.Command, err, stdout.String())
	}
	return &resp, nil
}
