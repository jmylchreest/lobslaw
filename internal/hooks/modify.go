package hooks

import (
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Modifications are string-valued patches, matching the executor's arguments.
// Reject unsupported shapes instead of acknowledging an ineffective rewrite.
func applyModification(event types.HookEvent, payload Payload, resp *Response) (map[string]string, error) {
	switch resp.Decision {
	case "", types.HookApprove:
		if _, ok := resp.HookSpecificOutput["updatedInput"]; ok {
			return nil, errors.New("updatedInput requires decision=modify")
		}
		return nil, nil
	case types.HookModify:
	default:
		return nil, fmt.Errorf("unsupported decision %q (use approve, block or modify)", resp.Decision)
	}
	if event != types.HookPreToolUse {
		return nil, errors.New("decision=modify is only supported for PreToolUse")
	}
	patch, ok := resp.HookSpecificOutput["updatedInput"].(map[string]any)
	if !ok || len(resp.HookSpecificOutput) != 1 {
		return nil, errors.New("modify requires hookSpecificOutput containing only an updatedInput object")
	}
	input, ok := payload["tool_input"].(map[string]string)
	if !ok {
		return nil, errors.New("modify requires string-valued tool_input")
	}
	next := maps.Clone(input)
	if next == nil {
		next = map[string]string{}
	}
	for key, value := range patch {
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("updatedInput[%q] must be a string", key)
		}
		if key == "" || strings.HasPrefix(key, "__") {
			return nil, fmt.Errorf("updatedInput key %q is reserved or empty", key)
		}
		next[key] = text
	}
	return next, nil
}
