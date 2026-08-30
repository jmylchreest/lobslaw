package tools

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// R22 asks that generate_image can be gated by
// effect = "require_confirmation" WITH NO NEW MACHINERY.
//
// That claim is only worth anything if something checks it. A builtin
// reaches the executor's policy gate as action "tool:exec" and its own
// name as the resource, so an ordinary rule covers it — but "ordinary
// rule covers it" is exactly the sort of thing that is true until a
// dispatch path is added that skips the gate.
//
// The sharp part is WHEN. A confirmation that fires after the image
// exists has already spent the money, and asking afterwards is asking
// about a bill rather than about a decision.

// countingImageDriver records whether it was ever asked to generate.
type countingImageDriver struct{ calls int }

func (d *countingImageDriver) Generate(context.Context, compute.ImageRequest) (*compute.Artifact, error) {
	d.calls++
	return &compute.Artifact{Kind: compute.ArtifactInline, Bytes: mockPNG(), MIME: "image/png"}, nil
}

func seedRule(t *testing.T, store *memory.Store, id, resource, effect string, priority int32) {
	t.Helper()
	rule := &lobslawv1.PolicyRule{
		Id: id, Subject: "*", Action: "tool:exec", Resource: resource,
		Effect: effect, Priority: priority,
	}
	raw, err := proto.Marshal(rule)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(memory.BucketPolicyRules, rule.Id, raw); err != nil {
		t.Fatal(err)
	}
}

// registerImageTool makes generate_image reachable through the
// executor, the way the node does.
func registerImageTool(t *testing.T, env *agentEnv, d compute.ImageDriver) {
	t.Helper()
	b := NewBuiltins()
	if err := RegisterImageBuiltin(b, ImageConfig{
		Driver:   d,
		Resolver: billingResolver(t),
		Label:    "picture-vendor",
	}); err != nil {
		t.Fatal(err)
	}
	env.executor.SetBuiltins(b)
	if err := env.reg.Register(ImageToolDef()); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateImageIsGatedByAnOrdinaryPolicyRule(t *testing.T) {
	t.Parallel()
	env := newAgentEnv(t)
	d := &countingImageDriver{}
	registerImageTool(t, env, d)

	// An ordinary rule. No image-specific machinery, no new effect,
	// no special case in the builtin.
	seedRule(t, env.store, "confirm-images", "generate_image", "require_confirmation", 100)

	_, err := env.executor.Invoke(context.Background(), compute.InvokeRequest{
		ToolName: "generate_image",
		Params:   map[string]string{"prompt": "a cat"},
		Claims:   &types.Claims{UserID: "alice"},
	})
	if !errors.Is(err, compute.ErrRequireConfirm) {
		t.Fatalf("err = %v, want compute.ErrRequireConfirm", err)
	}

	// BEFORE the money is spent. A confirmation that fires after the
	// image exists is asking about a bill, not about a decision.
	if d.calls != 0 {
		t.Errorf("the provider was called %d times before the user confirmed", d.calls)
	}
}

// The gate must not fire when it was not asked for, or every
// deployment pays a prompt per picture and learns to click through.
func TestGenerateImageIsNotGatedByDefault(t *testing.T) {
	t.Parallel()
	env := newAgentEnv(t)
	d := &countingImageDriver{}
	registerImageTool(t, env, d)

	if _, err := env.executor.Invoke(context.Background(), compute.InvokeRequest{
		ToolName: "generate_image",
		Params:   map[string]string{"prompt": "a cat"},
		Claims:   &types.Claims{UserID: "alice"},
	}); err != nil {
		t.Fatalf("an ungated generate_image failed: %v", err)
	}
	if d.calls != 1 {
		t.Errorf("the provider was called %d times, want 1", d.calls)
	}
}

// A deny rule stops it outright, and must be distinguishable from a
// confirmation — one is "ask me", the other is "never".
func TestGenerateImageCanBeDeniedOutright(t *testing.T) {
	t.Parallel()
	env := newAgentEnv(t)
	d := &countingImageDriver{}
	registerImageTool(t, env, d)
	seedRule(t, env.store, "no-images", "generate_image", "deny", 100)

	_, err := env.executor.Invoke(context.Background(), compute.InvokeRequest{
		ToolName: "generate_image",
		Params:   map[string]string{"prompt": "a cat"},
		Claims:   &types.Claims{UserID: "alice"},
	})
	if !errors.Is(err, compute.ErrPolicyDenied) {
		t.Fatalf("err = %v, want compute.ErrPolicyDenied", err)
	}
	if errors.Is(err, compute.ErrRequireConfirm) {
		t.Error("a deny was reported as a confirmation; the user would be offered a choice they do not have")
	}
	if d.calls != 0 {
		t.Errorf("the provider was called %d times despite a deny", d.calls)
	}
}

// Gating one modality must not gate the others. An operator worried
// about image spend has said nothing about speech.
func TestGatingImagesLeavesOtherModalitiesAlone(t *testing.T) {
	t.Parallel()
	env := newAgentEnv(t)
	registerImageTool(t, env, &countingImageDriver{})

	b := NewBuiltins()
	sd, _ := compute.MockSpeakFactory(compute.SpeakDriverConfig{})
	if err := RegisterSpeakBuiltin(b, SpeakConfig{
		Driver: sd, Resolver: billingResolver(t), Label: "voice-vendor",
	}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterImageBuiltin(b, ImageConfig{
		Driver: &countingImageDriver{}, Resolver: billingResolver(t), Label: "picture-vendor",
	}); err != nil {
		t.Fatal(err)
	}
	env.executor.SetBuiltins(b)
	if err := env.reg.Register(SpeakToolDef()); err != nil {
		t.Fatal(err)
	}
	seedRule(t, env.store, "confirm-images", "generate_image", "require_confirmation", 100)

	if _, err := env.executor.Invoke(context.Background(), compute.InvokeRequest{
		ToolName: "speak",
		Params:   map[string]string{"text": "hello"},
		Claims:   &types.Claims{UserID: "alice"},
	}); err != nil {
		t.Errorf("gating generate_image also gated speak: %v", err)
	}
}
