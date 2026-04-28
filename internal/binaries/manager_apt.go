package binaries

import (
	"context"
	"log/slog"
	"os/exec"
)

type aptManager struct{}

func (aptManager) Name() string { return "apt" }

func (aptManager) Hosts(spec InstallSpec) []string {
	hosts := []string{
		"deb.debian.org",
		"security.debian.org",
		"archive.ubuntu.com",
		"security.ubuntu.com",
		"keyserver.ubuntu.com",
	}
	if spec.Repo != "" {
		// best-effort: extract host from "deb https://host/..." form
		if h := hostFromDebLine(spec.Repo); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

func (aptManager) Available(ctx context.Context) bool {
	_, err := exec.LookPath("apt-get")
	return err == nil
}

func (m aptManager) Install(ctx context.Context, spec InstallSpec, runner ProcessRunner, log *slog.Logger) error {
	args := append([]string{"install", "-y", spec.Package}, spec.Args...)
	return runManagerCmd(ctx, runner, log, "apt-get", true, args)
}

// hostFromDebLine extracts the host from a "deb [opts] https://host/path suite components" line.
func hostFromDebLine(line string) string {
	// Skip "deb " and any [bracketed] options.
	rest := line
	if len(rest) >= 4 && rest[:4] == "deb " {
		rest = rest[4:]
	}
	// strip leading whitespace + bracket options
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
		rest = rest[1:]
	}
	if len(rest) > 0 && rest[0] == '[' {
		end := -1
		for i := 0; i < len(rest); i++ {
			if rest[i] == ']' {
				end = i
				break
			}
		}
		if end < 0 {
			return ""
		}
		rest = rest[end+1:]
	}
	for len(rest) > 0 && rest[0] == ' ' {
		rest = rest[1:]
	}
	// Now rest should start with the URL.
	end := len(rest)
	for i, c := range rest {
		if c == ' ' || c == '\t' {
			end = i
			break
		}
	}
	urlStr := rest[:end]
	for _, prefix := range []string{"https://", "http://"} {
		if len(urlStr) > len(prefix) && urlStr[:len(prefix)] == prefix {
			rest := urlStr[len(prefix):]
			for i, c := range rest {
				if c == '/' {
					return rest[:i]
				}
			}
			return rest
		}
	}
	return ""
}
