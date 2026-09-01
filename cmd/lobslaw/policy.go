package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/policy"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// An "always" approval is a permanent widening of what the agent may
// do, tapped once and then easy to forget. These subcommands are the
// other half of that feature: without a way to see and undo the
// grants, "revocable" is a claim in a doc rather than something a
// person can act on.
//
// Both used to open state.db directly, which needs the node STOPPED —
// and on an operator's laptop there is no state.db to open. Revoking a
// grant you regret is routine, and a workflow that begins "stop the
// cluster" is one nobody performs. So live is the default now, and
// --offline is the forensic opt-out.

const policyUsage = `lobslaw policy — see and undo the grants an "always" approval made

subcommands:
  approvals          list the rules minted by "always" approvals
  revoke-approvals   delete them, all or by id
  classify           show what the shell classifier makes of a command,
                     and what each approval mode would do with it

Both talk to a RUNNING node over mTLS by default — use --context, or
--addr with the credential flags. Pass --offline to open state.db
directly instead; that path needs the node STOPPED, because bbolt takes
an exclusive lock, and it exists for reading a cluster that will not
start.

revoke-approvals is DRY RUN unless --apply is given, and refuses to
touch any rule an operator wrote — only approval-minted ones. That
refusal is enforced by the NODE, not by this command.`

// policyForms pairs each subcommand's live and offline implementation.
//
// A table rather than a switch so the ROUTING is a value a test can
// assert. The bug worth catching is not a missing function — it is
// `approvals` quietly reading a laptop-local state.db and reporting an
// empty list as "no grants outstanding".
var policyForms = map[string]struct{ live, offline func([]string) error }{
	"approvals":        {live: policyApprovalsLive, offline: policyApprovals},
	"revoke-approvals": {live: policyRevokeLive, offline: policyRevokeApprovals},
}

// policyRoute returns the implementation for a subcommand, or nil if
// there is none. Live is the default; --offline is the opt-out.
func policyRoute(sub string, offline bool) func([]string) error {
	// A local-only subcommand reads no state at all, so --offline is
	// neither honoured nor needed: there is nothing for it to change.
	if fn, ok := policyLocalOnly[sub]; ok {
		return fn
	}
	form, ok := policyForms[sub]
	if !ok {
		return nil
	}
	if offline {
		return form.offline
	}
	return form.live
}

// dispatchPolicy handles `lobslaw policy <subcmd>`.
func dispatchPolicy(args []string) bool {
	idx := findSubcmd(args, "policy")
	if idx < 0 {
		return false
	}
	sub := args[idx+1:]
	if len(sub) == 0 {
		fmt.Fprintln(os.Stderr, policyUsage)
		os.Exit(2)
	}

	rest, offline := takeOffline(sub[1:])

	run := policyRoute(sub[0], offline)
	if run == nil {
		fmt.Fprintf(os.Stderr, "unknown policy subcommand %q\n\n%s\n", sub[0], policyUsage)
		os.Exit(2)
	}
	if err := run(rest); err != nil {
		fmt.Fprintf(os.Stderr, "policy %s: %v\n", sub[0], err)
		os.Exit(1)
	}
	return true
}

func policyClient(node *liveNode) (lobslawv1.PolicyServiceClient, func(), error) {
	conn, err := node.dial()
	if err != nil {
		return nil, nil, err
	}
	return lobslawv1.NewPolicyServiceClient(conn), func() { _ = conn.Close() }, nil
}

// --- live --------------------------------------------------------------

