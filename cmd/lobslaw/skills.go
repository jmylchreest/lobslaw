package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Installing skills on a running cluster.
//
// The BYTES travel, not a path. This runs on somebody's laptop and the
// cluster is elsewhere, so the directory is read HERE and the bundle
// is sent — a command that passed a path would be asking the node to
// read a directory that exists perfectly well on the wrong machine.
//
// Live only. There is no --offline form, unlike `lobslaw learned`:
// importing writes to raft and replicates, and doing it against a
// stopped node's state.db would produce a record the running cluster
// never sees. `learned approve` refuses --offline for the same reason.

const skillsUsage = `lobslaw skills — install and inspect skills on a RUNNING cluster

  list [--all]                    installed skills; --all includes superseded versions
  import <dir> [--tier=operator]  read a skill directory and install it
  export <name> <version> <dir>   write a stored skill back out, byte-identical
  remove <name> <version>         remove one version
  rollback <name> <version>       make a stored version the one in force

Connection comes from --config ([cluster] advertise_addr and
[cluster.mtls]), or from --addr / --ca-cert / --node-cert / --node-key.

There is no --offline form. Importing writes to raft; doing it against
a stopped node would produce a record the running cluster never sees.

A skill dropped into the skills storage mount is imported automatically
— this is for installing from anywhere else.`

