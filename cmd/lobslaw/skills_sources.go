package main

import (
	"errors"
	"flag"
	"os"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/clawhub"
	"github.com/jmylchreest/lobslaw/internal/sharing"
	"github.com/jmylchreest/lobslaw/internal/skills"
	"github.com/jmylchreest/lobslaw/pkg/config"
)

func skillShareSource(node *liveNode, ref string) (sharing.Source, error) {
	if strings.HasPrefix(ref, "file:") {
		return sharing.FileBackend{}, nil
	}
	if !strings.HasPrefix(ref, "clawhub:") {
		return nil, errors.New("source must be file:<path> or clawhub:<slug> (optionally @version for native catalogues)")
	}
	cfg, err := config.Load(config.LoadOptions{Path: node.configPath})
	if err != nil && node.configPath != "" {
		return nil, err
	}
	base := strings.TrimSpace(os.Getenv("LOBSLAW_CLAWHUB_BASE_URL"))
	var verifier *skills.Verifier
	if cfg != nil {
		if base == "" {
			base = cfg.Security.ClawhubBaseURL
		}
		if cfg.Skills.TrustedPublishers != "" {
			verifier = skills.NewVerifier()
			if err := verifier.LoadTrustedPublishersFile(cfg.Skills.TrustedPublishers); err != nil {
				return nil, err
			}
		}
	}
	if base == "" {
		return nil, errors.New("configure security.clawhub_base_url or LOBSLAW_CLAWHUB_BASE_URL")
	}
	// Avoid a typed-nil verifier inside an interface.
	if verifier == nil {
		return clawhub.NewShareSource(base, nil)
	}
	return clawhub.NewShareSource(base, verifier)
}

func skillsFetch(args []string) error {
	fs := newFlagSet("skills fetch", flag.ContinueOnError)
	var node liveNode
	node.bind(fs)
	to := fs.String("to", "", "save exact retrieved artifact to a new file:<path>")
	rest, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 || *to == "" {
		return errors.New("supply a source and --to file:<path>")
	}
	source, err := skillShareSource(&node, rest[0])
	if err != nil {
		return err
	}
	ctx, cancel := node.ctx()
	defer cancel()
	a, err := source.Fetch(ctx, rest[0])
	if err != nil {
		return err
	}
	return publishShare(ctx, sharing.FileBackend{}, a, *to)
}