// policyApprovalsLive lists approval-minted rules on a running node.
//
// SyncRules already returns the complete set with provenance attached,
// so the filter is the same one the offline form applies, against the
// same constant. No new RPC was needed for reading.
func policyApprovalsLive(args []string) error {
	fs := flag.NewFlagSet("policy approvals", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	client, closeConn, err := policyClient(&node)
	if err != nil {
		return err
	}
	defer closeConn()

	ctx, cancel := node.ctx()
	defer cancel()
	res, err := client.SyncRules(ctx, &lobslawv1.SyncRulesRequest{})
	if err != nil {
		return err
	}
	return renderApprovals(os.Stdout, approvalRulesFrom(res.GetRules()), node.addr, *asJSON)
}

// policyRevokeLive revokes on a running node.
//
// The provenance check is NOT done here. RevokeApprovalRules is scoped
// to approval-minted rules at the server, because a check in the
// client is one an attacker replaces — and "revocable" is only worth
// something if the revoking stays narrow.
func policyRevokeLive(args []string) error {
	fs := flag.NewFlagSet("policy revoke-approvals", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	apply := fs.Bool("apply", false, "actually delete (default is a dry run)")
	all := fs.Bool("all", false, "revoke every approval-minted rule")
	asJSON := fs.Bool("json", false, "emit JSON")
	positional, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	req, err := revokeRequest(positional, *all, *apply)
	if err != nil {
		return err
	}

	client, closeConn, err := policyClient(&node)
	if err != nil {
		return err
	}
	defer closeConn()

	ctx, cancel := node.ctx()
	defer cancel()
	res, err := client.RevokeApprovalRules(ctx, req)
	if err != nil {
		return err
	}

	if *asJSON {
		return emitJSON(map[string]any{
			"applied": *apply, "source": node.addr,
			"revoked": res.GetRevoked(), "refused": res.GetRefused(), "not_found": res.GetNotFound(),
		})
	}
	renderRevocation(os.Stdout, node.addr, res.GetRevoked(), res.GetRefused(), res.GetNotFound(), *apply)
	return nil
}

// revokeRequest turns the flags into a request, refusing the two
// shapes the server also refuses.
//
// Checked here as well so the operator gets the refusal without a
// round trip — and DryRun is the INVERSE of --apply, which is the one
// bit of this command that must never invert: a dry run that writes is
// the flag doing the opposite of what it says.
func revokeRequest(ids []string, all, apply bool) (*lobslawv1.RevokeApprovalRulesRequest, error) {
	switch {
	case len(ids) == 0 && !all:
		// Naming nothing is not "everything". A command that turns a
		// missing argument into a blanket revocation is one somebody
		// runs by accident.
		return nil, fmt.Errorf("name the rule ids to revoke, or pass --all")
	case len(ids) > 0 && all:
		return nil, fmt.Errorf("--all and explicit ids are mutually exclusive")
	}
	return &lobslawv1.RevokeApprovalRulesRequest{
		Ids:    ids,
		All:    all,
		DryRun: !apply,
	}, nil
}

// approvalRulesFrom keeps only the approval-minted rules, sorted by id
// so output is stable.
//
// The same constant the node uses. A second definition of "minted by
// an approval" on the client would be a second authority for a fact
// the server already owns.
func approvalRulesFrom(rules []*lobslawv1.PolicyRule) []*lobslawv1.PolicyRule {
	out := make([]*lobslawv1.PolicyRule, 0, len(rules))
	for _, r := range rules {
		if strings.HasPrefix(r.GetCreatedBy(), policy.ApprovalRulePrefix) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetId() < out[j].GetId() })
	return out
}

// renderApprovals prints the rules and SAYS WHERE THEY CAME FROM. An
// empty list is indistinguishable from the wrong store unless the
// source is on the page.
func renderApprovals(w io.Writer, rules []*lobslawv1.PolicyRule, source string, asJSON bool) error {
	if asJSON {
		out := make([]map[string]any, 0, len(rules))
		for _, r := range rules {
			out = append(out, approvalRuleJSON(r))
		}
		return emitJSON(map[string]any{"source": source, "rules": out})
	}

	_, _ = fmt.Fprintf(w, "%s\n", source)
	if len(rules) == 0 {
		_, _ = fmt.Fprintln(w, "no approval-minted rules.")
		return nil
	}
	for _, r := range rules {
		when := ""
		if r.GetCreatedAt() != nil {
			when = r.GetCreatedAt().AsTime().Format("2006-01-02 15:04")
		}
		_, _ = fmt.Fprintf(w, "  %-40s %s %s -> %s  (%s)\n",
			r.GetId(), r.GetSubject(), r.GetAction(), r.GetResource(), when)
	}
	_, _ = fmt.Fprintf(w, "\n%d rule(s). Revoke with: lobslaw policy revoke-approvals [<id>...] --apply\n", len(rules))
	return nil
}

// renderRevocation reports what happened, keeping protected rules and
// unknown ids apart: one is a rule somebody wrote deliberately, the
// other is a typo, and "not revoked" without saying which leaves the
// operator to guess.
func renderRevocation(w io.Writer, source string, revoked, refused, notFound []string, applied bool) {
	_, _ = fmt.Fprintf(w, "%s\n", source)
	for _, id := range revoked {
		_, _ = fmt.Fprintf(w, "  %s\n", id)
	}
	if len(refused) > 0 {
		_, _ = fmt.Fprintf(w, "\nnot approval-minted, left alone: %s\n", strings.Join(refused, ", "))
	}
	if len(notFound) > 0 {
		_, _ = fmt.Fprintf(w, "no such rule: %s\n", strings.Join(notFound, ", "))
	}
	switch {
	case len(revoked) == 0:
		_, _ = fmt.Fprintln(w, "\nnothing to do.")
	case applied:
		_, _ = fmt.Fprintf(w, "\nREVOKED %d rule(s).\n", len(revoked))
	default:
		_, _ = fmt.Fprintf(w, "\nDRY RUN — nothing was written. Re-run with --apply to revoke %d rule(s).\n", len(revoked))
	}
}

// --- offline -----------------------------------------------------------

func policyApprovals(args []string) error {
	fs := flag.NewFlagSet("policy approvals", flag.ExitOnError)
	var store offlineStore
	store.bind(fs)
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	s, path, err := store.open()
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	rules, err := approvalMintedRules(s)
	if err != nil {
		return err
	}

	if *asJSON {
		out := make([]map[string]any, 0, len(rules))
		for _, r := range rules {
			out = append(out, approvalRuleJSON(r))
		}
		return emitJSON(map[string]any{"state_db": path, "rules": out})
	}

	fmt.Printf("%s\n", path)
	if len(rules) == 0 {
		fmt.Println("no approval-minted rules.")
		return nil
	}
	for _, r := range rules {
		when := ""
		if r.CreatedAt != nil {
			when = r.CreatedAt.AsTime().Format("2006-01-02 15:04")
		}
		fmt.Printf("  %-40s %s %s -> %s  (%s)\n",
			r.Id, r.Subject, r.Action, r.Resource, when)
	}
	fmt.Printf("\n%d rule(s). Revoke with: lobslaw policy revoke-approvals [<id>...] --apply\n", len(rules))
	return nil
}

func policyRevokeApprovals(args []string) error {
	fs := flag.NewFlagSet("policy revoke-approvals", flag.ExitOnError)
	var store offlineStore
	store.bind(fs)
	apply := fs.Bool("apply", false, "actually delete (default is a dry run)")
	asJSON := fs.Bool("json", false, "emit JSON")
	positional, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	wanted := positional

	s, path, err := store.open()
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	rules, err := approvalMintedRules(s)
	if err != nil {
		return err
	}

	// An id the operator named that is not an approval-minted rule is
	// reported rather than skipped. Silently ignoring it would let
	// someone believe they had revoked something they had not.
	var target []*lobslawv1.PolicyRule
	var unknown []string
	if len(wanted) == 0 {
		target = rules
	} else {
		byID := make(map[string]*lobslawv1.PolicyRule, len(rules))
		for _, r := range rules {
			byID[r.Id] = r
		}
		for _, id := range wanted {
			if r, ok := byID[id]; ok {
				target = append(target, r)
				continue
			}
			unknown = append(unknown, id)
		}
	}

	if *apply {
		for _, r := range target {
			if err := s.Delete(memory.BucketPolicyRules, r.Id); err != nil {
				return fmt.Errorf("delete %s: %w", r.Id, err)
			}
		}
	}

	if *asJSON {
		ids := make([]string, 0, len(target))
		for _, r := range target {
			ids = append(ids, r.Id)
		}
		return emitJSON(map[string]any{
			"applied":  *apply,
			"state_db": path,
			"revoked":  ids,
			"unknown":  unknown,
		})
	}

	fmt.Printf("%s\n", path)
	for _, r := range target {
		fmt.Printf("  %-40s %s %s -> %s\n", r.Id, r.Subject, r.Action, r.Resource)
	}
	if len(unknown) > 0 {
		fmt.Printf("\nnot approval-minted (left alone): %s\n", strings.Join(unknown, ", "))
	}
	switch {
	case len(target) == 0:
		fmt.Println("\nnothing to do.")
	case *apply:
		fmt.Printf("\nREVOKED %d rule(s).\n", len(target))
	default:
		fmt.Printf("\nDRY RUN — nothing was written. Re-run with --apply to revoke %d rule(s).\n", len(target))
	}
	return nil
}

// approvalMintedRules reads every rule whose provenance says an
// approval created it, sorted by id so output is stable.
func approvalMintedRules(s *memory.Store) ([]*lobslawv1.PolicyRule, error) {
	var out []*lobslawv1.PolicyRule
	err := s.ForEach(memory.BucketPolicyRules, func(_ string, raw []byte) error {
		var rule lobslawv1.PolicyRule
		if err := proto.Unmarshal(raw, &rule); err != nil {
			return nil //nolint:nilerr // one unreadable rule should not hide the rest
		}
		if strings.HasPrefix(rule.CreatedBy, policy.ApprovalRulePrefix) {
			out = append(out, &rule)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read policy rules: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out, nil
}

func approvalRuleJSON(r *lobslawv1.PolicyRule) map[string]any {
	m := map[string]any{
		"id":         r.Id,
		"subject":    r.Subject,
		"action":     r.Action,
		"resource":   r.Resource,
		"effect":     r.Effect,
		"created_by": r.CreatedBy,
	}
	if r.CreatedAt != nil {
		m["created_at"] = r.CreatedAt.AsTime()
	}
	return m
}
