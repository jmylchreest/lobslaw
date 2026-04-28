package binaries

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// MaxScriptSize caps a curl-sh install script at 10 MiB. Anything
// larger is almost certainly hostile or wrong.
const MaxScriptSize = 10 << 20

type curlShManager struct {
	client *http.Client
}

// NewCurlShManager returns a curl-sh manager wired to the given
// HTTP client. The client should be the egress-aware client for
// the "binaries-install" role so the script download flows through
// smokescreen.
func NewCurlShManager(client *http.Client) Manager {
	if client == nil {
		client = http.DefaultClient
	}
	return curlShManager{client: client}
}

func (curlShManager) Name() string { return "curl-sh" }

func (curlShManager) Hosts(spec InstallSpec) []string {
	if spec.URL == "" {
		return nil
	}
	u, err := url.Parse(spec.URL)
	if err != nil {
		return nil
	}
	return []string{u.Hostname()}
}

func (curlShManager) Available(_ context.Context) bool {
	// curl-sh runs the script via /bin/sh. /bin/sh is universally
	// present on every supported OS.
	_, err := os.Stat("/bin/sh")
	return err == nil
}

func (m curlShManager) Install(ctx context.Context, spec InstallSpec, runner ProcessRunner, log *slog.Logger) error {
	if spec.URL == "" {
		return errors.New("curl-sh: url required")
	}
	if !strings.HasPrefix(spec.Checksum, "sha256:") {
		return errors.New("curl-sh: checksum must be sha256:<hex>")
	}
	expected := strings.TrimPrefix(spec.Checksum, "sha256:")

	log.Info("binaries: curl-sh fetch", "url", spec.URL)
	body, err := m.fetch(ctx, spec.URL)
	if err != nil {
		return fmt.Errorf("curl-sh: fetch: %w", err)
	}

	got := sha256.Sum256(body)
	gotHex := hex.EncodeToString(got[:])
	if gotHex != expected {
		return fmt.Errorf("curl-sh: checksum mismatch: got sha256:%s, want %s", gotHex, spec.Checksum)
	}

	tmp, err := os.CreateTemp("", "lobslaw-binstall-*.sh")
	if err != nil {
		return fmt.Errorf("curl-sh: tempfile: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("curl-sh: write tempfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("curl-sh: close tempfile: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o700); err != nil {
		return fmt.Errorf("curl-sh: chmod tempfile: %w", err)
	}

	args := append([]string{tmp.Name()}, spec.Args...)
	log.Info("binaries: curl-sh exec", "tmp", tmp.Name())
	if spec.Sudo {
		if err := ensureSudoAllowed(ctx, runner); err != nil {
			return err
		}
		out, err := runner.Run(ctx, "sudo", append([]string{"-n", "/bin/sh"}, args...), nil)
		if err != nil {
			return fmt.Errorf("curl-sh: sudo sh: %w (output: %s)", err, truncate(out, 512))
		}
		log.Info("binaries: curl-sh ok", "output", truncate(out, 256))
		return nil
	}
	out, err := runner.Run(ctx, "/bin/sh", args, nil)
	if err != nil {
		return fmt.Errorf("curl-sh: sh: %w (output: %s)", err, truncate(out, 512))
	}
	log.Info("binaries: curl-sh ok", "output", truncate(out, 256))
	return nil
}

func (m curlShManager) fetch(ctx context.Context, urlStr string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, MaxScriptSize+1))
}
