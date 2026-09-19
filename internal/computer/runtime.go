package computer

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/internal/sandbox"
)

const maxRuntimeResponse int = 8 << 20
const browserWidth float64 = 1280
const browserHeight float64 = 800
const runtimeExitTimeout time.Duration = 3 * time.Second

//go:embed runtime.cjs
var runtimeScript string

// Config is intentionally explicit: a browser without a functioning network
// namespace and the cluster egress UDS has no permissive fallback.
type Config struct {
	Root         string
	Node         string
	Chromium     string
	Playwright   string
	IP           string
	ProxySocket  string
	ProxyAddress string
	ReadPaths    []string
}

type processBrowser struct {
	cmd    *exec.Cmd
	input  io.WriteCloser
	output *bufio.Scanner
	done   chan error
	once   sync.Once
	temp   string
	broker *roleBroker
}

func (cfg Config) openBrowser(ctx context.Context, path string) (browser, error) {
	return cfg.openProcess(ctx, path, runtimeScript)
}

func (cfg Config) openProcess(_ context.Context, path, script string) (browser, error) {
	if runtime.GOOS != "linux" || cfg.ProxySocket == "" || len(cfg.ReadPaths) == 0 {
		return nil, ErrUnavailable
	}
	upstream, err := filepath.EvalSymlinks(cfg.ProxySocket)
	if err != nil {
		return nil, fmt.Errorf("%w: egress socket is missing", ErrUnavailable)
	}
	cfg.ProxySocket = upstream
	upstreamDir := filepath.Dir(upstream)
	if upstreamDir == "/" || pathInside(upstreamDir, cfg.Root) {
		return nil, fmt.Errorf("%w: egress socket needs a dedicated directory outside the browser root", ErrUnavailable)
	}
	for _, p := range []string{cfg.Node, cfg.Chromium, cfg.Playwright, cfg.IP, cfg.ProxySocket} {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("%w: runtime paths must be absolute", ErrUnavailable)
		}
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("%w: required runtime path absent", ErrUnavailable)
		}
		if p != cfg.ProxySocket && pathInside(upstreamDir, p) {
			return nil, fmt.Errorf("%w: runtime paths must be outside the egress socket directory", ErrUnavailable)
		}
	}
	execution, err := executionDirectory(path)
	if err != nil {
		return nil, err
	}
	broker, err := newRoleBroker(cfg.ProxySocket, cfg.ProxyAddress)
	if err != nil {
		return nil, err
	}
	// Chromium's singleton socket is AF_UNIX, whose pathname limit is much
	// shorter than an owner/project profile path. A private, short temp root
	// avoids truncating the stable identity hash to fit that unrelated limit.
	temp, err := os.MkdirTemp(brokerTempParent(upstream), "lc-")
	if err != nil {
		_ = broker.Close()
		return nil, fmt.Errorf("computer temp: %w", err)
	}
	started := false
	defer func() {
		if !started {
			_ = broker.Close()
			_ = os.RemoveAll(temp)
		}
	}()
	cmd := exec.Command(cfg.Node, "-e", script)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + execution, "TMPDIR=" + temp, "COMPUTER_PROFILE=" + execution,
		"COMPUTER_CHROMIUM=" + cfg.Chromium, "COMPUTER_PLAYWRIGHT=" + cfg.Playwright, "COMPUTER_IP=" + cfg.IP, "COMPUTER_PROXY=" + broker.Path()}
	policy := &sandbox.Policy{HideDirs: []string{upstreamDir}, RequireLandlock: true, PrivateProc: true, NoNewPrivs: true, Namespaces: sandbox.NamespaceSet{User: true, Network: true, Mount: true, PID: true},
		Mounts: []sandbox.PolicyMount{{Path: execution, Read: true, Write: true}, {Path: temp, Read: true, Write: true}, {Path: "/dev/null", Read: true, Write: true}, {Path: "/dev/urandom", Read: true}, {Path: "/dev/random", Read: true}}}
	for _, p := range cfg.ReadPaths {
		if !filepath.IsAbs(p) || filepath.Clean(p) == "/" {
			return nil, fmt.Errorf("%w: read_paths must name narrow absolute runtime directories", ErrUnavailable)
		}
		if rel, err := filepath.Rel(p, cfg.Root); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("%w: read_paths must not expose other profiles", ErrUnavailable)
		}
		policy.Mounts = append(policy.Mounts, sandbox.PolicyMount{Path: p, Read: true, Exec: true})
	}
	policy.Mounts = append(policy.Mounts, sandbox.PolicyMount{Path: broker.Path(), Read: true})
	if err := sandbox.Apply(cmd, policy); err != nil {
		return nil, fmt.Errorf("computer sandbox: %w", err)
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("computer stdin: %w", err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		_ = in.Close()
		return nil, fmt.Errorf("computer stdout: %w", err)
	}
	// Never collect Playwright stderr: it can include user values or page URLs.
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		_ = in.Close()
		_ = out.Close()
		return nil, fmt.Errorf("computer start: %w", err)
	}
	scanner := bufio.NewScanner(out)
	scanner.Buffer(nil, maxRuntimeResponse)
	b := &processBrowser{cmd: cmd, input: in, output: scanner, done: make(chan error, 1), temp: temp, broker: broker}
	started = true
	go func() { b.done <- cmd.Wait() }()
	return b, nil
}

