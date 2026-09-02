package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A role can be written two ways, and the bare string is not a legacy
// shape being tolerated on the way out — it is the right way to write a
// role that has nothing to say beyond which provider serves it.
//
// The failure this guards against is total: supplying a DecoderConfig
// REPLACES koanf's default, and its default is what parses every
// duration and every TrustTier in the file. Getting that wrong does not
// error, it silently stops parsing values the config is full of.

func loadTOML(t *testing.T, body string) *Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(LoadOptions{Path: path, SkipEnv: true})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg
}

func TestRoleAcceptsBareStringAndTable(t *testing.T) {
	t.Parallel()
	cfg := loadTOML(t, `
[compute]
model_timeout = "60s"

[compute.roles]
main         = "qwen-flash"
preflight    = "qwen-flash"
summariser   = { provider = "qwen-plus", timeout = "90s" }
command_risk = { provider = "qwen-flash", timeout = "15s" }
review       = { provider = "qwen-max", timeout = "5m" }
`)
	r := cfg.Compute.Roles

	// The bare string is the whole value, and carries no timeout.
	if r.Main.Provider != "qwen-flash" || r.Main.Timeout != 0 {
		t.Errorf("main = %+v, want provider qwen-flash and no timeout", r.Main)
	}
	if r.Preflight.Provider != "qwen-flash" {
		t.Errorf("preflight provider = %q, want qwen-flash", r.Preflight.Provider)
	}

	// The table form carries both.
	if r.Summariser.Provider != "qwen-plus" || r.Summariser.Timeout != 90*time.Second {
		t.Errorf("summariser = %+v, want qwen-plus/90s", r.Summariser)
	}
	if r.CommandRisk.Provider != "qwen-flash" || r.CommandRisk.Timeout != 15*time.Second {
		t.Errorf("command_risk = %+v, want qwen-flash/15s", r.CommandRisk)
	}
	if r.Review.Provider != "qwen-max" || r.Review.Timeout != 5*time.Minute {
		t.Errorf("review = %+v, want qwen-max/5m", r.Review)
	}
	if cfg.Compute.ModelTimeout != 60*time.Second {
		t.Errorf("model_timeout = %v, want 60s", cfg.Compute.ModelTimeout)
	}
}

// Every config in existence writes roles as bare strings. A change that
// added an optional field by breaking all of them would be a bad trade,
// so this is the compatibility case stated on its own.
func TestRoleStringFormStillLoads(t *testing.T) {
	t.Parallel()
	cfg := loadTOML(t, `
[compute.roles]
main       = "qwen-flash"
preflight  = "qwen-flash"
summariser = "qwen-plus"
`)
	r := cfg.Compute.Roles
	for _, tc := range []struct{ got, want string }{
		{r.Main.Provider, "qwen-flash"},
		{r.Preflight.Provider, "qwen-flash"},
		{r.Summariser.Provider, "qwen-plus"},
	} {
		if tc.got != tc.want {
			t.Errorf("provider = %q, want %q", tc.got, tc.want)
		}
	}
	// Unset roles are the zero value, not an error.
	if r.CommandRisk.Provider != "" || r.Review.Provider != "" {
		t.Errorf("unset roles are populated: %+v %+v", r.CommandRisk, r.Review)
	}
}

// Supplying a DecoderConfig replaces koanf's default wholesale. These
// two are what that default was doing for us everywhere else in the
// file, and dropping either is silent.
func TestDecoderKeepsKoanfDefaults(t *testing.T) {
	t.Parallel()
	cfg := loadTOML(t, `
[gateway]
confirmation_timeout = "7m"

[[compute.providers]]
label      = "p"
model      = "m"
trust_tier = "private"
`)
	// StringToTimeDurationHookFunc.
	if got := cfg.Gateway.ConfirmationTimeout; got != 7*time.Minute {
		t.Errorf("confirmation_timeout = %v, want 7m — the duration hook is gone", got)
	}
	// TextUnmarshallerHookFunc: TrustTier parses "private" through its
	// own UnmarshalText and nothing else.
	if len(cfg.Compute.Providers) != 1 {
		t.Fatalf("%d providers, want 1", len(cfg.Compute.Providers))
	}
	if got := cfg.Compute.Providers[0].TrustTier; got.String() != "private" {
		t.Errorf("trust_tier = %v, want private — the text hook is gone", got)
	}
}
