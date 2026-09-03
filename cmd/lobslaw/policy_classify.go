package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/commandrisk"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/config"
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
	withModel := fs.String("with-model", "",
		"also ask the [compute.roles] command_risk model, using this config file")
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
		commandrisk.SetScratchPaths(strings.Split(*scratch, ","))
	}

	// The static verdict is always shown on its own first: it is
	// reproducible, and a reader needs to see what the classifier knew
	// unaided before seeing what a model did to it.
	v := commandrisk.ClassifyRisk(command)

	// --with-model is the only way to exercise the model path without
	// going through a real confirmation. That matters more than it
	// sounds: every failure mode of that path is silent — a timeout, a
	// reply outside the enum, a low-confidence hedge — and all of them
	// look exactly like "the model agreed with the classifier".
	var modelErr error
	if *withModel != "" {
		v, modelErr = classifyWithModel(*withModel, command, v)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			Command  string                    `json:"command"`
			Labels   []commandrisk.RiskLabel   `json:"labels"`
			Why      string                    `json:"why,omitempty"`
			Headline string                    `json:"headline"`
			Scratch  []string                  `json:"scratch_paths"`
			Steps    []commandrisk.RiskSegment `json:"steps"`
			Modes    map[string]string         `json:"approval_modes"`
		}{
			Command:  command,
			Labels:   v.Labels,
			Why:      v.Why,
			Headline: commandrisk.RiskHeadline(v),
			Scratch:  commandrisk.ActiveScratchPaths(),
			Steps:    v.Segments,
			Modes:    modeOutcomes(v),
		})
	}

	if modelErr != nil {
		fmt.Fprintf(os.Stderr, "model verdict unavailable: %v\n\n", modelErr)
	}
	fmt.Println(commandrisk.RiskHeadline(v))
	if len(v.Segments) > 1 {
		fmt.Println()
		fmt.Println("steps:")
		for i, seg := range v.Segments {
			fmt.Printf("  %2d  %-28s %-20s %s\n",
				i+1, commandrisk.RenderLabels(seg.Labels), seg.Why, seg.Raw)
		}
	}
	fmt.Println()
	fmt.Println("under each approval mode:")
	outcomes := modeOutcomes(v)
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
	fmt.Println("scratch roots:", strings.Join(commandrisk.ActiveScratchPaths(), ", "))
	fmt.Println("(* is the shipped default; the hardline floor applies in every mode)")
	return nil
}

// classifyWithModel asks the configured command_risk model and folds
// its verdict in exactly as the running node would.
//
// It builds the one provider that role names rather than a whole node:
// the question is "does this role answer, in time, in the enum", and
// standing up raft to find out would make the answer depend on a dozen
// things that are not being asked about.
func classifyWithModel(configPath, command string, static commandrisk.RiskVerdict) (commandrisk.RiskVerdict, error) {
	cfg, err := config.Load(config.LoadOptions{Path: configPath})
	if err != nil {
		return static, err
	}
	role := cfg.Compute.Roles.CommandRisk
	if role.Provider == "" {
		return static, errors.New("[compute.roles] command_risk names no provider, so no model is asked")
	}
	var pc config.ProviderConfig
	found := false
	for _, p := range cfg.Compute.Providers {
		if p.Label == role.Provider {
			pc, found = p, true
			break
		}
	}
	if !found {
		return static, fmt.Errorf("command_risk names provider %q, which is not declared", role.Provider)
	}
	apiKey, err := config.ResolveSecret(pc.APIKeyRef)
	if err != nil {
		return static, fmt.Errorf("resolve %s: %w", pc.APIKeyRef, err)
	}
	client, err := compute.NewLLMClientFromProvider(pc, apiKey)
	if err != nil {
		return static, err
	}

	timeout := role.Timeout
	if timeout == 0 {
		timeout = cfg.Compute.ModelTimeout
	}
	trust, terr := compute.ParseRiskTrust(cfg.Compute.ShellApproval.VerdictTrust)
	if terr != nil {
		fmt.Fprintf(os.Stderr, "%v; using %q\n", terr, trust)
	}

	judge := compute.NewRiskJudge(client, pc.Model, trust, timeout, slog.New(slog.DiscardHandler))
	started := time.Now()
	out := compute.AdjudicateWith(context.Background(), static, command, judge)
	elapsed := time.Since(started)

	fmt.Fprintf(os.Stderr, "model: provider=%s model=%s trust=%s timeout=%s took=%s\n",
		role.Provider, pc.Model, trust, timeoutLabel(timeout), elapsed.Round(10*time.Millisecond))
	if !out.FromModel {
		// Said plainly, because this is the outcome that looks like
		// success and is not: the static verdict stands and nothing
		// says why unless somebody asks.
		fmt.Fprintln(os.Stderr,
			"model: verdict not used — it declined, timed out, answered outside the enum, "+
				"was low-confidence, or agreed with the classifier")
	}
	return out, nil
}

// timeoutLabel names the deadline in force, saying so when nothing was
// configured and the call site's own constant applies.
func timeoutLabel(d time.Duration) string {
	if d == 0 {
		return "(call-site default)"
	}
	return d.String()
}

// modeOutcomes says what each shipped preset would do with this
// verdict.
//
// Approval is a subset check, so the answer is simply whether every
// label the command carries is one that preset approves — no
// comparison, no ranking.
func modeOutcomes(v commandrisk.RiskVerdict) map[string]string {
	out := map[string]string{}
	for _, mode := range []compute.ApprovalMode{
		compute.ApprovalStrict, compute.ApprovalStandard, compute.ApprovalTrusted,
	} {
		approved, err := compute.ApprovedLabels([]string{string(mode)})
		if err != nil {
			continue
		}
		outcome := "asks"
		if v.Approved(approved) {
			outcome = "runs without asking"
		}
		out[string(mode)] = outcome
	}
	return out
}