func executionDirectory(workspace string) (string, error) {
	dir := filepath.Join(workspace, "browser")
	if err := privateDirectory(dir); err != nil {
		return "", err
	}
	// Preserve pre-isolation profiles without granting the subprocess access to
	// their former parent, which also holds authoritative control metadata.
	for _, name := range []string{"profile", "location"} {
		source, target := filepath.Join(workspace, name), filepath.Join(dir, name)
		info, err := os.Lstat(source)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("computer legacy profile: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: legacy profile path must not be a symlink", ErrUnavailable)
		}
		if _, err := os.Lstat(target); err == nil {
			return "", fmt.Errorf("%w: both legacy and isolated browser profiles exist; reconcile before launch", ErrUnavailable)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("computer profile destination: %w", err)
		}
		if err := os.Rename(source, target); err != nil {
			return "", fmt.Errorf("computer isolate legacy profile: %w", err)
		}
	}
	return dir, nil
}

func (b *processBrowser) Do(ctx context.Context, step RoutineStep) (Result, error) {
	// Closing the process interrupts both a blocked pipe write and browser work.
	stop := context.AfterFunc(ctx, func() { _ = b.cmd.Process.Kill() })
	defer stop()
	if err := json.NewEncoder(b.input).Encode(struct {
		RoutineStep
		Automated bool `json:"automated"`
	}{RoutineStep: step, Automated: step.automated}); err != nil {
		return Result{}, fmt.Errorf("computer command: %w", err)
	}
	var response struct {
		OK     bool   `json:"ok"`
		Result Result `json:"result"`
		Code   string `json:"code"`
	}
	if !b.output.Scan() {
		return Result{}, ErrUnavailable
	}
	if err := json.Unmarshal(b.output.Bytes(), &response); err != nil {
		return Result{}, ErrUnavailable
	}
	if response.Code == "manual" {
		return Result{}, ErrManual
	}
	if !response.OK {
		return Result{}, ErrUnavailable
	}
	return response.Result, nil
}

func (b *processBrowser) Close() error {
	b.once.Do(func() {
		defer func() { _ = b.broker.Close() }()
		defer func() { _ = os.RemoveAll(b.temp) }()
		_ = b.input.Close()
		timer := time.NewTimer(runtimeExitTimeout)
		defer timer.Stop()
		select {
		case <-b.done:
		case <-timer.C:
			_ = b.cmd.Process.Kill()
			<-b.done
		}
	})
	return nil
}
