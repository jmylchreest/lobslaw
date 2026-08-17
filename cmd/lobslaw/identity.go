package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Binding a channel address to a canonical id re-points that person at
// a new principal. Everything they own was written under the old one,
// so without this it stays there: not deleted, not theirs any more,
// and invisible to them.
//
// Offline and dry-run by default, like `lobslaw memory forget`.
// Rewriting ownership records is not something to do on a typo.

const identityUsage = `lobslaw identity — repoint a principal after binding a channel

The node must be STOPPED. These subcommands open state.db directly and
bbolt takes an exclusive lock on the file, so a running node makes every
one of them fail.

subcommands:
  rebind <from> <to>   move everything owned by <from> to <to>

Ids are bare, as they appear in claims.UserID — "tg-@alice", not
"user:tg-@alice". The principal prefix is added where records use it.

rebind is DRY RUN unless --apply is given.`

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

	var err error
	switch sub[0] {
	case "rebind":
		err = identityRebind(sub[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown identity subcommand %q\n\n%s\n", sub[0], identityUsage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "identity %s: %v\n", sub[0], err)
		os.Exit(1)
	}
	return true
}

// rebindPlan is what a rebind would change, per bucket.
type rebindPlan struct {
	// Changes maps bucket name → record ids that would be rewritten.
	Changes map[string][]string
	// Conflicts are records the rebind refuses to touch, with why.
	// Reported rather than resolved: guessing at a merge is how a
	// migration loses somebody's data quietly.
	Conflicts []string
}

func (p *rebindPlan) note(bucket, id string) {
	if p.Changes == nil {
		p.Changes = map[string][]string{}
	}
	p.Changes[bucket] = append(p.Changes[bucket], id)
}

func (p *rebindPlan) total() int {
	var n int
	for _, ids := range p.Changes {
		n += len(ids)
	}
	return n
}

func identityRebind(args []string) error {
	fs := flag.NewFlagSet("identity rebind", flag.ExitOnError)
	var store offlineStore
	store.bind(fs)
	apply := fs.Bool("apply", false, "actually rewrite (default is a dry run)")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 2 {
		return fmt.Errorf("usage: lobslaw identity rebind <from> <to> [--apply]")
	}
	from, to := strings.TrimSpace(rest[0]), strings.TrimSpace(rest[1])
	switch {
	case from == "" || to == "":
		return fmt.Errorf("both <from> and <to> are required")
	case from == to:
		return fmt.Errorf("<from> and <to> are the same id (%q); nothing to do", from)
	}

	s, path, err := store.open()
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	plan, err := planRebind(s, from, to)
	if err != nil {
		return err
	}
	if *apply && plan.total() > 0 {
		if err := applyRebind(s, from, to); err != nil {
			return err
		}
	}

	if *asJSON {
		return emitJSON(map[string]any{
			"applied":   *apply,
			"state_db":  path,
			"from":      from,
			"to":        to,
			"changes":   plan.Changes,
			"conflicts": plan.Conflicts,
			"total":     plan.total(),
		})
	}

	fmt.Printf("%s\n", path)
	fmt.Printf("%s -> %s\n\n", from, to)
	for _, bucket := range sortedKeys(plan.Changes) {
		ids := plan.Changes[bucket]
		fmt.Printf("  %-20s %d record(s)\n", bucket, len(ids))
		fprintSample(os.Stdout, ids)
	}
	for _, c := range plan.Conflicts {
		fmt.Printf("  SKIPPED: %s\n", c)
	}
	switch {
	case plan.total() == 0:
		fmt.Println("\nnothing owned by that id.")
	case *apply:
		fmt.Printf("\nREBOUND %d record(s).\n", plan.total())
	default:
		fmt.Printf("\nDRY RUN — nothing was written. Re-run with --apply to rebind %d record(s).\n", plan.total())
	}
	return nil
}

// rebindRewriter walks one bucket, decoding each record and reporting
// whether it belongs to `from`. Returning the re-encoded bytes applies
// the change; returning nil leaves the record alone.
type rebindRewriter struct {
	bucket string
	// rewrite returns the new bytes and true when the record was
	// owned by from, or (nil, false) when it was not.
	rewrite func(raw []byte, fromPrincipal, toPrincipal, fromID, toID string) ([]byte, bool, error)
}

func rebindRewriters() []rebindRewriter {
	return []rebindRewriter{
		{memory.BucketVectorRecords, rewriteVectorOwner},
		{memory.BucketEpisodicRecords, rewriteEpisodicOwner},
		{memory.BucketCommitments, rewriteCommitmentOwner},
		{memory.BucketScheduledTasks, rewriteTaskOwner},
		{memory.BucketPrompts, rewritePromptOwner},
		{memory.BucketSessions, rewriteSessionUser},
		{memory.BucketPolicyRules, rewriteRuleSubject},
	}
}

func planRebind(s *memory.Store, from, to string) (*rebindPlan, error) {
	plan := &rebindPlan{}
	fromP, toP := identity.User(from).String(), identity.User(to).String()

	for _, rw := range rebindRewriters() {
		err := s.ForEach(rw.bucket, func(key string, raw []byte) error {
			_, changed, err := rw.rewrite(raw, fromP, toP, from, to)
			if err != nil {
				plan.Conflicts = append(plan.Conflicts,
					fmt.Sprintf("%s/%s: %v", rw.bucket, key, err))
				return nil
			}
			if changed {
				plan.note(rw.bucket, key)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", rw.bucket, err)
		}
	}

	// User prefs are keyed BY the canonical id, so a rebind would have
	// to merge two records rather than rewrite a field. Reported, not
	// attempted: preferences are small and hand-written, and silently
	// picking a winner between two timezones is worse than saying so.
	if _, err := s.Get(memory.BucketUserPrefs, from); err == nil {
		plan.Conflicts = append(plan.Conflicts, fmt.Sprintf(
			"user_prefs/%s exists and is keyed by the id itself — copy any settings onto %q by hand, "+
				"then delete it; this command will not merge two preference records", from, to))
	}
	return plan, nil
}

func applyRebind(s *memory.Store, from, to string) error {
	fromP, toP := identity.User(from).String(), identity.User(to).String()

	for _, rw := range rebindRewriters() {
		// Collected before writing: mutating a bucket while iterating
		// it is not something bbolt promises anything about.
		pending := map[string][]byte{}
		err := s.ForEach(rw.bucket, func(key string, raw []byte) error {
			out, changed, err := rw.rewrite(raw, fromP, toP, from, to)
			if err != nil || !changed {
				return nil //nolint:nilerr // conflicts were reported by planRebind
			}
			pending[key] = out
			return nil
		})
		if err != nil {
			return fmt.Errorf("scan %s: %w", rw.bucket, err)
		}
		for key, raw := range pending {
			if err := s.Put(rw.bucket, key, raw); err != nil {
				return fmt.Errorf("write %s/%s: %w", rw.bucket, key, err)
			}
		}
	}
	return nil
}

// The owner rewriters are spelled out one per record type rather than
// shared behind an interface. Protobuf messages carry state that must
// not be copied by value, and the generated types have no common owner
// accessor to write against — the clever version of this was five
// lines shorter and a copylocks violation waiting to happen.
//
// Each matches the principal form ("user:alice") AND the bare id,
// because both shapes have been written into owner fields over the
// project's life and a migration understanding only one would leave
// the other behind.

func rewriteVectorOwner(raw []byte, fromP, toP, fromID, toID string) ([]byte, bool, error) {
	var rec lobslawv1.VectorRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, false, fmt.Errorf("decode: %w", err)
	}
	next, ok := rebindOwner(rec.Owner, fromP, toP, fromID, toID)
	if !ok {
		return nil, false, nil
	}
	rec.Owner = next
	return marshalRebound(&rec)
}

func rewriteEpisodicOwner(raw []byte, fromP, toP, fromID, toID string) ([]byte, bool, error) {
	var rec lobslawv1.EpisodicRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, false, fmt.Errorf("decode: %w", err)
	}
	next, ok := rebindOwner(rec.Owner, fromP, toP, fromID, toID)
	if !ok {
		return nil, false, nil
	}
	rec.Owner = next
	return marshalRebound(&rec)
}

func rewriteCommitmentOwner(raw []byte, fromP, toP, fromID, toID string) ([]byte, bool, error) {
	var rec lobslawv1.AgentCommitment
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, false, fmt.Errorf("decode: %w", err)
	}
	next, ok := rebindOwner(rec.Owner, fromP, toP, fromID, toID)
	if !ok {
		return nil, false, nil
	}
	rec.Owner = next
	return marshalRebound(&rec)
}

