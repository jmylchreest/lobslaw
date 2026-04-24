package compute

import (
	"sync"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// ProviderEntry is one registered provider with its safe metadata.
// Endpoint URL + API key are deliberately NOT exposed — they're
// implementation detail the LLM has no business seeing via
// list_providers.
type ProviderEntry struct {
	Label        string
	TrustTier    types.TrustTier
	Capabilities []string
	Backup       string // label of backup provider; empty = end of chain
	Client       LLMProvider
}

// ProviderRegistry is the runtime label → provider lookup. Shared
// across Agent (for backup-chain walks) and the list_providers /
// council_review builtins. Thread-safe for concurrent reads.
type ProviderRegistry struct {
	mu      sync.RWMutex
	entries map[string]ProviderEntry
}

// NewProviderRegistry returns an empty registry. Callers populate
// it via Register during node boot.
func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{entries: make(map[string]ProviderEntry)}
}

// Register inserts a provider. Panics on empty label (boot-time
// wiring bug — no recovery path).
func (r *ProviderRegistry) Register(e ProviderEntry) {
	if e.Label == "" {
		panic("ProviderRegistry.Register: empty label")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[e.Label] = e
}

// Get returns the entry for a label + true, or a zero entry + false.
func (r *ProviderRegistry) Get(label string) (ProviderEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[label]
	return e, ok
}

// List returns a snapshot of all entries in stable (alphabetical-by-
// label) order. Safe to iterate without holding a lock.
func (r *ProviderRegistry) List() []ProviderEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ProviderEntry, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e)
	}
	// sort by label for determinism
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].Label > out[j].Label; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// Chain returns the provider chain starting at startLabel, walking
// .Backup pointers. Cycles are impossible here (loader rejects them
// via validateProviderBackups) but we still bound iterations
// defensively in case this is called before validation.
func (r *ProviderRegistry) Chain(startLabel string) []ProviderEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var chain []ProviderEntry
	seen := map[string]bool{}
	cur := startLabel
	for step := 0; step < len(r.entries); step++ {
		if seen[cur] {
			break
		}
		seen[cur] = true
		e, ok := r.entries[cur]
		if !ok {
			break
		}
		chain = append(chain, e)
		if e.Backup == "" {
			break
		}
		cur = e.Backup
	}
	return chain
}
