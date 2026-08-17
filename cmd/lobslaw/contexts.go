package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	koanftoml "github.com/knadh/koanf/parsers/toml/v2"
	koanffile "github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// Named clusters, so an operator does not type four flags per command.
//
// THE NODE'S config.toml IS NOT THIS FILE. It is the node's: full of
// paths that exist on the node and mean nothing on a laptop, and it
// describes one cluster because a node belongs to one. An operator has
// several and belongs to none of them.
//
// So this is a separate file in the operator's own config directory,
// holding only what is needed to REACH a cluster: where it is, and
// what to present.

// ContextsFile is the operator's cluster list.
type ContextsFile struct {
	// Default names the context used when none is given. Empty means
	// there is no default, which is a reasonable thing to want when an
	// operator has both a staging and a production cluster — a bare
	// command should not reach either by accident.
	Default  string             `koanf:"default"`
	Contexts map[string]Context `koanf:"contexts"`
}

// Context is one cluster an operator can reach.
type Context struct {
	Addr   string `koanf:"addr"`
	CACert string `koanf:"ca_cert"`
	Cert   string `koanf:"cert"`
	Key    string `koanf:"key"`
}

// contextsPath resolves the operator's context file, following the
// same chain as every other operator-side file: the explicit env var,
// then XDG, then the conventional home directory.
func contextsPath() string {
	if v := strings.TrimSpace(os.Getenv("LOBSLAW_CONTEXTS")); v != "" {
		return v
	}
	return filepath.Join(defaultInitDir(), "contexts.toml")
}

// loadContexts reads the file. A missing one is not an error — most
// installations never have it, and the flags still work.
func loadContexts() (*ContextsFile, error) {
	path := contextsPath()
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return &ContextsFile{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	k := koanf.New(".")
	if err := k.Load(koanffile.Provider(path), koanftoml.Parser()); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var out ContextsFile
	if err := k.UnmarshalWithConf("", &out, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &out, nil
}

// resolveContext returns the named context, or the default when no
// name was given. An empty name and no default yields a zero Context
// and no error: having no contexts at all is the ordinary case.
//
// An unknown name is an error NAMING WHAT EXISTS. An operator who
// typed "prod" when the context is called "production" needs the list,
// and the alternative — quietly falling back to the default — would
// run the command against a cluster they did not name, which is the
// one outcome worth preventing here.
func resolveContext(name string) (Context, error) {
	file, err := loadContexts()
	if err != nil {
		return Context{}, err
	}
	if name == "" {
		name = file.Default
	}
	if name == "" {
		return Context{}, nil
	}
	ctx, ok := file.Contexts[name]
	if !ok {
		return Context{}, fmt.Errorf(
			"no context named %q in %s; defined: %s",
			name, contextsPath(), strings.Join(contextNames(file), ", "))
	}
	return expandContext(ctx), nil
}

// expandContext resolves a leading ~ in the credential paths, because
// a context file is written by hand and "~/.config/..." is what
// somebody types. A literal "~" directory is not what they meant, and
// the failure would be a confusing "no such file" naming a path that
// looks right.
func expandContext(c Context) Context {
	c.CACert = expandHome(c.CACert)
	c.Cert = expandHome(c.Cert)
	c.Key = expandHome(c.Key)
	return c
}

func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[2:])
}

func contextNames(file *ContextsFile) []string {
	if len(file.Contexts) == 0 {
		return []string{"(none)"}
	}
	out := make([]string, 0, len(file.Contexts))
	for name := range file.Contexts {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// --- the subcommand ----------------------------------------------------

const contextUsage = `lobslaw context — the clusters this machine can reach

  list    show the configured contexts

Contexts live in contexts.toml in your lobslaw config directory
(override with LOBSLAW_CONTEXTS). Each names one cluster:

  default = "prod"

  [contexts.prod]
  addr    = "node1.example.com:9090"
  ca_cert = "~/.config/lobslaw/prod/ca.pem"
  cert    = "~/.config/lobslaw/prod/operator.pem"
  key     = "~/.config/lobslaw/prod/operator-key.pem"

Use one with --context, or set LOBSLAW_CONTEXT.`

func dispatchContext(args []string) bool {
	idx := findSubcmd(args, "context")
	if idx < 0 {
		return false
	}
	sub := args[idx+1:]
	if len(sub) > 0 && sub[0] != "list" {
		fmt.Fprintf(os.Stderr, "unknown context subcommand %q\n\n%s\n", sub[0], contextUsage)
		os.Exit(2)
	}
	if err := contextList(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "context list: %v\n", err)
		os.Exit(1)
	}
	return true
}

func contextList(w io.Writer) error {
	file, err := loadContexts()
	if err != nil {
		return err
	}
	if len(file.Contexts) == 0 {
		_, _ = fmt.Fprintf(w, "no contexts configured (%s)\n\n%s\n", contextsPath(), contextUsage)
		return nil
	}
	for _, name := range contextNames(file) {
		c := expandContext(file.Contexts[name])
		marker := " "
		if name == file.Default {
			marker = "*"
		}
		_, _ = fmt.Fprintf(w, "%s %-16s %s\n", marker, name, c.Addr)
		// Each credential is reported present or MISSING rather than
		// merely listed: a context whose key was left behind on another
		// machine fails at dial time with an error about a file, and
		// this listing is where somebody looks first.
		for _, f := range []struct{ label, path string }{
			{"ca", c.CACert}, {"cert", c.Cert}, {"key", c.Key},
		} {
			state := "ok"
			if f.path == "" {
				state = "not set"
			} else if _, err := os.Stat(f.path); err != nil {
				state = "MISSING"
			}
			_, _ = fmt.Fprintf(w, "      %-4s %s (%s)\n", f.label, f.path, state)
		}
	}
	if file.Default == "" {
		_, _ = fmt.Fprintln(w, "\nNo default context: pass --context, or set `default = \"...\"`.")
	}
	return nil
}
