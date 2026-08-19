package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/jmylchreest/lobslaw/internal/memory"
)

// Binding a channel address to a canonical id re-points that person at
// a new principal. Everything they own was written under the old one,
// so without this it stays there: not deleted, not theirs any more,
// and invisible to them.
//
// Offline and dry-run by default, like `lobslaw memory forget`.
// Rewriting ownership records is not something to do on a typo.

const identityUsage = `lobslaw identity — repoint a principal after binding a channel

subcommands:
  rebind <from> <to>   move everything owned by <from> to <to>

Talks to a RUNNING node over mTLS by default — use --context, or --addr
with the credential flags — so the rewrites REPLICATE. Pass --offline to
open state.db directly instead; that path needs the node STOPPED,
because bbolt takes an exclusive lock, and pointing it at a follower's
file while the cluster runs would write ownership no other replica has.

Ids are bare, as they appear in claims.UserID — "tg-@alice", not
"user:tg-@alice". The principal prefix is added where records use it.

rebind is DRY RUN unless --apply is given.`

// identityForms pairs each subcommand's live and offline
// implementation.
//
// A table rather than a switch so the ROUTING is a value a test can
// assert. The bug worth catching is not a missing function — it is
// `rebind --apply` writing to one replica's file while the cluster
// runs.
var identityForms = map[string]struct{ live, offline func([]string) error }{
	"rebind": {live: identityRebindLive, offline: identityRebind},
}

// identityRoute returns the implementation for a subcommand, or nil if
// there is none. Live is the default; --offline is the opt-out.
func identityRoute(sub string, offline bool) func([]string) error {
	form, ok := identityForms[sub]
	if !ok {
		return nil
	}
	if offline {
		return form.offline
	}
	return form.live
}

func dispatchIdentity(args []string) bool {
	idx := findSubcmd(args, "identity")
	if idx < 0 {
		return false
	}
	sub := args[idx+1:]
	if len(sub) == 0 {
		fmt.Fprintln(os.Stderr, identityUsage)
		os.Exit(2)
	}

	rest, offline := takeOffline(sub[1:])
	run := identityRoute(sub[0], offline)
	if run == nil {
		fmt.Fprintf(os.Stderr, "unknown identity subcommand %q\n\n%s\n", sub[0], identityUsage)
		os.Exit(2)
	}
	if err := run(rest); err != nil {
		fmt.Fprintf(os.Stderr, "identity %s: %v\n", sub[0], err)
		os.Exit(1)
	}
	return true
}

func identityRebind(args []string) error {
	fs := flag.NewFlagSet("identity rebind", flag.ExitOnError)
	var store offlineStore
	store.bind(fs)
	apply := fs.Bool("apply", false, "actually rewrite (default is a dry run)")
	asJSON := fs.Bool("json", false, "emit JSON")
	positional, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	from, to, err := rebindArgs(positional)
	if err != nil {
		return err
	}

	s, path, oerr := store.open()
	if oerr != nil {
		return oerr
	}
	defer func() { _ = s.Close() }()

	plan, err := memory.PlanRebind(s, from, to)
	if err != nil {
		return err
	}
	if *apply && plan.Total() > 0 {
		if err := memory.ApplyRebindOffline(s, from, to); err != nil {
			return err
		}
	}
	return renderRebind(os.Stdout, plan, from, to, path, *apply, *asJSON)
}
