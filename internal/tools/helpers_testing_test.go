package tools

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Test fixtures copied from the compute package, whose own test
// helpers are unexported and therefore unreachable from here.

func newImageDriver(t *testing.T, endpoint string, preferURL bool) *compute.OpenAIImageDriver {
	t.Helper()
	d, err := compute.NewOpenAIImageDriver(compute.OpenAIImageConfig{
		Endpoint:   endpoint,
		Model:      "gpt-image-1",
		Credential: compute.NewBearerCredential("k"),
		PreferURL:  preferURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// fakeMounts resolves one label to a temp dir.
type fakeMounts struct{ label, root string }

func (f fakeMounts) MountRoot(label string) (string, bool) {
	if label == f.label {
		return f.root, true
	}
	return "", false
}

// mkBudget is a tiny helper so tests aren't littered with
// zero-value budget constructions.
func mkBudget(t *testing.T, caps compute.BudgetCaps) *compute.TurnBudget {
	t.Helper()
	b, err := compute.NewTurnBudget(caps)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func shellGatedExecutor(t *testing.T, rules ...*lobslawv1.PolicyRule) (*compute.Executor, *compute.SessionApprovals) {
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

	// The allow every node carries from wire_seeds.go. Its presence is
	// the point: the gate must not be satisfied by it.
	rules = append(rules, &lobslawv1.PolicyRule{
		Id: "lobslaw-builtin-shell_command", Subject: "*",
		Action: "tool:exec", Resource: "shell_command",
		Effect: "allow", Priority: 1,
	})
	for _, r := range rules {
		raw, err := proto.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(memory.BucketPolicyRules, r.Id, raw); err != nil {
			t.Fatal(err)
		}
	}

	eng := policy.NewEngine(store, slog.New(slog.DiscardHandler))
	eng.SetDefaults([]types.PolicyRule{compute.ShellApprovalDefault()})

	approvals := compute.NewSessionApprovals()
	e := compute.NewExecutor(NewRegistry(), eng, nil, compute.ExecutorConfig{}, slog.New(slog.DiscardHandler))
	e.SetSessionApprovals(approvals)
	e.RequireCommandApproval("shell_command", compute.ShellGrantResource, compute.ShellCommandSummary)
	return e, approvals
}

func checkShell(ctx context.Context, t *testing.T, e *compute.Executor, cmd string) error {
	t.Helper()
	return e.CheckGate(ctx, &types.Claims{UserID: "alice"}, "shell_command", shellParams(cmd))
}

func newSpeakDriver(t *testing.T, endpoint string) *compute.OpenAISpeakDriver {
	t.Helper()
	d, err := compute.NewOpenAISpeakDriver(compute.OpenAISpeakConfig{
		Endpoint:   endpoint,
		Model:      "tts-1",
		Voice:      "alloy",
		Credential: compute.NewBearerCredential("k"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func shellParams(cmd string) map[string]string {
	return map[string]string{"command": cmd}
}

func newResolver(t *testing.T) (*compute.ArtifactResolver, string) {
	t.Helper()
	root := t.TempDir()
	return &compute.ArtifactResolver{
		Mounts:       fakeMounts{label: "store", root: root},
		DefaultMount: "store",
	}, root
}

// mockPNG returns a 1x1 opaque PNG. Built literally rather than
// base64-decoded at init, so a typo is a compile error.
func mockPNG() []byte {
	return []byte{
		0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 'I', 'H', 'D', 'R',
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53, 0xDE,
		0x00, 0x00, 0x00, 0x0C, 'I', 'D', 'A', 'T',
		0x08, 0xD7, 0x63, 0xF8, 0xCF, 0xC0, 0x00, 0x00,
		0x03, 0x01, 0x01, 0x00, 0x18, 0xDD, 0x8D, 0xB0,
		0x00, 0x00, 0x00, 0x00, 'I', 'E', 'N', 'D',
		0xAE, 0x42, 0x60, 0x82,
	}
}
