package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jmylchreest/lobslaw/internal/plugins"
)

// dispatchPlugin handles `lobslaw plugin <subcmd>`. Returns true if
// the invocation was a plugin subcommand (caller should exit);
// false falls through to the main agent.
func dispatchPlugin(args []string) bool {
	idx := findSubcmd(args, "plugin")
	if idx < 0 {
		return false
	}
	sub := args[idx+1:]
	if len(sub) == 0 {
		pluginUsage()
		os.Exit(2)
	}
	switch sub[0] {
	case "install":
		pluginInstall(sub[1:])
	case "uninstall", "remove":
		pluginUninstall(sub[1:])
	case "list", "ls":
		pluginList(sub[1:])
	case "enable":
		pluginEnable(sub[1:])
	case "disable":
		pluginDisable(sub[1:])
	case "import":
		pluginImport(sub[1:])
	case "help", "-h", "--help":
		pluginUsage()
	default:
		fmt.Fprintf(os.Stderr, "lobslaw plugin: unknown subcommand %q\n", sub[0])
		pluginUsage()
		os.Exit(2)
	}
	return true
}

func pluginUsage() {
	fmt.Fprintln(os.Stderr, "usage: lobslaw plugin <command> [args]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  install <source>     copy a plugin directory into the skills root")
	fmt.Fprintln(os.Stderr, "  uninstall <name>     remove an installed plugin")
	fmt.Fprintln(os.Stderr, "  list                 show installed plugins")
	fmt.Fprintln(os.Stderr, "  enable <name>        re-enable a previously disabled plugin")
	fmt.Fprintln(os.Stderr, "  disable <name>       mark a plugin disabled without uninstalling it")
	fmt.Fprintln(os.Stderr, "  import <claude-dir>  install from a Claude Code plugin directory")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "environment:")
	fmt.Fprintln(os.Stderr, "  LOBSLAW_SKILLS_ROOT  destination root (default: ~/.local/share/lobslaw/plugins)")
}

