package main

import (
	"flag"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
	"text/tabwriter"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// The live half of `lobslaw learned`.
//
// Everything else in this file's neighbour opens state.db directly,
// which takes bbolt's exclusive lock and therefore needs the node
// stopped. That is the right shape for forensics. It is the wrong one
// for approval: approving a proposal is routine, and a workflow that
// begins "stop the cluster" is one nobody performs — after which
// propose mode is a queue that only fills and the curator's expiry
// does all the deciding.

const learnedLiveUsage = `These subcommands talk to a RUNNING node:

  pending [--all]        proposals waiting for a decision
  approve <id>...        let a proposal out of PROPOSED
  accept <id>...         apply a staged refinement
  reject <id>...         discard a staged refinement, leaving the live one
  archive <id> <reason>  move a live artefact out of use, recoverably
  restore <id>...        bring an archived artefact back, as a proposal

Connection comes from --config ([cluster] advertise_addr and
[cluster.mtls]), or from --addr / --ca-cert / --node-cert / --node-key.

Approval is recorded against a named person. --as defaults to the
current OS user; pass it explicitly on shared machines.

Pass --offline to work on a STOPPED node's state.db instead. That path
is for reading a cluster that will not start; it is not the routine
way to approve anything, because a workflow beginning "stop the
cluster" is one nobody performs.`

// livePending lists what is waiting for somebody.
func livePending(args []string) error {
	fs := flag.NewFlagSet("learned pending", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	all := fs.Bool("all", false, "include live artefacts with a staged refinement")
	if err := fs.Parse(args); err != nil {
		return err
	}

	conn, err := node.dial()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	client := lobslawv1.NewSelfLearningServiceClient(conn)

	ctx, cancel := node.ctx()
	defer cancel()
	resp, err := client.ListArtefacts(ctx, &lobslawv1.ListArtefactsRequest{
		State: lobslawv1.SelfTaughtState_SELF_TAUGHT_STATE_PROPOSED,
	})
	if err != nil {
		return err
	}
	rows := resp.GetArtefacts()

	// A staged refinement against a live artefact is also "waiting for
	// somebody", and it is invisible in a PROPOSED filter because the
	// record itself is ACTIVE. Listing only one of the two would make
	// half the queue unreachable from the command whose job is to show
	// the queue.
	if *all {
		ctx2, cancel2 := node.ctx()
		defer cancel2()
		live, err := client.ListArtefacts(ctx2, &lobslawv1.ListArtefactsRequest{})
		if err != nil {
			return err
		}
		for _, rec := range live.GetArtefacts() {
			if rec.GetPending() != nil {
				rows = append(rows, rec)
			}
		}
	}

	if len(rows) == 0 {
		fmt.Println("nothing waiting")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tKIND\tNAME\tWAITING ON\tDESCRIPTION")
	for _, rec := range rows {
		waiting := "approval"
		if rec.GetPending() != nil {
			waiting = "refinement"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			rec.GetId(), kindLabel(rec.GetKind()), rec.GetName(), waiting, rec.GetDescription())
	}
	return w.Flush()
}

// liveApprove lets proposals out of PROPOSED.
func liveApprove(args []string) error {
	fs := flag.NewFlagSet("learned approve", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	as := fs.String("as", "", "principal recorded as the approver; defaults to the OS user")
	ids, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return fmt.Errorf("approve: at least one artefact id is required\n\n%s", learnedLiveUsage)
	}
	by, err := approverName(*as)
	if err != nil {
		return err
	}

	conn, err := node.dial()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	client := lobslawv1.NewSelfLearningServiceClient(conn)

	// Each id independently. One bad id in a list must not silently
	// drop the approvals that would have succeeded — the operator
	// would have no way to tell which took effect.
	var failed int
	for _, id := range ids {
		ctx, cancel := node.ctx()
		resp, err := client.ApproveArtefact(ctx, &lobslawv1.ApproveArtefactRequest{
			Id: id, ApprovedBy: by,
		})
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", id, err)
			failed++
			continue
		}
		fmt.Printf("approved %s (%s) by %s\n", id, resp.GetArtefact().GetName(), by)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d could not be approved", failed, len(ids))
	}
	return nil
}

// liveDecide accepts or rejects staged refinements.
func liveDecide(args []string, accept bool) error {
	name := "reject"
	if accept {
		name = "accept"
	}
	fs := flag.NewFlagSet("learned "+name, flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	as := fs.String("as", "", "principal recorded as the approver; defaults to the OS user")
	ids, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return fmt.Errorf("%s: at least one artefact id is required\n\n%s", name, learnedLiveUsage)
	}
	// Only an acceptance needs attribution: a rejection changes nothing
	// about what the agent follows, and the thing discarded was never
	// in force.
	var by string
	if accept {
		var err error
		if by, err = approverName(*as); err != nil {
			return err
		}
	}

	conn, err := node.dial()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	client := lobslawv1.NewSelfLearningServiceClient(conn)

	var failed int
	for _, id := range ids {
		ctx, cancel := node.ctx()
		resp, err := client.DecideRevision(ctx, &lobslawv1.DecideRevisionRequest{
			Id: id, Accept: accept, DecidedBy: by,
		})
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", id, err)
			failed++
			continue
		}
		fmt.Printf("%sed refinement to %s (now version %d)\n",
			name, id, resp.GetArtefact().GetVersion())
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d could not be %sed", failed, len(ids), name)
	}
	return nil
}

// liveShelve archives one artefact on a running node.
func liveShelve(args []string) error {
	fs := flag.NewFlagSet("learned archive", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	rest, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(rest) < 2 {
		return fmt.Errorf("archive: need an id and a reason\n\n%s", learnedLiveUsage)
	}
	id, reason := rest[0], strings.Join(rest[1:], " ")

	conn, err := node.dial()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := node.ctx()
	defer cancel()
	if _, err := lobslawv1.NewSelfLearningServiceClient(conn).ArchiveArtefact(ctx,
		&lobslawv1.ArchiveArtefactRequest{Id: id, Reason: reason}); err != nil {
		return err
	}
	fmt.Printf("archived %s: %s\n", id, reason)
	return nil
}

// liveRestore brings archived artefacts back as proposals.
func liveRestore(args []string) error {
	fs := flag.NewFlagSet("learned restore", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	ids, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return fmt.Errorf("restore: at least one artefact id is required\n\n%s", learnedLiveUsage)
	}

	conn, err := node.dial()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	client := lobslawv1.NewSelfLearningServiceClient(conn)

	var failed int
	for _, id := range ids {
		ctx, cancel := node.ctx()
		_, err := client.RestoreArtefact(ctx, &lobslawv1.RestoreArtefactRequest{Id: id})
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", id, err)
			failed++
			continue
		}
		// Said out loud, because it is the surprising half: something
		// that archived itself out of use once does not go straight
		// back into force.
		fmt.Printf("restored %s as a PROPOSAL — approve it to put it back in force\n", id)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d could not be restored", failed, len(ids))
	}
	return nil
}

// approverName resolves who is approving.
//
// Defaults to the OS user rather than to something like "cli". An
// approval attributed to the tool rather than to a person is one
// nobody can be asked about, which is the whole reason the field
// exists — and on a single-admin box making them type --as every time
// would just train them to script it away.
func approverName(as string) (string, error) {
	if as = strings.TrimSpace(as); as != "" {
		return as, nil
	}
	u, err := user.Current()
	if err != nil || strings.TrimSpace(u.Username) == "" {
		return "", fmt.Errorf(
			"cannot determine who is approving: pass --as (%v)", err)
	}
	return "user:" + u.Username, nil
}

// liveList is `learned list` against a running node.
//
// ListArtefacts already existed and had no caller here, so the usage
// text calling list "offline-only" was a claim rather than a
// constraint — and on a laptop it meant printing "the agent has taught
// itself nothing" about a cluster it never contacted.
func liveList(args []string) error {
	fs := flag.NewFlagSet("learned list", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	archived := fs.Bool("archived", false, "read the archive instead of the live set")
	owner := fs.String("owner", "", "restrict to one principal")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	conn, err := node.dial()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	client := lobslawv1.NewSelfLearningServiceClient(conn)

	ctx, cancel := node.ctx()
	defer cancel()
	resp, err := client.ListArtefacts(ctx, &lobslawv1.ListArtefactsRequest{Archived: *archived})
	if err != nil {
		return explainUnimplemented(err, node.addr)
	}

	// Owner filtering is client-side because the RPC has no such
	// filter. Done here rather than adding one: the artefact set is
	// small by construction — it is what one agent taught itself — and
	// a filter on the wire that only this command uses is protocol
	// nobody else needs.
	records := filterByOwner(resp.GetArtefacts(), *owner)
	return renderLearnedList(os.Stdout, records, node.addr, *archived, *asJSON)
}

func filterByOwner(records []*lobslawv1.SelfTaughtRecord, owner string) []*lobslawv1.SelfTaughtRecord {
	if owner == "" {
		return records
	}
	out := make([]*lobslawv1.SelfTaughtRecord, 0, len(records))
	for _, r := range records {
		if r.GetOwner() == owner {
			out = append(out, r)
		}
	}
	return out
}

// liveHistory shows the versions kept for rollback, from a running
// node.
//
// The offline form reads the history bucket directly, which it cannot
// do while the node holds state.db. "What did it used to think" is a
// question about a running agent, so requiring it be stopped first
// made the answer unavailable exactly when it was wanted.
func liveHistory(args []string) error {
	fs := flag.NewFlagSet("learned history", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	rest, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("usage: lobslaw learned history <id>")
	}

	conn, err := node.dial()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := node.ctx()
	defer cancel()
	res, err := lobslawv1.NewSelfLearningServiceClient(conn).ListArtefactHistory(ctx,
		&lobslawv1.ListArtefactHistoryRequest{Id: rest[0]})
	if err != nil {
		return err
	}
	return renderLearnedHistory(os.Stdout, res.GetCurrent(), res.GetHistory(), node.addr)
}

// liveRollback puts a prior version back in force.
//
// Dry run by default, like the offline form — and the preview comes
// from the server, so what it describes is what the write would do
// rather than what a second read of a different store suggests.
func liveRollback(args []string) error {
	fs := flag.NewFlagSet("learned rollback", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	apply := fs.Bool("apply", false, "actually write (default is a dry run)")
	rest, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 2 {
		return fmt.Errorf("usage: lobslaw learned rollback <id> <version> [--apply]")
	}
	version, err := strconv.ParseUint(rest[1], 10, 32)
	if err != nil {
		return fmt.Errorf("version must be a number: %w", err)
	}

	conn, err := node.dial()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := node.ctx()
	defer cancel()
	res, err := lobslawv1.NewSelfLearningServiceClient(conn).RollbackArtefact(ctx,
		&lobslawv1.RollbackArtefactRequest{
			Id:      rest[0],
			Version: uint32(version), //nolint:gosec // bounded by ParseUint above
			Apply:   *apply,
		})
	if err != nil {
		return err
	}
	rec := res.GetArtefact()
	if !res.GetApplied() {
		fmt.Printf("DRY RUN — would restore %s to v%d: %s\n",
			rest[0], rec.GetVersion(), firstLine(rec.GetDescription()))
		fmt.Printf("Re-run with --apply to write it.\n")
		return nil
	}
	fmt.Printf("restored %s to v%d (now v%d)\n", rest[0], version, rec.GetVersion())
	return nil
}
