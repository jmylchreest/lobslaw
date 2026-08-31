package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// stubInspector answers every probe with something recognisable, so a
// test can tell which one a handler actually called.
type stubInspector struct{ called []string }

func (s *stubInspector) note(name string) { s.called = append(s.called, name) }

func (s *stubInspector) DebugTools() []string { s.note("tools"); return []string{"read_file", "glob"} }
func (s *stubInspector) DebugPolicyRules() []string {
	s.note("policy")
	return []string{"allow-all"}
}
func (s *stubInspector) DebugStorageMounts() []string {
	s.note("storage")
	return []string{"workspace"}
}
func (s *stubInspector) DebugMemoryStats() map[string]int {
	s.note("memory")
	return map[string]int{"episodic": 7}
}
func (s *stubInspector) DebugSoul() string { s.note("soul"); return "a soul" }
func (s *stubInspector) DebugRaft() map[string]any {
	s.note("raft")
	return map[string]any{"term": 3}
}
func (s *stubInspector) DebugScheduler() []map[string]any {
	s.note("scheduler")
	return []map[string]any{{"id": "t1"}}
}
func (s *stubInspector) DebugProviders() []map[string]any {
	s.note("providers")
	return []map[string]any{{"label": "main"}}
}
func (s *stubInspector) DebugVersion() string { s.note("version"); return "v1" }
func (s *stubInspector) DebugSandbox() map[string]any {
	s.note("sandbox")
	return map[string]any{"landlock": true}
}
func (s *stubInspector) DebugMCP() map[string]any { s.note("mcp"); return map[string]any{"servers": 0} }

// Every debug tool must reach its own probe and return what it found.
//
// They are built through a helper rather than written out, so a
// copy-paste slip wiring two names to one probe would answer the wrong
// question with a straight face — which for an introspection tool is
// the whole failure. Asserting the probe each one called is what
// catches that.
func TestEachDebugToolCallsItsOwnProbe(t *testing.T) {
	t.Parallel()

	for _, c := range []struct{ tool, probe, contains string }{
		{"debug_tools", "tools", "read_file"},
		{"debug_policy", "policy", "allow-all"},
		{"debug_storage", "storage", "workspace"},
		{"debug_memory", "memory", "episodic"},
		{"debug_soul", "soul", "a soul"},
		{"debug_raft", "raft", "term"},
		{"debug_scheduler", "scheduler", "t1"},
		{"debug_providers", "providers", "main"},
		{"debug_version", "version", "v1"},
		{"debug_sandbox", "sandbox", "landlock"},
		{"debug_mcp", "mcp", "servers"},
	} {
		t.Run(c.tool, func(t *testing.T) {
			t.Parallel()
			insp := &stubInspector{}
			b := NewBuiltins()
			if err := RegisterDebugBuiltins(b, insp); err != nil {
				t.Fatalf("register: %v", err)
			}
			fn, ok := b.Get(c.tool)
			if !ok {
				t.Fatalf("%s is not registered", c.tool)
			}
			out, code, err := fn(context.Background(), nil)
			if err != nil || code != 0 {
				t.Fatalf("%s: code=%d err=%v", c.tool, code, err)
			}
			if len(insp.called) != 1 || insp.called[0] != c.probe {
				t.Errorf("%s called %v, want exactly [%s]", c.tool, insp.called, c.probe)
			}
			if !strings.Contains(string(out), c.contains) {
				t.Errorf("%s output does not carry the probe's answer: %s", c.tool, out)
			}
		})
	}
}

// The output is read by a model and rendered to an operator, so it has
// to be parseable rather than a Go value printed with %v.
func TestDebugOutputIsJSON(t *testing.T) {
	t.Parallel()

	b := NewBuiltins()
	if err := RegisterDebugBuiltins(b, &stubInspector{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, name := range []string{"debug_tools", "debug_memory", "debug_raft"} {
		fn, _ := b.Get(name)
		out, _, _ := fn(context.Background(), nil)
		var any any
		if err := json.Unmarshal(out, &any); err != nil {
			t.Errorf("%s did not return JSON: %v (%s)", name, err, out)
		}
	}
}

// A node with nothing to introspect registers nothing, rather than
// registering handlers that would answer with a nil dereference.
//
// Silent no-op rather than an error, because a node without the
// subsystems these probe is a legitimate deployment and not a
// misconfiguration to report.
func TestNoInspectorRegistersNoDebugTools(t *testing.T) {
	t.Parallel()

	b := NewBuiltins()
	if err := RegisterDebugBuiltins(b, nil); err != nil {
		t.Fatalf("a node with no inspector should register nothing, not fail: %v", err)
	}
	for _, name := range []string{"debug_tools", "debug_memory", "debug_raft"} {
		if _, ok := b.Get(name); ok {
			t.Errorf("%s registered with no inspector behind it", name)
		}
	}
}
