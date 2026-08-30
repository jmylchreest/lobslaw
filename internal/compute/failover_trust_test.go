package compute

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// The failover machinery that makes the assistant resilient was the
// same machinery that quietly lowered the trust floor: the backup
// chain walked label to label with no notion of a tier, so a turn
// completed on a public provider the moment the primary 429'd.
//
// A floor checked at the first candidate and nowhere after is not a
// floor. It has a hole exactly where the interesting case lives,
// because the whole point of a backup is that it runs when something
// has gone wrong.

// tieredAgent wires primary → backup with declared tiers and a soul
// floor.
func tieredAgent(t *testing.T, floor types.TrustTier, primaryTier, backupTier types.TrustTier, failWith error) (*Agent, *[]string) {
	t.Helper()
	calls := &[]string{}
	reg := NewProviderRegistry()
	reg.Register(ProviderEntry{
		Label: "primary", TrustTier: primaryTier, Backup: "backup",
		Client: &scriptedProvider{label: "primary", err: failWith, calls: calls},
	})
	reg.Register(ProviderEntry{
		Label: "backup", TrustTier: backupTier,
		Client: &scriptedProvider{label: "backup", calls: calls},
	})
	a := &Agent{cfg: AgentConfig{
		Provider:     &scriptedProvider{label: "unused", calls: calls},
		Providers:    reg,
		PrimaryLabel: "primary",
		Health:       NewProviderHealth(),
		Soul:         func() *types.SoulConfig { return &types.SoulConfig{MinTrustTier: floor} },
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}
	return a, calls
}

// The bug, pinned. A private-floor deployment whose primary is rate
// limited must NOT complete on a public backup.
func TestAPublicBackupCannotRescueAPrivateFloorTurn(t *testing.T) {
	t.Parallel()
	a, calls := tieredAgent(t, types.TrustPrivate,
		types.TrustPrivate, types.TrustPublic, Transient(errors.New("429")))

	_, err := a.dispatchWithBackup(context.Background(), ChatRequest{})
	if err == nil {
		t.Fatal("the turn succeeded on a below-floor backup")
	}
	for _, c := range *calls {
		if c == "backup" {
			t.Errorf("the public backup was called: %v", *calls)
		}
	}
}

// And the floor does not break failover when the backup qualifies.
func TestAQualifyingBackupStillRescuesTheTurn(t *testing.T) {
	t.Parallel()
	a, calls := tieredAgent(t, types.TrustPrivate,
		types.TrustPrivate, types.TrustLocal, Transient(errors.New("429")))

	resp, err := a.dispatchWithBackup(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatalf("a qualifying backup was refused: %v", err)
	}
	if resp.resp.Content != "ok from backup" {
		t.Errorf("content = %q", resp.resp.Content)
	}
	if len(*calls) != 2 {
		t.Errorf("calls = %v", *calls)
	}
}

// No floor configured is every deployment before this change, and it
// must behave exactly as it did.
func TestWithNoFloorTheChainIsUnchanged(t *testing.T) {
	t.Parallel()
	a, calls := tieredAgent(t, types.TrustUnset,
		types.TrustPublic, types.TrustPublic, Transient(errors.New("429")))

	if _, err := a.dispatchWithBackup(context.Background(), ChatRequest{}); err != nil {
		t.Fatalf("an unconfigured deployment broke: %v", err)
	}
	if len(*calls) != 2 {
		t.Errorf("calls = %v", *calls)
	}
}

// The floor beating the chain is not an outage, and the error has to
// say so — an operator sent to the logs looking for a provider problem
// would find a healthy one.
func TestTheFloorBeatingTheChainIsItsOwnError(t *testing.T) {
	t.Parallel()
	a, _ := tieredAgent(t, types.TrustLocal,
		types.TrustPublic, types.TrustPublic, nil)

	_, err := a.dispatchWithBackup(context.Background(), ChatRequest{})
	var floorErr *ErrBelowTrustFloor
	if !errors.As(err, &floorErr) {
		t.Fatalf("err = %v, want ErrBelowTrustFloor", err)
	}
	if !strings.Contains(err.Error(), "primary(public)") {
		t.Errorf("err = %q; it does not name what was considered", err)
	}
}

// A provider excluded by the floor is not "demoted" — health is about
// providers that failed, and conflating the two would put a healthy
// provider into a cooldown it can never leave.
func TestTheFloorDoesNotDemoteAProvider(t *testing.T) {
	t.Parallel()
	a, _ := tieredAgent(t, types.TrustPrivate,
		types.TrustPrivate, types.TrustPublic, Transient(errors.New("429")))
	_, _ = a.dispatchWithBackup(context.Background(), ChatRequest{})

	if !a.cfg.Health.Available("backup") {
		t.Error("a provider excluded by the floor was marked unhealthy")
	}
}

// --- the modality path ---------------------------------------------

func okHandler(label string, calls *[]string) BuiltinFunc {
	return func(context.Context, map[string]string) ([]byte, int, error) {
		*calls = append(*calls, label)
		return []byte("ok from " + label), 0, nil
	}
}

func failHandler(label string, calls *[]string, err error) BuiltinFunc {
	return func(context.Context, map[string]string) ([]byte, int, error) {
		*calls = append(*calls, label)
		return nil, 1, err
	}
}

// A modality provider is not a lesser recipient of content — a vision
// provider is handed the user's image, and a speak provider the text
// of the reply.
func TestAModalityChainHonoursTheFloor(t *testing.T) {
	t.Parallel()
	calls := &[]string{}
	fn := FailoverBuiltin("read_image", slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewProviderHealth(),
		func() types.TrustTier { return types.TrustPrivate },
		FailoverHandler{Label: "a", Tier: types.TrustPrivate,
			Fn: failHandler("a", calls, Transient(errors.New("429")))},
		FailoverHandler{Label: "b", Tier: types.TrustPublic, Fn: okHandler("b", calls)},
	)

	_, _, err := fn(context.Background(), nil)
	if err == nil {
		t.Fatal("the modality completed on a below-floor provider")
	}
	for _, c := range *calls {
		if c == "b" {
			t.Errorf("the public provider was called: %v", *calls)
		}
	}
}

// The single-provider case used to skip the wrapper entirely, which is
// exactly the config where an unchecked provider is the only thing
// that runs.
func TestASingleModalityProviderIsStillChecked(t *testing.T) {
	t.Parallel()
	calls := &[]string{}
	fn := FailoverBuiltin("speak", slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewProviderHealth(),
		func() types.TrustTier { return types.TrustLocal },
		FailoverHandler{Label: "only", Tier: types.TrustPublic, Fn: okHandler("only", calls)},
	)

	if _, _, err := fn(context.Background(), nil); err == nil {
		t.Fatal("a lone below-floor provider ran")
	}
	if len(*calls) != 0 {
		t.Errorf("it was called anyway: %v", *calls)
	}
}

func TestASingleQualifyingModalityProviderStillRuns(t *testing.T) {
	t.Parallel()
	calls := &[]string{}
	fn := FailoverBuiltin("speak", slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewProviderHealth(),
		func() types.TrustTier { return types.TrustLocal },
		FailoverHandler{Label: "only", Tier: types.TrustLocal, Fn: okHandler("only", calls)},
	)

	out, _, err := fn(context.Background(), nil)
	if err != nil {
		t.Fatalf("a qualifying provider was refused: %v", err)
	}
	if string(out) != "ok from only" {
		t.Errorf("out = %q", out)
	}
}

// A nil accessor is what a node with no SOUL.md passes, and it must
// permit everything rather than nothing.
func TestANilFloorAccessorPermitsEverything(t *testing.T) {
	t.Parallel()
	calls := &[]string{}
	fn := FailoverBuiltin("speak", slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewProviderHealth(), nil,
		FailoverHandler{Label: "only", Tier: types.TrustPublic, Fn: okHandler("only", calls)},
	)
	if _, _, err := fn(context.Background(), nil); err != nil {
		t.Errorf("a nil accessor refused a provider: %v", err)
	}
}
