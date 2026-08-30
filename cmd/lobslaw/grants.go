package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// `lobslaw grants` — the conversation tier's half of "revocable".
//
// `lobslaw policy approvals` has covered the PERMANENT tier since it
// shipped, on the argument that a grant nobody can see or undo is only
// revocable in a document. The conversation tier had neither: /new was
// the only way to drop one, and it takes the transcript with it. So
// the choice on regretting an approval was between keeping it and
// losing the conversation it was given in.
//
// Deliberately its own command rather than `policy grants`. The two
// tiers are stored differently, expire differently and are keyed
// differently — permanent grants belong to a PRINCIPAL, conversation
// grants to a CONVERSATION — and filing one under the other would
// suggest they are the same thing seen from two angles.

const grantsUsage = `lobslaw grants — see and undo "approve for the rest of this conversation"

subcommands:
  list      show standing conversation grants
  revoke    drop them, by id or by conversation

  --conversation <channel>:<id>   narrow to one conversation
  --json                          machine-readable

Talks to a RUNNING node over mTLS — use --context, or --addr with the
credential flags. There is no --offline form: these expire on their
own, so reading them from a stopped cluster answers a question about a
moment that has already passed.

revoke is DRY RUN unless --apply is given.

For the permanent tier — what "always allow" minted — use
` + "`lobslaw policy approvals`" + `.`

func dispatchGrants(args []string) bool {
	if len(args) == 0 || args[0] != "grants" {
		return false
	}
	sub := args[1:]
	if len(sub) == 0 || sub[0] == "-h" || sub[0] == "--help" {
		fmt.Println(grantsUsage)
		return true
	}

	var run func([]string) error
	switch sub[0] {
	case "list":
		run = grantsList
	case "revoke":
		run = grantsRevoke
	default:
		fmt.Fprintf(os.Stderr, "unknown grants subcommand %q\n\n%s\n", sub[0], grantsUsage)
		os.Exit(2)
	}
	if err := run(sub[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "grants %s: %v\n", sub[0], err)
		os.Exit(1)
	}
	return true
}

func grantsList(args []string) error {
	fs := flag.NewFlagSet("grants list", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	conversation := fs.String("conversation", "", "narrow to one conversation (<channel>:<id>)")
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
	res, err := client.ListSessionGrants(ctx, &lobslawv1.ListSessionGrantsRequest{
		SessionId: strings.TrimSpace(*conversation),
	})
	if err != nil {
		return err
	}
	return renderGrants(os.Stdout, res, node.addr, *asJSON)
}

func grantsRevoke(args []string) error {
	fs := flag.NewFlagSet("grants revoke", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	conversation := fs.String("conversation", "", "revoke every grant in one conversation")
	apply := fs.Bool("apply", false, "actually revoke; without this it is a dry run")
	// parseFlagsAndPositionals, not fs.Parse: Go's flag package stops
	// at the first positional, so `revoke <id> --apply` would take
	// --apply as a second id and silently do a dry run while the
	// operator believed they had applied it.
	positional, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}

	req, err := grantsRevokeRequest(positional, *conversation, *apply)
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
	res, err := client.RevokeSessionGrants(ctx, req)
	if err != nil {
		return err
	}
	renderGrantRevoke(os.Stdout, res, *apply)
	return nil
}

// grantsRevokeRequest builds the request, refusing the shapes that
// would revoke more than the operator named.
//
// Split out so the argument rules are testable without a node — the
// mistake worth catching is a revoke that means something wider than
// what was typed, and that is decided here.
func grantsRevokeRequest(ids []string, conversation string, apply bool) (
	*lobslawv1.RevokeSessionGrantsRequest, error) {
	conversation = strings.TrimSpace(conversation)
	switch {
	case len(ids) == 0 && conversation == "":
		return nil, fmt.Errorf("name grant ids to revoke, or --conversation <channel>:<id> " +
			"to revoke a whole conversation's grants (`lobslaw grants list` shows both)")
	case len(ids) > 0 && conversation != "":
		return nil, fmt.Errorf("--conversation and explicit ids are alternatives; pass one")
	}
	return &lobslawv1.RevokeSessionGrantsRequest{
		Ids:       ids,
		SessionId: conversation,
		DryRun:    !apply,
	}, nil
}

func renderGrants(w io.Writer, res *lobslawv1.ListSessionGrantsResponse, addr string, asJSON bool) error {
	grants := res.GetGrants()
	if asJSON {
		out := make([]map[string]any, 0, len(grants))
		for _, g := range grants {
			out = append(out, map[string]any{
				"id":         g.GetId(),
				"session_id": g.GetSessionId(),
				"action":     g.GetAction(),
				"resource":   g.GetResource(),
				"granted_by": g.GetGrantedBy(),
				"prompt_id":  g.GetPromptId(),
				"expires_in": expiryOf(g),
			})
		}
		return emitJSON(map[string]any{
			"source":      addr,
			"ttl_seconds": res.GetTtlSeconds(),
			"grants":      out,
		})
	}
	if len(grants) == 0 {
		_, _ = fmt.Fprintf(w, "No standing conversation grants on %s.\n", addr)
		return nil
	}

	// Grouped by conversation, because that is the unit somebody
	// revokes: seeing four grants from one chat together is what makes
	// "--conversation" the obvious next command.
	byConv := map[string][]*lobslawv1.SessionGrant{}
	for _, g := range grants {
		byConv[g.GetSessionId()] = append(byConv[g.GetSessionId()], g)
	}
	convs := make([]string, 0, len(byConv))
	for c := range byConv {
		convs = append(convs, c)
	}
	sort.Strings(convs)

	_, _ = fmt.Fprintf(w, "%d standing grant(s) on %s. Each expires %s after it was given.\n",
		len(grants), addr, time.Duration(res.GetTtlSeconds())*time.Second)

	for _, c := range convs {
		_, _ = fmt.Fprintf(w, "\n%s\n", c)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "  ACTION\tRESOURCE\tGRANTED BY\tEXPIRES")
		for _, g := range byConv[c] {
			_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
				g.GetAction(), g.GetResource(), orDash(g.GetGrantedBy()), expiryOf(g))
		}
		_ = tw.Flush()
	}
	_, _ = fmt.Fprintf(w, "\nRevoke one conversation's:  lobslaw grants revoke --apply --conversation <id>\n")
	return nil
}

