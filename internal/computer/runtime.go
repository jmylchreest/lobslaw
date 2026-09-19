package computer

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
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
	Root        string
	Node        string
	Chromium    string
	Playwright  string
	IP          string
	ProxySocket string
	ReadPaths   []string
}

type processBrowser struct {
	cmd    *exec.Cmd
	input  io.WriteCloser
	output *bufio.Scanner
	done   chan error
	once   sync.Once
	temp   string
}

func (cfg Config) openBrowser(_ context.Context, path string) (browser, error) {
	if runtime.GOOS != "linux" || cfg.ProxySocket == "" || len(cfg.ReadPaths) == 0 {
		return nil, ErrUnavailable
	}
	for _, p := range []string{cfg.Node, cfg.Chromium, cfg.Playwright, cfg.IP, cfg.ProxySocket} {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("%w: runtime paths must be absolute", ErrUnavailable)
		}
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("%w: required runtime path absent", ErrUnavailable)
		}
	}
	// Chromium's singleton socket is AF_UNIX, whose pathname limit is much
	// shorter than an owner/project profile path. A private, short temp root
	// avoids truncating the stable identity hash to fit that unrelated limit.
	temp, err := os.MkdirTemp("/tmp", "lc-")
	if err != nil {
		return nil, fmt.Errorf("computer temp: %w", err)
	}
	started := false
	defer func() {
		if !started {
			_ = os.RemoveAll(temp)
		}
	}()
	cmd := exec.Command(cfg.Node, "-e", runtimeScript)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + path, "TMPDIR=" + temp, "COMPUTER_PROFILE=" + path,
		"COMPUTER_CHROMIUM=" + cfg.Chromium, "COMPUTER_PLAYWRIGHT=" + cfg.Playwright, "COMPUTER_IP=" + cfg.IP, "COMPUTER_PROXY=" + cfg.ProxySocket}
	policy := &sandbox.Policy{RequireLandlock: true, PrivateProc: true, NoNewPrivs: true, Namespaces: sandbox.NamespaceSet{User: true, Network: true, Mount: true, PID: true},
		Mounts: []sandbox.PolicyMount{{Path: path, Read: true, Write: true}, {Path: temp, Read: true, Write: true}, {Path: "/dev/null", Read: true, Write: true}, {Path: "/dev/urandom", Read: true}, {Path: "/dev/random", Read: true}}}
	for _, p := range cfg.ReadPaths {
		if !filepath.IsAbs(p) || filepath.Clean(p) == "/" {
			return nil, fmt.Errorf("%w: read_paths must name narrow absolute runtime directories", ErrUnavailable)
		}
		if rel, err := filepath.Rel(p, cfg.Root); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("%w: read_paths must not expose other profiles", ErrUnavailable)
		}
		policy.Mounts = append(policy.Mounts, sandbox.PolicyMount{Path: p, Read: true, Exec: true})
	}
	// Access to the Unix socket does not grant a route out of the netns. The
	// helper's bridge injects the same role as egress.For("computer").
	policy.Mounts = append(policy.Mounts, sandbox.PolicyMount{Path: cfg.ProxySocket, Read: true, Write: true})
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
	b := &processBrowser{cmd: cmd, input: in, output: scanner, done: make(chan error, 1), temp: temp}
	started = true
	go func() { b.done <- cmd.Wait() }()
	return b, nil
}

func (b *processBrowser) Do(ctx context.Context, step RoutineStep) (Result, error) {
	// Closing the process interrupts both a blocked pipe write and browser work.
	stop := context.AfterFunc(ctx, func() { _ = b.cmd.Process.Kill() })
	defer stop()
	if err := json.NewEncoder(b.input).Encode(step); err != nil {
		return Result{}, fmt.Errorf("computer command: %w", err)
	}
	var response struct {
		OK     bool   `json:"ok"`
		Result Result `json:"result"`
	}
	if !b.output.Scan() {
		return Result{}, ErrUnavailable
	}
	if err := json.Unmarshal(b.output.Bytes(), &response); err != nil || !response.OK {
		return Result{}, ErrUnavailable
	}
	return response.Result, nil
}

func (b *processBrowser) Close() error {
	b.once.Do(func() {
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
