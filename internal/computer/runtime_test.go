//go:build linux

package computer

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/internal/sandbox"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 3 && os.Args[1] == sandbox.HelperSubcommand {
		policy, err := sandbox.DecodePolicy(os.Getenv(sandbox.PolicyEnvVar))
		if err == nil {
			_ = os.Unsetenv(sandbox.PolicyEnvVar)
			err = sandbox.InstallAndExec(policy, os.Args[3], os.Args[3:], os.Environ())
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// Opt-in locally, with no external sites or credentials. This exercises the
// production sandbox, the real egress UDS, Chromium, and a restart of its profile.
func TestChromiumPersistentProfile(t *testing.T) {
	node := os.Getenv("COMPUTER_TEST_NODE")
	if node == "" {
		t.Skip("set COMPUTER_TEST_NODE, COMPUTER_TEST_CHROMIUM and COMPUTER_TEST_PLAYWRIGHT for real browser QA")
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, `<title>Computer fixture</title><style>#password{position:absolute;left:20px;top:20px;width:180px}#save{position:absolute;left:220px;top:20px;width:80px;height:30px}</style><input id="password" type="password"><button id="save" onclick="localStorage.setItem('saved','yes');document.cookie='saved=yes;max-age=3600'">Save</button><script>if(localStorage.getItem('saved')==='yes' && document.cookie.includes('saved=yes')) document.write('<p id="persisted">Profile survived</p>')</script>`)
	}))
	defer fixture.Close()
	root := t.TempDir()
	socket := filepath.Join(root, "egress.sock")
	proxy, err := egress.NewSmokescreenProvider(egress.SmokescreenConfig{UDSPath: socket, ACL: egress.Rules{Roles: map[string][]string{"computer": {"127.0.0.1"}}}, AllowRanges: []string{"127.0.0.0/8"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proxy.Stop(context.Background()) }()
	cfg := Config{Root: filepath.Join(root, "private"), Node: node, Chromium: os.Getenv("COMPUTER_TEST_CHROMIUM"), Playwright: os.Getenv("COMPUTER_TEST_PLAYWRIGHT"), IP: "/usr/sbin/ip", ProxySocket: socket,
		ReadPaths: []string{"/usr", "/lib", "/lib64", "/etc/fonts", "/etc/ssl", "/etc/ld.so.cache", "/proc", "/sys", filepath.Dir(node), filepath.Dir(os.Getenv("COMPUTER_TEST_CHROMIUM")), filepath.Dir(os.Getenv("COMPUTER_TEST_PLAYWRIGHT"))}}
	s := New(cfg, ownerAuth{})
	defer func() { _ = s.Close() }()
	x, y := 250.0, 35.0
	for _, step := range []RoutineStep{{Action: "takeover"}, {Action: "navigate", URL: fixture.URL}, {Action: "record"}, {Action: "fill", Selector: "#password", Value: "test-secret"}, {Action: "click", X: &x, Y: &y}, {Action: "capture"}} {
		result, err := s.Action(t.Context(), "user:alice", "project", step)
		if err != nil {
			t.Fatalf("%s: %v", step.Action, err)
		}
		if step.Action == "capture" {
			frame, err := base64.StdEncoding.DecodeString(result.Screenshot)
			if err != nil || len(frame) < 8 || string(frame[1:4]) != "PNG" {
				t.Fatal("not an actual PNG screenshot")
			}
		}
	}
	state, err := s.State(t.Context(), "user:alice", "project")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Steps) != 2 || state.Steps[0].Value != "" || !state.Steps[0].Sensitive {
		t.Fatalf("unsafe steps: %#v", state.Steps)
	}
	if state.Steps[1].Selector == "" || state.Steps[1].X != nil || state.Steps[1].Sensitive {
		t.Fatalf("coordinate click not recorded as replayable selector: %+v", state.Steps[1])
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = New(cfg, ownerAuth{})
	if _, err := s.Action(t.Context(), "user:alice", "project", RoutineStep{Action: "wait", Selector: "#persisted"}); err != nil {
		t.Fatalf("profile restart: %v", err)
	}
	if _, err := s.Action(t.Context(), "user:alice", "project", RoutineStep{Action: "navigate", URL: strings.Replace(fixture.URL, "127.0.0.1", "localhost", 1)}); err == nil {
		t.Fatal("browser bypassed computer hostname ACL")
	}
	// The profile is local only, while the saved draft is value-free.
	if err := filepath.WalkDir(cfg.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, "control.json") {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(string(raw), "test-secret") {
				t.Error("secret stored in recording")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