// expiryOf renders how long a grant has left, which is what somebody
// deciding whether to revoke it actually wants. An absolute timestamp
// makes them do the arithmetic.
func expiryOf(g *lobslawv1.SessionGrant) string {
	ts := g.GetExpiresAt()
	if ts == nil {
		return "-"
	}
	left := time.Until(ts.AsTime())
	if left <= 0 {
		// Visible rather than hidden: the sweeper runs periodically, so
		// an expired grant can still be listed, and it is not honoured.
		return "expired"
	}
	return left.Round(time.Minute).String()
}

func renderGrantRevoke(w io.Writer, res *lobslawv1.RevokeSessionGrantsResponse, applied bool) {
	revoked, notFound := res.GetRevoked(), res.GetNotFound()
	switch {
	case len(revoked) == 0 && len(notFound) == 0:
		_, _ = fmt.Fprintln(w, "Nothing matched.")
		return
	case !applied:
		_, _ = fmt.Fprintf(w, "DRY RUN — nothing was revoked. %d grant(s) would be:\n", len(revoked))
	default:
		_, _ = fmt.Fprintf(w, "Revoked %d grant(s):\n", len(revoked))
	}
	for _, id := range revoked {
		_, _ = fmt.Fprintf(w, "  %s\n", renderGrantID(id))
	}
	if len(notFound) > 0 {
		// Named rather than counted, for the same reason
		// revoke-approvals names them: "2 not found" leaves the
		// operator to guess, and the wrong guess is "I must have
		// already revoked it".
		_, _ = fmt.Fprintf(w, "not found (%d):\n", len(notFound))
		for _, id := range notFound {
			_, _ = fmt.Fprintf(w, "  %s\n", renderGrantID(id))
		}
	}
	if !applied {
		_, _ = fmt.Fprintln(w, "\nRe-run with --apply to revoke.")
	}
}

// renderGrantID makes the NUL-separated key readable. The separator is
// deliberately unprintable so a channel id cannot forge one, which
// also means echoing it raw prints a line with invisible joins.
func renderGrantID(id string) string {
	return strings.ReplaceAll(id, "\x00", "  ")
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
