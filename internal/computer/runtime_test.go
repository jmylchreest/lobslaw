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
	socket := filepath.Join(t.TempDir(), "egress.sock")
	proxy, err := egress.NewSmokescreenProvider(egress.SmokescreenConfig{UDSPath: socket, ACL: egress.Rules{Roles: map[string][]string{"computer": {"127.0.0.1"}, "fetch_url": {"localhost"}}}, AllowRanges: []string{"127.0.0.0/8"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proxy.Stop(context.Background()) }()
	cfg := Config{Root: filepath.Join(root, "private"), Node: node, Chromium: os.Getenv("COMPUTER_TEST_CHROMIUM"), Playwright: os.Getenv("COMPUTER_TEST_PLAYWRIGHT"), IP: "/usr/sbin/ip", ProxySocket: socket,
		ProxyAddress: proxy.ProxyURL().Host,
		ReadPaths:    []string{"/usr", "/lib", "/lib64", "/etc/fonts", "/etc/ssl", "/etc/ld.so.cache", "/proc", "/sys", filepath.Dir(node), filepath.Dir(os.Getenv("COMPUTER_TEST_CHROMIUM")), filepath.Dir(os.Getenv("COMPUTER_TEST_PLAYWRIGHT"))}}
	s := New(cfg, ownerAuth{})
	s.open = func(ctx context.Context, path string) (browser, error) {
		// Execute native attacks from the real sandbox, before Chromium starts.
		// A JavaScript route hook cannot provide this containment guarantee.
		probe := fmt.Sprintf(`
  try {
  try { fs.writeFileSync(%q, '{"control":"bot"}'); throw new Error('host metadata writable'); }
  catch (err) { if (!['EACCES','EPERM'].includes(err.code)) throw err; }
  try { if (fs.statSync(%q).isSocket()) throw new Error('shared socket was not masked'); }
  catch (err) { if (!['ENOENT','EACCES'].includes(err.code)) throw err; }
  await new Promise((resolve,reject) => {
    const socket=net.connect(%q); const timer=setTimeout(()=>{socket.destroy();reject(new Error('shared socket probe timeout'));},1000);
    socket.on('connect',()=>{clearTimeout(timer);socket.destroy();reject(new Error('shared egress socket exposed'));});
    socket.on('error',err=>{clearTimeout(timer);['ENOENT','ENOTSOCK','ECONNREFUSED','EACCES'].includes(err.code)?resolve():reject(err);});
  });
  await new Promise((resolve,reject)=>{
    const req=http.request({socketPath:process.env.COMPUTER_PROXY,path:%q,headers:{'X-Lobslaw-Role':'fetch_url','Proxy-Authorization':'Basic '+Buffer.from('fetch_url:_').toString('base64')}},res=>{
      res.resume();res.on('end',()=>res.statusCode===200?reject(new Error('forged role escaped')):resolve());
    });req.on('error',reject);req.end();
  });
  } catch(err) { fs.writeFileSync(path.join(process.env.COMPUTER_PROFILE,'probe-error'),String(err.code || err.message));throw err; }
`, filepath.Join(path, "control.json"), socket, socket, strings.Replace(fixture.URL, "127.0.0.1", "localhost", 1))
		return cfg.openProcess(ctx, path, strings.Replace(runtimeScript, "async function main() {", "async function main() {"+probe, 1))
	}
	defer func() { _ = s.Close() }()
	x, y := 250.0, 35.0
	for _, step := range []RoutineStep{{Action: "takeover"}, {Action: "navigate", URL: fixture.URL}, {Action: "record"}, {Action: "fill", Selector: "#password", Value: "test-secret"}, {Action: "click", X: &x, Y: &y}, {Action: "capture"}} {
		result, err := s.Action(t.Context(), "user:alice", "project", step)
		if err != nil {
			for _, space := range s.spaces {
				diagnostic, _ := os.ReadFile(filepath.Join(space.path, "browser", "probe-error"))
				if len(diagnostic) > 0 {
					t.Logf("native containment probe: %s", diagnostic)
				}
			}
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