func dispatchSkills(args []string) bool {
	idx := findSubcmd(args, "skills")
	if idx < 0 {
		return false
	}
	sub := args[idx+1:]
	if len(sub) == 0 {
		fmt.Fprintln(os.Stderr, skillsUsage)
		os.Exit(2)
	}

	var err error
	switch sub[0] {
	case "list":
		err = skillsList(sub[1:])
	case "import":
		err = skillsImport(sub[1:])
	case "export":
		err = skillsExport(sub[1:])
	case "remove":
		err = skillsRemove(sub[1:])
	case "rollback":
		err = skillsRollback(sub[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown skills subcommand %q\n\n%s\n", sub[0], skillsUsage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "skills %s: %v\n", sub[0], err)
		os.Exit(1)
	}
	return true
}

// skillClient dials and returns the client plus a close func.
func skillClient(node *liveNode) (lobslawv1.SkillServiceClient, func(), error) {
	conn, err := node.dial()
	if err != nil {
		return nil, nil, err
	}
	return lobslawv1.NewSkillServiceClient(conn), func() { _ = conn.Close() }, nil
}

func skillsList(args []string) error {
	fs := flag.NewFlagSet("skills list", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	all := fs.Bool("all", false, "include superseded versions")
	if err := fs.Parse(args); err != nil {
		return err
	}
	client, closeConn, err := skillClient(&node)
	if err != nil {
		return err
	}
	defer closeConn()

	ctx, cancel := node.ctx()
	defer cancel()
	resp, err := client.ListSkills(ctx, &lobslawv1.ListSkillsRequest{ActiveOnly: !*all})
	if err != nil {
		return explainUnimplemented(err, node.addr)
	}
	if len(resp.GetSkills()) == 0 {
		fmt.Println("no skills installed")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tVERSION\tTIER\tACTIVE\tFILES\tSOURCE")
	for _, s := range resp.GetSkills() {
		active := ""
		if s.GetActive() {
			active = "yes"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n",
			s.GetName(), s.GetVersion(), tierLabel(s.GetTier()), active,
			len(s.GetFiles()), s.GetSource())
	}
	return w.Flush()
}

func skillsImport(args []string) error {
	fs := flag.NewFlagSet("skills import", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	tier := fs.String("tier", "operator", "operator | signed")
	noActivate := fs.Bool("no-activate", false, "store the version without putting it in force")
	rest, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("import: exactly one directory is required\n\n%s", skillsUsage)
	}
	dir, err := filepath.Abs(rest[0])
	if err != nil {
		return err
	}

	// Read here, on the client. The bytes travel.
	bundle, err := memory.ReadBundle(dir)
	if err != nil {
		return err
	}
	name, version, err := manifestIdentity(bundle.Manifest)
	if err != nil {
		return err
	}
	parsedTier, err := parseTier(*tier)
	if err != nil {
		return err
	}
	// A --tier=signed import with no signature would be stored at a
	// tier its contents cannot support, and would then fail to verify
	// wherever signing is enforced. Caught here rather than at load.
	if parsedTier == lobslawv1.SkillTier_SKILL_TIER_SIGNED && len(bundle.Signature) == 0 {
		return fmt.Errorf("--tier=signed but %s carries no %s",
			dir, memory.SignatureFile)
	}

	client, closeConn, err := skillClient(&node)
	if err != nil {
		return err
	}
	defer closeConn()

	who, err := approverName("")
	if err != nil {
		// Not fatal: attribution is useful, but refusing an install
		// because the OS user is unreadable would be ceremony.
		who = "cli"
	}
	ctx, cancel := node.ctx()
	defer cancel()
	resp, err := client.ImportSkill(ctx, &lobslawv1.ImportSkillRequest{
		Name: name, Version: version, Tier: parsedTier,
		ManifestYaml: bundle.Manifest,
		ManifestSig:  bundle.Signature,
		Files:        bundle.Files,
		Source:       "cli:" + dir,
		ImportedBy:   who,
		Activate:     !*noActivate,
	})
	if err != nil {
		return err
	}
	rec := resp.GetSkill()
	fmt.Printf("installed %s %s (%s, %d files)\n",
		rec.GetName(), rec.GetVersion(), tierLabel(rec.GetTier()), len(rec.GetFiles()))
	if *noActivate {
		fmt.Println("stored but NOT in force — it will not load until activated")
	}
	return nil
}

func skillsExport(args []string) error {
	fs := flag.NewFlagSet("skills export", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	positional, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	rest := positional
	if len(rest) != 3 {
		return fmt.Errorf("export: need a name, a version and a directory\n\n%s", skillsUsage)
	}
	name, version, dir := rest[0], rest[1], rest[2]

	client, closeConn, err := skillClient(&node)
	if err != nil {
		return err
	}
	defer closeConn()

	ctx, cancel := node.ctx()
	defer cancel()
	resp, err := client.ExportSkill(ctx, &lobslawv1.ExportSkillRequest{Name: name, Version: version})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Written verbatim. An export whose manifest differs by a byte from
	// what was imported is one whose signature no longer verifies, and
	// the whole point of storing the bytes untouched was to make this
	// round trip safe.
	if err := os.WriteFile(filepath.Join(dir, memory.ManifestFile), resp.GetManifestYaml(), 0o600); err != nil {
		return err
	}
	if len(resp.GetManifestSig()) > 0 {
		if err := os.WriteFile(filepath.Join(dir, memory.SignatureFile), resp.GetManifestSig(), 0o600); err != nil {
			return err
		}
	}
	for rel, content := range resp.GetFiles() {
		cleaned := filepath.Clean(filepath.FromSlash(rel))
		if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
			// The server checks this too. Checked again here because a
			// client writing to its own filesystem on the say-so of a
			// remote response is exactly where a path traversal lands.
			return fmt.Errorf("refusing to write %q: it is outside %s", rel, dir)
		}
		dest := filepath.Join(dir, cleaned)
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(dest, content, 0o600); err != nil {
			return err
		}
	}
	fmt.Printf("exported %s %s to %s (%d files)\n", name, version, dir, len(resp.GetFiles()))
	return nil
}

func skillsRemove(args []string) error {
	fs := flag.NewFlagSet("skills remove", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	positional, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	rest := positional
	if len(rest) != 2 {
		return fmt.Errorf("remove: need a name and a version\n\n%s", skillsUsage)
	}

	client, closeConn, err := skillClient(&node)
	if err != nil {
		return err
	}
	defer closeConn()

	ctx, cancel := node.ctx()
	defer cancel()
	if _, err := client.RemoveSkill(ctx, &lobslawv1.RemoveSkillRequest{
		Name: rest[0], Version: rest[1],
	}); err != nil {
		return err
	}
	fmt.Printf("removed %s %s\n", rest[0], rest[1])
	return nil
}

// manifestIdentity pulls the name and version out of a manifest.
//
// Read from the manifest rather than taken as flags, because they are
// already stated there and two sources for one fact eventually
// disagree — an operator who typed a version that did not match the
// file would install a record describing a skill that is not the one
// in it.
//
// Parsed by hand rather than with a YAML decoder: this is the CLI, the
// bytes must not be round-tripped anywhere near this path, and two
// top-level scalars do not justify pulling the schema in.
func manifestIdentity(manifest []byte) (name, version string, err error) {
	for line := range strings.SplitSeq(string(manifest), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, found := strings.Cut(trimmed, ":")
		if !found || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			// Indented lines belong to a nested block; a nested "name"
			// is a credential provider or a binary, not the skill.
			continue
		}
		value = strings.TrimSpace(strings.Trim(strings.TrimSpace(value), `"'`))
		switch strings.TrimSpace(key) {
		case "name":
			if name == "" {
				name = value
			}
		case "version":
			if version == "" {
				version = value
			}
		}
	}
	if name == "" || version == "" {
		return "", "", fmt.Errorf("the manifest does not declare both a name and a version")
	}
	return name, version, nil
}

func parseTier(s string) (lobslawv1.SkillTier, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "operator":
		return lobslawv1.SkillTier_SKILL_TIER_OPERATOR, nil
	case "signed":
		return lobslawv1.SkillTier_SKILL_TIER_SIGNED, nil
	default:
		// agent is deliberately not accepted. That tier means "the
		// agent wrote this", and letting a person claim it from a
		// command line would make provenance a thing anybody can
		// assert rather than a fact about where a skill came from.
		return 0, fmt.Errorf("tier %q is not installable (operator, signed)", s)
	}
}

func tierLabel(t lobslawv1.SkillTier) string {
	switch t {
	case lobslawv1.SkillTier_SKILL_TIER_SIGNED:
		return "signed"
	case lobslawv1.SkillTier_SKILL_TIER_OPERATOR:
		return "operator"
	case lobslawv1.SkillTier_SKILL_TIER_AGENT:
		return "agent"
	default:
		return "?"
	}
}

// skillsRollback makes a stored version the one in force.
//
// A rollback needs no bundle and no directory: every version ever
// imported is still in the log, so going back to one is a matter of
// saying which. `lobslaw skills list --all` is where to find the
// versions that are not currently in force.
func skillsRollback(args []string) error {
	fs := flag.NewFlagSet("skills rollback", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	positional, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	rest := positional
	if len(rest) != 2 {
		return fmt.Errorf("rollback: need a name and a version\n\n%s", skillsUsage)
	}

	client, closeConn, err := skillClient(&node)
	if err != nil {
		return err
	}
	defer closeConn()

	ctx, cancel := node.ctx()
	defer cancel()
	resp, err := client.ActivateSkill(ctx, &lobslawv1.ActivateSkillRequest{
		Name: rest[0], Version: rest[1],
	})
	if err != nil {
		return err
	}
	if resp.GetAlreadyActive() {
		fmt.Printf("%s %s was already in force\n", rest[0], rest[1])
		return nil
	}
	rec := resp.GetSkill()
	fmt.Printf("rolled back to %s %s (%s)\n",
		rec.GetName(), rec.GetVersion(), tierLabel(rec.GetTier()))
	return nil
}
