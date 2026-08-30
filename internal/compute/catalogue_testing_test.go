package compute

import (
	"errors"
	"maps"
	"slices"
	"sync"

	"github.com/jmylchreest/lobslaw/internal/sandbox"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// testCatalogue is a minimal ToolCatalogue for the executor and agent
// tests. The real implementation is tools.Registry, which imports this
// package — so an in-package test cannot reach for it without a cycle.
// That constraint is the point of the interface: the executor's
// contract is three methods, and a test should only have to satisfy
// three methods.
type testCatalogue struct {
	mu     sync.RWMutex
	defs   map[string]*types.ToolDef
	policy map[string]*sandbox.Policy
}

func newTestCatalogue() *testCatalogue {
	return &testCatalogue{defs: map[string]*types.ToolDef{}, policy: map[string]*sandbox.Policy{}}
}

func (c *testCatalogue) Register(t *types.ToolDef) error {
	if t == nil || t.Name == "" {
		return errors.New("tool: name required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, dup := c.defs[t.Name]; dup {
		return errors.New("tool: already registered: " + t.Name)
	}
	c.defs[t.Name] = t
	return nil
}

func (c *testCatalogue) Replace(t *types.ToolDef) error {
	if t == nil || t.Name == "" {
		return errors.New("tool: name required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.defs[t.Name] = t
	return nil
}

func (c *testCatalogue) Get(name string) (*types.ToolDef, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.defs[name]
	return t, ok
}

func (c *testCatalogue) SetPolicy(name string, p *sandbox.Policy) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.policy[name] = p
}

func (c *testCatalogue) PolicyFor(name string) *sandbox.Policy {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.policy[name]
}

func (c *testCatalogue) LLMTools() []Tool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Tool, 0, len(c.defs))
	for _, name := range slices.Sorted(maps.Keys(c.defs)) {
		d := c.defs[name]
		schema := d.ParametersSchema
		if len(schema) == 0 {
			schema = []byte(`{"type":"object","properties":{}}`)
		}
		out = append(out, Tool{Name: d.Name, Description: d.Description, Parameters: schema})
	}
	return out
}

// testDispatcher is a minimal BuiltinDispatcher for the agent tests.
type testDispatcher struct{ fns map[string]BuiltinFunc }

func newTestDispatcher() *testDispatcher { return &testDispatcher{fns: map[string]BuiltinFunc{}} }

func (d *testDispatcher) Register(name string, fn BuiltinFunc) error {
	if name == "" || fn == nil {
		return errors.New("builtin: name and fn required")
	}
	if _, dup := d.fns[name]; dup {
		return errors.New("builtin: already registered: " + name)
	}
	d.fns[name] = fn
	return nil
}

func (d *testDispatcher) Get(name string) (BuiltinFunc, bool) {
	fn, ok := d.fns[name]
	return fn, ok
}
