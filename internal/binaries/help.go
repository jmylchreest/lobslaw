package binaries

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// MaxHelpOutput caps the captured --help output. Most CLI help is
// well under this; truncating keeps the agent's context budget
// predictable.
const MaxHelpOutput = 6 << 10 // 6 KiB

// CaptureHelp runs the supplied helpCmd (default: "<name> --help"),
// captures combined stdout+stderr, truncates to MaxHelpOutput, and
// returns it. Errors are silenced — a non-zero exit is fine; many
// tools print their help to stderr and return non-zero. Empty
// output means "no help captured" and the caller skips persistence.
func CaptureHelp(ctx context.Context, binaryName, helpCmd string) string {
	if strings.TrimSpace(helpCmd) == "" {
		helpCmd = binaryName + " --help"
	}
	parts := strings.Fields(helpCmd)
	if len(parts) == 0 {
		return ""
	}
	cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
	out, _ := cmd.CombinedOutput()
	s := string(out)
	if len(s) > MaxHelpOutput {
		s = s[:MaxHelpOutput] + "\n…(truncated)\n"
	}
	return strings.TrimSpace(s)
}

// WriteHelp persists captured help text under the install prefix at
// <prefix>/share/lobslaw/<name>/help.txt. The directory is created
// lazily; an empty body removes any prior file.
func WriteHelp(prefix, name, body string) error {
	if prefix == "" {
		return errors.New("binaries: WriteHelp requires prefix")
	}
	if name == "" {
		return errors.New("binaries: WriteHelp requires name")
	}
	dir := filepath.Join(prefix, "share", "lobslaw", name)
	path := filepath.Join(dir, "help.txt")
	if strings.TrimSpace(body) == "" {
		_ = os.Remove(path)
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("binaries: WriteHelp mkdir: %w", err)
	}
	tmp := path + ".part"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		return fmt.Errorf("binaries: WriteHelp tempfile: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("binaries: WriteHelp rename: %w", err)
	}
	return nil
}

// ReadHelp returns the help text persisted by WriteHelp. Returns
// the empty string when no help has been captured (caller treats
// as "fall back to PostInstall prose only").
func ReadHelp(prefix, name string) string {
	if prefix == "" || name == "" {
		return ""
	}
	path := filepath.Join(prefix, "share", "lobslaw", name, "help.txt")
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}
