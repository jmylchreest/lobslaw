// Package tools holds every tool the agent can call, the registry
// that advertises them to the model, and the guard chain that decides
// which paths a tool may touch.
//
// The filename is the index. A file named for a tool holds that tool —
// the ToolDef the model reads, the handler that runs, and the config
// that wires it. A file named lib_* is machinery the tools share:
//
//   - lib_registry.go — the catalogue, the disabled-tool globs, and
//     the recommend/avoid cross-references
//   - lib_builtins.go — the in-process handler dispatcher
//   - lib_guard.go, lib_mounts.go, lib_landlock.go — the
//     mount → hardline → policy.d chain every path-taking tool runs
//
// The split is worth knowing before reading a diff: a change under
// lib_ moves the floor for all thirty tools, where a change to
// pdf.go moves one. TestMachineryFilesDeclareNoTools keeps the rule
// honest.
//
// This package sits above [compute]. A tool reaches for that
// package's drivers, providers and trust floor; compute never reaches
// back, consuming this package only through the ToolCatalogue and
// BuiltinDispatcher interfaces it declares itself. That is what keeps
// the dependency one-directional and the executor testable without
// the whole tool catalogue.
package tools
