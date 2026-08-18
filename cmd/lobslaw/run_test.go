package main

import "testing"

// Booting a node used to be what happened when nothing matched. That
// is how `lobslaw nodeid` — documented, dispatched by nothing — booted
// a second assistant on somebody's machine.

func TestABareInvocationListsCommandsInsteadOfBooting(t *testing.T) {
	t.Parallel()
	if _, wantsNode := dispatchRun(nil); wantsNode {
		t.Error("a bare `lobslaw` would start a node")
	}
	if _, wantsNode := dispatchRun([]string{}); wantsNode {
		t.Error("an empty argument list would start a node")
	}
}

func TestRunAndServeBothStartANode(t *testing.T) {
	t.Parallel()
	for _, verb := range []string{"run", "serve"} {
		args, wantsNode := dispatchRun([]string{verb, "--config", "x.toml"})
		if !wantsNode {
			t.Errorf("%q did not start a node", verb)
		}
		// The verb is consumed, so the node path sees exactly what it
		// saw when there was no verb at all.
		for _, a := range args {
			if a == verb {
				t.Errorf("%q was left in the args: %v", verb, args)
			}
		}
		if len(args) != 2 || args[0] != "--config" {
			t.Errorf("remaining args = %v, want the flags untouched", args)
		}
	}
}

// `lobslaw --config x` is in enough unit files and scripts that
// breaking it would cost more than the ambiguity it buys.
func TestFlagsWithoutAVerbStillBoot(t *testing.T) {
	t.Parallel()
	args, wantsNode := dispatchRun([]string{"--config", "x.toml"})
	if !wantsNode {
		t.Error("flags-only invocation stopped starting a node")
	}
	if len(args) != 2 {
		t.Errorf("args = %v, want them untouched", args)
	}
}

// A positional that is not a run verb is somebody else's — or a typo,
// which must reach the usage message rather than the node.
func TestAnUnknownPositionalDoesNotBoot(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"nodid", "clustr", "hepl"} {
		if _, wantsNode := dispatchRun([]string{arg}); wantsNode {
			t.Errorf("the typo %q would start a node", arg)
		}
	}
}

// The listing and the dispatch table must describe the same set. A
// command with no description reads as less real than its neighbours;
// a description outliving its command is a lie in the help output.
func TestEveryCommandIsDescribedAndEveryDescriptionHasACommand(t *testing.T) {
	t.Parallel()
	dispatched := map[string]bool{}
	for _, d := range topLevelDispatchers() {
		dispatched[d.name] = true
		if _, ok := commandSummaries[d.name]; !ok {
			t.Errorf("%q dispatches but has no description in the command list", d.name)
		}
	}
	for name := range commandSummaries {
		if !dispatched[name] {
			t.Errorf("%q is described in the command list but dispatches to nothing", name)
		}
	}
}

// The run verbs must not collide with a real command, or typing it
// would start a node instead of running the command.
func TestNoRunVerbShadowsACommand(t *testing.T) {
	t.Parallel()
	for _, d := range topLevelDispatchers() {
		for _, verb := range runNames {
			if d.name == verb {
				t.Errorf("%q is both a command and a run verb", verb)
			}
		}
	}
}
