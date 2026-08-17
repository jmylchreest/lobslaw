package main

import (
	"strings"
	"testing"
)

// R28's rule, as a test rather than a claim.
//
// "Every command either reaches the cluster or refuses; none reads a
// local state.db that is not the one the operator meant."
//
// The failure mode is not a command that errors. It is a laptop-local
// state.db being read as though it were the cluster's, and answering
// confidently with nothing in it. An operator listing sessions would
// see an empty list and have no reason to doubt it.
//
// So this is an INVENTORY, and it fails when a new subcommand is added
// without deciding which side of the rule it falls on. That is the
// point: the check has to be something a future change trips over
// rather than something somebody remembers.

// commandGroup is one dispatcher's declared surface.
type commandGroup struct {
	name  string
	usage string
	// forms have both a live and an offline implementation.
	forms map[string]struct{ live, offline func([]string) error }
	// offlineOnly run against a local store and MUST announce it.
	offlineOnly map[string]func([]string) error
	// liveOnly have no offline form at all.
	liveOnly map[string]func([]string) error
}

// everyGroup is the inventory. A dispatcher missing from here is
// invisible to every check below, so adding one is the deliberate act.
func everyGroup() []commandGroup {
	return []commandGroup{
		{name: "audit", usage: auditUsage, forms: auditForms},
		{name: "policy", usage: policyUsage, forms: policyForms},
		{name: "memory", usage: memoryUsage, forms: memoryForms, offlineOnly: memoryOfflineOnly},
		{name: "session", usage: sessionUsage, forms: sessionForms},
		{name: "identity", usage: identityUsage, forms: identityForms},
		{name: "trace", usage: traceUsage, forms: traceForms},
		{
			name: "learned", usage: learnedUsage, forms: learnedForms,
			offlineOnly: learnedOfflineOnly, liveOnly: learnedLiveOnly,
		},
	}
}

// THE RULE. Anything with both forms goes LIVE without --offline.
func TestEveryCommandReachesTheClusterByDefault(t *testing.T) {
	for _, g := range everyGroup() {
		for sub, form := range g.forms {
			if form.live == nil {
				t.Errorf("%s %s: declared as having a live form but it is nil", g.name, sub)
				continue
			}
			if routesTo(t, form.live, form.offline) {
				t.Errorf("%s %s: live and offline are the same function, so one of them is a lie",
					g.name, sub)
			}
		}
	}
}

// Every subcommand appears in its group's usage. A command nobody can
// find is a command nobody uses correctly.
func TestEverySubcommandIsDiscoverable(t *testing.T) {
	for _, g := range everyGroup() {
		for _, set := range []map[string]func([]string) error{g.offlineOnly, g.liveOnly} {
			for sub := range set {
				if !strings.Contains(g.usage, sub) {
					t.Errorf("%s: usage does not mention %q", g.name, sub)
				}
			}
		}
		for sub := range g.forms {
			// trace's turn-id form has no typed name; "show" is the
			// internal label for it.
			if g.name == "trace" && sub == "show" {
				continue
			}
			if !strings.Contains(g.usage, sub) {
				t.Errorf("%s: usage does not mention %q", g.name, sub)
			}
		}
	}
}

// A group with any offline path must say --offline exists, or the
// forensic route for a cluster that will not start is undiscoverable.
func TestEveryGroupWithAnOfflinePathAdvertisesTheFlag(t *testing.T) {
	for _, g := range everyGroup() {
		hasOffline := len(g.offlineOnly) > 0
		for _, form := range g.forms {
			if form.offline != nil {
				hasOffline = true
			}
		}
		if hasOffline && !strings.Contains(g.usage, "--offline") {
			t.Errorf("%s: has an offline path but the usage never mentions --offline", g.name)
		}
	}
}

// An offline-only subcommand must be MARKED in the usage, so somebody
// reading it knows before they run it that the answer is about this
// machine.
//
// The announcement at run time is the other half; this is the half a
// person sees while deciding what to type.
func TestEveryOfflineOnlySubcommandIsMarkedInTheUsage(t *testing.T) {
	for _, g := range everyGroup() {
		for sub := range g.offlineOnly {
			line := usageLineFor(g.usage, sub)
			if line == "" {
				t.Errorf("%s: %q is offline-only but has no usage line", g.name, sub)
				continue
			}
			if !strings.Contains(line, "offline") {
				t.Errorf("%s: %q is offline-only but its usage line does not say so:\n  %s",
					g.name, sub, line)
			}
		}
	}
}

// usageLineFor returns the usage line describing a subcommand — the
// indented one that starts with its name, not any prose mentioning it.
func usageLineFor(usage, sub string) string {
	for _, line := range strings.Split(usage, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || !strings.HasPrefix(line, " ") {
			continue
		}
		name, _, _ := strings.Cut(trimmed, " ")
		if name == sub {
			return trimmed
		}
	}
	return ""
}

// A subcommand cannot be in two sets. Overlap means the route depends
// on map iteration order, which is the least debuggable bug available.
func TestNoSubcommandIsDeclaredTwice(t *testing.T) {
	for _, g := range everyGroup() {
		seen := map[string]string{}
		record := func(sub, set string) {
			if prev, dup := seen[sub]; dup {
				t.Errorf("%s: %q is declared in both %s and %s", g.name, sub, prev, set)
			}
			seen[sub] = set
		}
		for sub := range g.forms {
			record(sub, "forms")
		}
		for sub := range g.offlineOnly {
			record(sub, "offlineOnly")
		}
		for sub := range g.liveOnly {
			record(sub, "liveOnly")
		}
	}
}

// Nothing declared is nil. A nil in any of these maps is a route that
// panics rather than one that refuses.
func TestNoDeclaredSubcommandIsNil(t *testing.T) {
	for _, g := range everyGroup() {
		for sub, form := range g.forms {
			if form.live == nil || form.offline == nil {
				t.Errorf("%s %s: a declared form is nil", g.name, sub)
			}
		}
		for sub, fn := range g.offlineOnly {
			if fn == nil {
				t.Errorf("%s %s: offline-only entry is nil", g.name, sub)
			}
		}
		for sub, fn := range g.liveOnly {
			if fn == nil {
				t.Errorf("%s %s: live-only entry is nil", g.name, sub)
			}
		}
	}
}