func rewriteTaskOwner(raw []byte, fromP, toP, fromID, toID string) ([]byte, bool, error) {
	var rec lobslawv1.ScheduledTaskRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, false, fmt.Errorf("decode: %w", err)
	}
	next, ok := rebindOwner(rec.Owner, fromP, toP, fromID, toID)
	if !ok {
		return nil, false, nil
	}
	rec.Owner = next
	return marshalRebound(&rec)
}

func rewritePromptOwner(raw []byte, fromP, toP, fromID, toID string) ([]byte, bool, error) {
	var rec lobslawv1.PromptRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, false, fmt.Errorf("decode: %w", err)
	}
	next, ok := rebindOwner(rec.Owner, fromP, toP, fromID, toID)
	if !ok {
		return nil, false, nil
	}
	rec.Owner = next
	return marshalRebound(&rec)
}

// rebindOwner reports the replacement for an owner value, or ok=false
// when the record belongs to somebody else.
func rebindOwner(current, fromP, toP, fromID, toID string) (string, bool) {
	switch current {
	case fromP:
		return toP, true
	case fromID:
		return toID, true
	default:
		return "", false
	}
}

func marshalRebound(m proto.Message) ([]byte, bool, error) {
	out, err := proto.Marshal(m)
	if err != nil {
		return nil, false, fmt.Errorf("encode: %w", err)
	}
	return out, true, nil
}

func rewriteSessionUser(raw []byte, _, _, fromID, toID string) ([]byte, bool, error) {
	var rec lobslawv1.SessionRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, false, fmt.Errorf("decode: %w", err)
	}
	if rec.UserId != fromID {
		return nil, false, nil
	}
	rec.UserId = toID
	out, err := proto.Marshal(&rec)
	if err != nil {
		return nil, false, fmt.Errorf("encode: %w", err)
	}
	return out, true, nil
}

// rewriteRuleSubject repoints rules written against this principal,
// including the ones an "always" approval minted.
//
// Only `user:` subjects. A role or scope subject names a group the
// person is in, not the person — rebinding those would move everybody
// who holds the role.
func rewriteRuleSubject(raw []byte, fromP, toP, _, _ string) ([]byte, bool, error) {
	var rec lobslawv1.PolicyRule
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, false, fmt.Errorf("decode: %w", err)
	}
	if rec.Subject != fromP {
		return nil, false, nil
	}
	rec.Subject = toP
	out, err := proto.Marshal(&rec)
	if err != nil {
		return nil, false, fmt.Errorf("encode: %w", err)
	}
	return out, true, nil
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
