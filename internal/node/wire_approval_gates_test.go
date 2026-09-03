package node

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/tools"

	"github.com/jmylchreest/lobslaw/internal/commandrisk"
	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Engine.SetDefaults replaces rather than appends.
//
// So two gates each installing their own default — the memory write
// staging and the per-command shell approval — meant whichever wired
// second silently wiped the first. The symptom is a gate that never
// asks, which is indistinguishable from a gate that is working, and
// the only thing that would have shown it is a test that turns both on
// at once. This is that test.

func approvalGateNode(t *testing.T, memoryWrite, withShell bool) (*Node, *policy.Engine) {
	t.Helper()
	dir := t.TempDir()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(dir, "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	log := slog.New(slog.DiscardHandler)
	eng := policy.NewEngine(store, log)
	reg := tools.NewRegistry()
	if withShell {
		if err := reg.Register(tools.ShellToolDef()); err != nil {
			t.Fatal(err)
		}
	}

	n := &Node{}
	n.log = log
	n.policyEngine = eng
	n.toolRegistry = reg
	n.executor = compute.NewExecutor(reg, eng, nil, compute.ExecutorConfig{}, log)
	n.cfg.MemoryWriteApproval = memoryWrite
	return n, eng
}

func effectFor(t *testing.T, eng *policy.Engine, action, resource string) types.Effect {
	t.Helper()
	dec, err := eng.Evaluate(context.Background(), &types.Claims{UserID: "alice"}, action, resource)
	if err != nil {
		t.Fatal(err)
	}
	return dec.Effect
}

// Both gates on. Before the fix the second SetDefaults call dropped
// the first gate's rule, so one of these two was silently ungated.
func TestBothDefaultsSurviveWiring(t *testing.T) {
	t.Parallel()
	n, eng := approvalGateNode(t, true, true)

	if err := n.wireApprovalGates(); err != nil {
		t.Fatal(err)
	}

	if got := effectFor(t, eng, compute.MemoryWriteAction, "episodic"); got != types.EffectRequireConfirmation {
		t.Errorf("memory writes are not staged: effect = %v", got)
	}
	if got := effectFor(t, eng, compute.ShellAction, "git status"); got != types.EffectRequireConfirmation {
		t.Errorf("shell commands are not asked about: effect = %v", got)
	}
}

// Off means ABSENT: no rule at all, rather than a rule that always
// passes. A deployment that never opted in should carry no extra
// check.
func TestOffMeansAbsent(t *testing.T) {
	t.Parallel()
	n, eng := approvalGateNode(t, false, true)

	if err := n.wireApprovalGates(); err != nil {
		t.Fatal(err)
	}

	// Default-deny, because nothing matched — not require_confirmation
	// from a default that should not exist.
	if got := effectFor(t, eng, compute.MemoryWriteAction, "episodic"); got == types.EffectRequireConfirmation {
		t.Error("a memory write default was installed for a node that did not ask for one")
	}
	if got := effectFor(t, eng, compute.ShellAction, "git status"); got != types.EffectRequireConfirmation {
		t.Errorf("the shell gate was collateral damage: effect = %v", got)
	}
}

// The shell gate follows the tool. A node that never registered
// shell_command should not carry a rule about it.
func TestNoShellGateWithoutTheTool(t *testing.T) {
	t.Parallel()
	n, eng := approvalGateNode(t, true, false)

	if err := n.wireApprovalGates(); err != nil {
		t.Fatal(err)
	}

	if got := effectFor(t, eng, compute.ShellAction, "git status"); got == types.EffectRequireConfirmation {
		t.Error("a shell rule was installed on a node with no shell tool")
	}
	if got := effectFor(t, eng, compute.MemoryWriteAction, "episodic"); got != types.EffectRequireConfirmation {
		t.Errorf("the memory gate was collateral damage: effect = %v", got)
	}
}

// A node with neither gate must not crash and must not leave a
// previous boot's defaults standing.
func TestNeitherGateIsSafe(t *testing.T) {
	t.Parallel()
	n, eng := approvalGateNode(t, false, false)

	if err := n.wireApprovalGates(); err != nil {
		t.Fatal(err)
	}
	if got := effectFor(t, eng, compute.ShellAction, "git status"); got == types.EffectRequireConfirmation {
		t.Error("a shell default survived a node that wired neither gate")
	}
}

// A command class that reaches off the box is also a network command
// to the classifier, and config says it once.
//
// This regressed twice while the package was being extracted, both
// times invisibly: applyCommandRisks took the derived rules as a
// parameter and never read it, which Go compiles without complaint.
// Every other test still passed, because the symptom is a command that
// classifies as "unreadable" — which asks, and so looks exactly like
// the gate working. The two sources have to arrive in ONE
// SetCommandRisks call, since it rebuilds from the shipped catalogue on
// every call and the second would drop the first.
func TestDeclaredNetworkCommandsAreLabelledNetwork(t *testing.T) {
	t.Cleanup(func() {
		commandrisk.SetCommandRisks(nil)
		compute.SetCommandClasses(nil)
	})

	n, _ := approvalGateNode(t, false, true)
	n.cfg.Compute.CommandClasses = map[string]config.CommandClassConfig{
		"mytool": {Action: string(compute.NetFetchAction)},
	}
	// Deliberately no [compute.command_risks]: the derived rules used to
	// be discarded entirely unless config also carried an unrelated
	// section.
	n.applyCommandRisks(n.applyCommandClasses())

	if got := commandrisk.ClassifyRisk("mytool https://example.com").Labels; !commandrisk.HasLabel(got, commandrisk.LabelNetwork) {
		t.Errorf("declared net:fetch command classified %v, want network", got)
	}
	// The shipped catalogue must survive the merge.
	if got := commandrisk.ClassifyRisk("ls -la").Labels; !commandrisk.HasLabel(got, commandrisk.LabelReads) {
		t.Errorf("ls classified %v after merge, want reads; the shipped table was lost", got)
	}
}
