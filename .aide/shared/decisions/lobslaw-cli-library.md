---
topic: lobslaw-cli-library
decision: "alecthomas/kong. Struct-tag-driven CLI parser. Stdlib flag in Phase 1; Kong lands in Phase 8 when the subcommand tree (lobslaw plugin install|enable|disable|list|import, lobslaw skill install, lobslaw audit verify, etc.) warrants it. Rejected: Cobra (heavier, imperative builder API less ergonomic for a known-shape subcommand tree); urfave/cli (viable alternative, Kong's type-safety edges it out); pflag alone (no subcommand support)"
decided_by: johnm
date: 2026-04-22
---

# lobslaw-cli-library

**Decision:** alecthomas/kong. Struct-tag-driven CLI parser. Stdlib flag in Phase 1; Kong lands in Phase 8 when the subcommand tree (lobslaw plugin install|enable|disable|list|import, lobslaw skill install, lobslaw audit verify, etc.) warrants it. Rejected: Cobra (heavier, imperative builder API less ergonomic for a known-shape subcommand tree); urfave/cli (viable alternative, Kong's type-safety edges it out); pflag alone (no subcommand support)

## Rationale

Kong's struct-tag model is a natural fit when the subcommand tree is known in advance (which lobslaw's is). Lighter than Cobra, type-safe argument binding, no global state. Aide go-third-party approves Cobra but the decision's stated principle ('prefer narrow libraries over frameworks') points at Kong for our specific case. Supersedes the blueprint default for this project only