// defaultSkillsRoot returns the destination root for installed
// plugins. Operators override with LOBSLAW_SKILLS_ROOT; the
// default is an XDG-compliant user-space path so a single-user
// dev install works without sudo.
func defaultSkillsRoot() (string, error) {
	if v := os.Getenv("LOBSLAW_SKILLS_ROOT"); v != "" {
		return filepath.Abs(v)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	return filepath.Join(home, ".local", "share", "lobslaw", "plugins"), nil
}

func pluginInstall(args []string) {
	fs := flag.NewFlagSet("plugin install", flag.ExitOnError)
	root := fs.String("root", "", "skills root (default: $LOBSLAW_SKILLS_ROOT or ~/.local/share/lobslaw/plugins)")
	yes := fs.Bool("yes", false, "skip the approval prompt (CI / scripted use)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 1 {
		exitWith("plugin install: exactly one <source> argument required")
	}
	source := fs.Arg(0)

	if plugins.IsURLLikeSource(source) {
		exitWith(fmt.Sprintf("plugin install: %v", plugins.ErrUnsupportedSource))
	}

	absSource, err := filepath.Abs(source)
	if err != nil {
		exitWith(fmt.Sprintf("plugin install: resolve source: %v", err))
	}

	dstRoot := *root
	if dstRoot == "" {
		d, err := defaultSkillsRoot()
		if err != nil {
			exitWith(fmt.Sprintf("plugin install: %v", err))
		}
		dstRoot = d
	}
	if err := os.MkdirAll(dstRoot, 0o755); err != nil {
		exitWith(fmt.Sprintf("plugin install: mkdir root: %v", err))
	}

	if !*yes {
		if !approveInstall(absSource) {
			exitWith("plugin install: aborted by operator")
		}
	}

	p, err := plugins.Install(absSource, dstRoot)
	if err != nil {
		exitWith(fmt.Sprintf("plugin install: %v", err))
	}
	fmt.Printf("installed %s@%s at %s\n", p.Manifest.Name, p.Manifest.Version, p.Dir)
	fmt.Printf("  sha256: %s\n", p.SHA256)
}

// approveInstall prints a manifest tree summary and prompts Y/N/d
// (the "d" for details — print every file). Returns true on Y.
func approveInstall(source string) bool {
	fmt.Printf("About to install plugin from: %s\n", source)
	fmt.Println()
	printTree(source, 0, 30)
	fmt.Println()
	fmt.Print("Proceed? [y/N/d] ")
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	switch line {
	case "y", "yes":
		return true
	case "d", "details":
		fmt.Println()
		printTree(source, 0, 0) // unlimited
		fmt.Print("Proceed? [y/N] ")
		l2, _ := reader.ReadString('\n')
		l2 = strings.TrimSpace(strings.ToLower(l2))
		return l2 == "y" || l2 == "yes"
	default:
		return false
	}
}

// printTree writes a simple indented listing of the source tree.
// maxEntries caps output (0 = unlimited) so a massive plugin
// doesn't drown the operator in the approval prompt.
func printTree(root string, depth, maxEntries int) {
	shown := 0
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || path == root {
			return err
		}
		if maxEntries > 0 && shown >= maxEntries {
			if shown == maxEntries {
				fmt.Println("  ... (more files; use 'd' for full listing)")
				shown++
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		prefix := strings.Repeat("  ", strings.Count(rel, string(os.PathSeparator))+1)
		suffix := ""
		if info.IsDir() {
			suffix = "/"
		}
		fmt.Printf("%s%s%s\n", prefix, filepath.Base(path), suffix)
		shown++
		return nil
	})
}

func pluginUninstall(args []string) {
	if len(args) != 1 {
		exitWith("plugin uninstall: exactly one <name> argument required")
	}
	dstRoot, err := defaultSkillsRoot()
	if err != nil {
		exitWith(err.Error())
	}
	if err := plugins.Uninstall(dstRoot, args[0]); err != nil {
		exitWith(fmt.Sprintf("plugin uninstall: %v", err))
	}
	fmt.Printf("removed %s\n", args[0])
}

func pluginList(_ []string) {
	dstRoot, err := defaultSkillsRoot()
	if err != nil {
		exitWith(err.Error())
	}
	list, err := plugins.List(dstRoot)
	if err != nil {
		exitWith(fmt.Sprintf("plugin list: %v", err))
	}
	if len(list) == 0 {
		fmt.Println("no plugins installed")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 2, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tVERSION\tSTATUS\tINSTALLED\tSHA256")
	for _, p := range list {
		status := "enabled"
		if !p.Enabled {
			status = "disabled"
		}
		installed := p.InstalledAt.Format(time.RFC3339)
		sha := p.SHA256
		if len(sha) > 12 {
			sha = sha[:12] // truncate for readability
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			p.Manifest.Name, p.Manifest.Version, status, installed, sha)
	}
	_ = w.Flush()
}

func pluginEnable(args []string) {
	if len(args) != 1 {
		exitWith("plugin enable: exactly one <name> argument required")
	}
	dstRoot, err := defaultSkillsRoot()
	if err != nil {
		exitWith(err.Error())
	}
	if err := plugins.Enable(dstRoot, args[0]); err != nil {
		exitWith(fmt.Sprintf("plugin enable: %v", err))
	}
	fmt.Printf("enabled %s\n", args[0])
}

func pluginDisable(args []string) {
	if len(args) != 1 {
		exitWith("plugin disable: exactly one <name> argument required")
	}
	dstRoot, err := defaultSkillsRoot()
	if err != nil {
		exitWith(err.Error())
	}
	if err := plugins.Disable(dstRoot, args[0]); err != nil {
		exitWith(fmt.Sprintf("plugin disable: %v", err))
	}
	fmt.Printf("disabled %s\n", args[0])
}

// pluginImport installs from a Claude Code plugin directory. Format
// differences: Claude Code uses a top-level plugin.json (JSON) while
// lobslaw uses plugin.yaml. Import reads either and emits a
// lobslaw-shape plugin.yaml on the destination side so the plugin
// becomes a first-class lobslaw install regardless of origin.
// MVP: only supports directories already in plugin.yaml form — the
// plugin.json conversion path is deferred but the CLI surface is
// reserved so operators can script around it now.
func pluginImport(args []string) {
	fs := flag.NewFlagSet("plugin import", flag.ExitOnError)
	yes := fs.Bool("yes", false, "skip the approval prompt")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 1 {
		exitWith("plugin import: exactly one <claude-dir> argument required")
	}
	source, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		exitWith(fmt.Sprintf("plugin import: resolve source: %v", err))
	}

	// Claude Code plugin dirs have plugin.json at the root. If only
	// plugin.json is present and no plugin.yaml, reject with a
	// helpful message pointing at the deferred conversion.
	_, yamlErr := os.Stat(filepath.Join(source, plugins.ManifestFile))
	_, jsonErr := os.Stat(filepath.Join(source, "plugin.json"))
	switch {
	case yamlErr == nil:
		// Already a lobslaw-shape plugin. Install as-is.
		pluginInstall(append([]string{}, yesFlagArg(*yes), source))
	case jsonErr == nil:
		exitWith("plugin import: plugin.json → plugin.yaml conversion is not yet implemented; " +
			"manually add a plugin.yaml alongside plugin.json and re-run")
	default:
		exitWith(fmt.Sprintf("plugin import: neither plugin.yaml nor plugin.json found under %q", source))
	}
}

// yesFlagArg emits --yes only when y is true — avoids passing a
// literal "false" that flag.Parse would misinterpret.
func yesFlagArg(y bool) string {
	if y {
		return "--yes"
	}
	return "--"
}

// silence unused-error import warnings if future edits trim paths.
var _ = errors.New
