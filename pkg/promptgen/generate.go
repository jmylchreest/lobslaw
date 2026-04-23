package promptgen

import (
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// GenerateInput bundles everything the assembler needs. Each field
// is optional — callers leave zero values to skip the corresponding
// section. Tests, in particular, typically pass only Soul and
// Tools; the remaining fields degrade gracefully.
type GenerateInput struct {
	// Soul shapes BuildIdentity. nil → "default assistant persona".
	Soul *types.SoulConfig

	// Tools + Skills render available-capabilities sections. Empty
	// lists produce "(none configured)" / "(none installed)" bodies.
	Tools  []ToolInfo
	Skills []SkillInfo

	// Now + Timezone drive BuildCurrentTime. Zero Now → time.Now();
	// nil Timezone → UTC.
	Now      time.Time
	Timezone *time.Location

	// Runtime + Workspace render their namesake sections.
	Runtime   RuntimeInfo
	Workspace string

	// Bootstrap loads operator-supplied files. Empty Paths → no
	// bootstrap block (section elided entirely, not a stub).
	Bootstrap BootstrapConfig

	// Context blocks are wrapped via WrapContext and emitted inside
	// the "Context" section below the system prompt body. Empty list
	// → no Context section.
	Context []ContextBlock
}

// Generate assembles the full system prompt. Section order:
//
//  1. Identity (soul)
//  2. Operating Principles (safety)
//  3. Current Time
//  4. Runtime
//  5. Workspace
//  6. Available Tools
//  7. Installed Skills
//  8. Bootstrap (operator files)
//  9. Context (wrapped tool/memory output)
//
// Deterministic: same input → identical output. Stable across runs
// so provider-side prompt caches hit consistently.
func Generate(in GenerateInput) string {
	var b strings.Builder

	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}

	sections := []Section{
		BuildIdentity(in.Soul),
		BuildSafety(),
		BuildCurrentTime(now, in.Timezone),
		BuildRuntime(in.Runtime),
		BuildWorkspace(in.Workspace),
		BuildTooling(in.Tools),
		BuildSkills(in.Skills),
	}
	for _, s := range sections {
		b.WriteString(s.Format())
		b.WriteByte('\n')
	}

	if len(in.Bootstrap.Paths) > 0 {
		bootstrapResult := LoadBootstrap(in.Bootstrap)
		if bootstrapResult.Body != "" {
			b.WriteString("# Bootstrap\n\n")
			b.WriteString(bootstrapResult.Body)
			if !strings.HasSuffix(bootstrapResult.Body, "\n") {
				b.WriteByte('\n')
			}
			b.WriteByte('\n')
		}
	}

	if len(in.Context) > 0 {
		ctxBody := WrapContext(in.Context)
		if ctxBody != "" {
			b.WriteString("# Context\n\n")
			b.WriteString(ctxBody)
			if !strings.HasSuffix(ctxBody, "\n") {
				b.WriteByte('\n')
			}
		}
	}

	return b.String()
}
