package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// Seeing what the classifier thinks, without a chat window.
//
// A classifier whose only surface is a Telegram confirmation is one
// nobody exercises: to find out how `rm -rf $DIR` is read you would
// have to get an agent to run it. That is a bad enough loop that the
// answer becomes "trust it", which is not a property this particular
// component may have — its verdict decides what runs without anybody
// being asked.
//
// So the verdict is a command. It reads no state, reaches no cluster,
// and runs nothing: it takes a command line as TEXT and prints what
// would happen to it.

// policyLocalOnly are subcommands that answer from compiled-in
// knowledge alone.
//
// A third category beside live and offline, and a small one on
// purpose. The reach rule exists because a command that quietly reads
// a laptop-local state.db reports an empty cluster as fact; a command
// that reads NO state cannot make that mistake, and calling it
// "offline" would announce a local answer that has no local source.
var policyLocalOnly = map[string]func([]string) error{
	"classify": policyClassify,
}

// policyClassify prints what the classifier makes of a command line.
func policyClassify(args []string) error {
	fs := flag.NewFlagSet("policy classify", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit JSON")
	scratch := fs.String("scratch", "",
		"comma-separated scratch roots, as [compute.shell_approval] scratch_paths would set")
	// parseFlagsAndPositionals rather than fs.Parse: a command line is
	// the positional here, and `classify 'rm -rf /' --json` must not
	// read --json as part of the command being classified.
	positional, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	command := strings.TrimSpace(strings.Join(positional, " "))
	if command == "" {
		return fmt.Errorf("usage: lobslaw policy classify [--json] [--scratch /a,/b] '<command>'")
	}
	if *scratch != "" {
		compute.SetScratchPaths(strings.Split(*scratch, ","))
	}

	// The static verdict only. No model is consulted even when one is
	// configured: this command exists to show what the classifier
	// knows on its own, and a verdict that silently depended on a
	// provider being reachable would not be reproducible.
	v := compute.ClassifyRisk(command)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			Command  string                `json:"command"`
			Tier     compute.CommandRisk   `json:"tier"`
			Reason   string                `json:"reason"`
			Headline string                `json:"headline"`
			Scratch  []string              `json:"scratch_paths"`
			Steps    []compute.RiskSegment `json:"steps"`
			Modes    map[string]string     `json:"approval_modes"`
		}{
			Command:  command,
			Tier:     v.Tier,
			Reason:   v.Reason,
			Headline: compute.RiskHeadline(v),
			Scratch:  compute.ActiveScratchPaths(),
			Steps:    v.Segments,
			Modes:    modeOutcomes(v.Tier),
		})
	}

	fmt.Println(compute.RiskHeadline(v))
	if len(v.Segments) > 1 {
		fmt.Println()
		fmt.Println("steps:")
		for i, s := range v.Segments {
			fmt.Printf("  %2d  %-12s %-34s %s\n", i+1, s.Tier, s.Reason, s.Raw)
		}
	}
	fmt.Println()
	fmt.Println("under each approval mode:")
	outcomes := modeOutcomes(v.Tier)
	for _, mode := range []compute.ApprovalMode{
		compute.ApprovalStrict, compute.ApprovalStandard, compute.ApprovalTrusted,
	} {
		marker := " "
		if mode == compute.DefaultApprovalMode {
			marker = "*"
		}
		fmt.Printf("  %s %-9s %s\n", marker, mode, outcomes[string(mode)])
	}
	fmt.Println()
	fmt.Println("scratch roots:", strings.Join(compute.ActiveScratchPaths(), ", "))
	fmt.Println("(* is the shipped default; the hardline floor applies in every mode)")
	return nil
}

// modeOutcomes says what each mode would do with this tier.
func modeOutcomes(tier compute.CommandRisk) map[string]string {
	out := map[string]string{}
	for _, mode := range []compute.ApprovalMode{
		compute.ApprovalStrict, compute.ApprovalStandard, compute.ApprovalTrusted,
	} {
		outcome := "asks"
		for _, allowed := range mode.AutoAllowed() {
			if allowed == tier {
				outcome = "runs without asking"
				break
			}
		}
		out[string(mode)] = outcome
	}
	return out
}
