package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// stubBin writes an executable script and puts its directory at the
// front of PATH, so a driver that shells out to "bw" finds this instead
// of a real one. Not parallel-safe with anything else that touches
// PATH, which is why the tests using it do not call t.Parallel.
func stubBin(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

func mustProvider(t *testing.T, f Factory, cfg ProviderConfig) Provider {
	t.Helper()
	p, err := f(cfg)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	return p
}

func TestExecFactoryRejectsBadConfig(t *testing.T) {
	t.Parallel()

	if _, err := ExecFactory(ProviderConfig{Label: "pass"}); err == nil {
		t.Error("exec without a command should fail at boot, not at first fetch")
	}
	_, err := ExecFactory(ProviderConfig{
		Label: "pass", Command: []string{"true"},
		Options: map[string]string{"trim_whitepsace": "false"},
	})
	if err == nil || !strings.Contains(err.Error(), "trim_whitepsace") {
		t.Errorf("a typo'd option should be named at boot; got %v", err)
	}
	// Exactly, not case-folded: option() reads keys exactly, and a
	// validator that folds case lets a wrong-case key validate and then
	// be silently ignored.
	if _, err := ExecFactory(ProviderConfig{
		Label: "pass", Command: []string{"true"},
		Options: map[string]string{"Trim_Whitespace": "false"},
	}); err == nil {
		t.Error("a wrong-case option should be rejected, not quietly dropped")
	}
}

func TestExecSubstitutesPathAndTrims(t *testing.T) {
	stubBin(t, "fakevault", `echo "  secret-for-$1  "`)

	p := mustProvider(t, ExecFactory, ProviderConfig{
		Label: "v", Command: []string{"fakevault", pathPlaceholder},
	})
	got, err := p.Fetch(context.Background(), "app/key")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got != "secret-for-app/key" {
		t.Errorf("got %q; want the trimmed value", got)
	}
}

// A CLI that prints a trailing newline is the norm, and a key with \n
// on the end fails authentication in a way nothing reports usefully —
// so trimming is the default and turning it off is explicit.
func TestExecTrimCanBeDisabled(t *testing.T) {
	stubBin(t, "fakevault2", `printf 'value\n'`)

	p := mustProvider(t, ExecFactory, ProviderConfig{
		Label: "v", Command: []string{"fakevault2"},
		Options: map[string]string{"trim_whitespace": "false"},
	})
	got, err := p.Fetch(context.Background(), "ignored")
	if err != nil {
		t.Fatal(err)
	}
	if got != "value\n" {
		t.Errorf("got %q; want the newline preserved", got)
	}
}

// With no placeholder the path is appended, which is what `pass show
// <path>` and friends expect.
func TestExecAppendsPathWhenNoPlaceholder(t *testing.T) {
	stubBin(t, "fakepass", `echo "$2"`)

	p := mustProvider(t, ExecFactory, ProviderConfig{
		Label: "pass", Command: []string{"fakepass", "show"},
	})
	got, err := p.Fetch(context.Background(), "lobslaw/alibaba")
	if err != nil {
		t.Fatal(err)
	}
	if got != "lobslaw/alibaba" {
		t.Errorf("got %q; the path should have been appended", got)
	}
}

func TestExecSurfacesFailures(t *testing.T) {
	t.Run("stderr reaches the operator", func(t *testing.T) {
		stubBin(t, "failvault", `echo "gpg: decryption failed" >&2; exit 2`)
		p := mustProvider(t, ExecFactory, ProviderConfig{Label: "v", Command: []string{"failvault"}})
		_, err := p.Fetch(context.Background(), "x")
		if err == nil || !strings.Contains(err.Error(), "decryption failed") {
			t.Errorf("the command's own reason should survive; got %v", err)
		}
	})

	t.Run("missing binary is named", func(t *testing.T) {
		p := mustProvider(t, ExecFactory, ProviderConfig{
			Label: "v", Command: []string{"definitely-not-a-real-binary-xyz"},
		})
		_, err := p.Fetch(context.Background(), "x")
		if err == nil || !strings.Contains(err.Error(), "PATH") {
			t.Errorf("want a PATH error; got %v", err)
		}
	})

	t.Run("empty output is an error", func(t *testing.T) {
		stubBin(t, "emptyvault", `true`)
		p := mustProvider(t, ExecFactory, ProviderConfig{Label: "v", Command: []string{"emptyvault"}})
		if _, err := p.Fetch(context.Background(), "x"); err == nil {
			t.Error("an empty secret is a failure, not a secret")
		}
	})

	t.Run("a prompting CLI times out rather than hanging the boot", func(t *testing.T) {
		stubBin(t, "hangvault", `sleep 30`)
		p := mustProvider(t, ExecFactory, ProviderConfig{
			Label: "v", Command: []string{"hangvault"}, Timeout: 150 * time.Millisecond,
		})
		start := time.Now()
		_, err := p.Fetch(context.Background(), "x")
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Errorf("want a timeout; got %v", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("took %s; the timeout was not enforced", elapsed)
		}
	})
}

// The subprocess gets what it needs to work and nothing else.
//
// Before this, the vault CLI was handed all of os.Environ() — so a
// node holding an Anthropic key, a Telegram bot token and a Slack signing
// secret in its environment passed every one of them to `pass`, to `bw`,
// and to whatever argv an operator put in config.toml. Fetching one
// secret exposed all the others.
func TestExecEnvIsAllowlisted(t *testing.T) {
	// printenv rather than `env`: the classifier's own catalogue treats
	// `env` as a command wrapper, and a test that reads like an exploit
	// is a test someone will later "fix".
	stubBin(t, "envvault", `printenv | sort | tr '\n' ';'`)

	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-should-not-leak")
	t.Setenv("TELEGRAM_BOT_TOKEN", "123:should-not-leak")
	t.Setenv("GNUPGHOME", "/home/agent/.gnupg")

	p := mustProvider(t, ExecFactory, ProviderConfig{
		Label:   "v",
		Command: []string{"envvault"},
		Env:     map[string]string{"DECLARED_TOKEN": "declared-value"},
	})
	got, err := p.Fetch(context.Background(), "x")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	for _, leaked := range []string{"ANTHROPIC_API_KEY", "TELEGRAM_BOT_TOKEN", "should-not-leak"} {
		if strings.Contains(got, leaked) {
			t.Errorf("%s reached the vault subprocess; the environment is not allowlisted", leaked)
		}
	}
	// The allowlist has to be functional, not empty. The reason the
	// environment was inherited holds: `pass` reads GNUPGHOME and HOME, and a
	// provider started with a bare environment fails in a way that looks
	// like the vault is broken.
	for _, needed := range []string{"PATH=", "HOME=", "GNUPGHOME=/home/agent/.gnupg"} {
		if !strings.Contains(got, needed) {
			t.Errorf("%s did not reach the subprocess; the vault CLI needs it", needed)
		}
	}
	// A variable the operator declared is not subject to the allowlist —
	// declaring it IS the authorisation.
	if !strings.Contains(got, "DECLARED_TOKEN=declared-value") {
		t.Error("the provider's own env block should always pass through")
	}
}

// env_passthrough is how an operator keeps a variable the allowlist does
// not know about, without going back to inheriting everything.
func TestExecEnvPassthroughOption(t *testing.T) {
	stubBin(t, "envvault2", `printenv | sort | tr '\n' ';'`)

	t.Setenv("VAULT_EXTRA_FLAG", "wanted")
	t.Setenv("UNRELATED_SECRET", "not-wanted")

	p := mustProvider(t, ExecFactory, ProviderConfig{
		Label: "v", Command: []string{"envvault2"},
		Options: map[string]string{"env_passthrough": "VAULT_EXTRA_FLAG"},
	})
	got, err := p.Fetch(context.Background(), "x")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !strings.Contains(got, "VAULT_EXTRA_FLAG=wanted") {
		t.Error("a named passthrough variable should reach the subprocess")
	}
	if strings.Contains(got, "UNRELATED_SECRET") {
		t.Error("env_passthrough should name variables, not open the gate")
	}
}

// Each vendor driver knows which variables its own CLI authenticates
// with, so the documented `export BW_SESSION` workflow keeps working
// without the operator listing it by hand.
func TestVendorDriversAllowTheirOwnCredentialVars(t *testing.T) {
	t.Run("bitwarden keeps BW_SESSION", func(t *testing.T) {
		stubBin(t, "bw", `printenv | sort | tr '\n' ';'`)
		t.Setenv("BW_SESSION", "session-token")
		t.Setenv("UNRELATED_SECRET", "not-wanted")

		p := mustProvider(t, BitwardenFactory, ProviderConfig{Label: "bw"})
		got, err := p.Fetch(context.Background(), "app/key")
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if !strings.Contains(got, "BW_SESSION=session-token") {
			t.Error("BW_SESSION should survive; the driver's own hint tells operators to export it")
		}
		if strings.Contains(got, "UNRELATED_SECRET") {
			t.Error("the vendor allowlist should not reopen the whole environment")
		}
	})

	t.Run("1password keeps OP_SERVICE_ACCOUNT_TOKEN", func(t *testing.T) {
		stubBin(t, "op", `printenv | sort | tr '\n' ';'`)
		t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "ops-token")
		t.Setenv("UNRELATED_SECRET", "not-wanted")

		p := mustProvider(t, OnePasswordFactory, ProviderConfig{Label: "op"})
		got, err := p.Fetch(context.Background(), "Private/Item/field")
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if !strings.Contains(got, "OP_SERVICE_ACCOUNT_TOKEN=ops-token") {
			t.Error("OP_SERVICE_ACCOUNT_TOKEN should survive; the driver's own hint names it")
		}
		if strings.Contains(got, "UNRELATED_SECRET") {
			t.Error("the vendor allowlist should not reopen the whole environment")
		}
	})
}

// `op signin` exports a per-account session variable, and opAuthHint tells
// operators to run it. The name is not fixed: 1Password v1 exports
// OP_SESSION_<shorthand>, so an exact-match allowlist cannot cover it.
func TestOnePasswordKeepsInteractiveSessionVars(t *testing.T) {
	stubBin(t, "op", `printenv | sort | tr '\n' ';'`)
	t.Setenv("OP_SESSION_myaccount", "signin-session")
	t.Setenv("OP_SESSION", "bare-session")
	t.Setenv("UNRELATED_SECRET", "not-wanted")

	p := mustProvider(t, OnePasswordFactory, ProviderConfig{Label: "op"})
	got, err := p.Fetch(context.Background(), "Private/Item/field")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	for _, want := range []string{"OP_SESSION_myaccount=signin-session", "OP_SESSION=bare-session"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s did not reach the subprocess; `op signin` is what opAuthHint recommends", want)
		}
	}
	if strings.Contains(got, "UNRELATED_SECRET") {
		t.Error("a prefix rule must not reopen the whole environment")
	}
}

// A driver-contributed prefix is code, reviewed once. env_passthrough is
// operator input and stays exact-match, so nobody can write OP_* or
// SECRET_* and reinstate wholesale inheritance.
//
// Refused at boot rather than matched literally: matching literally
// would be safe but silent, and a setting that parses and then does
// nothing is the failure this package's option validation exists to
// prevent.
func TestEnvPassthroughRefusesPatterns(t *testing.T) {
	t.Parallel()

	for _, pattern := range []string{"PREFIXED_*", "*", "OP_SESSION_?", "SECRET_[AB]"} {
		_, err := ExecFactory(ProviderConfig{
			Label: "v", Command: []string{"true"},
			Options: map[string]string{"env_passthrough": pattern},
		})
		if err == nil {
			t.Errorf("env_passthrough %q should be refused at boot, not silently matched", pattern)
			continue
		}
		if !strings.Contains(err.Error(), "env_passthrough") {
			t.Errorf("the error should name the setting; got %v", err)
		}
	}

	// An ordinary comma-separated list still parses.
	if _, err := ExecFactory(ProviderConfig{
		Label: "v", Command: []string{"true"},
		Options: map[string]string{"env_passthrough": "PASSWORD_STORE_DIR, GNUPGHOME"},
	}); err != nil {
		t.Errorf("a plain name list should be accepted; got %v", err)
	}
}

// env_passthrough is the documented migration for a variable the
// allowlist does not know about. It has to work on the vendor drivers
// too, which is where a wrapper script or an unusual CLI setting is most
// likely to need it.
func TestVendorDriversAcceptEnvPassthrough(t *testing.T) {
	for _, tc := range []struct {
		name string
		bin  string
		f    Factory
		path string
	}{
		{"bitwarden", "bw", BitwardenFactory, "app/key"},
		{"onepassword", "op", OnePasswordFactory, "Private/Item/field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubBin(t, tc.bin, `printenv | sort | tr '\n' ';'`)
			t.Setenv("VAULT_EXTRA_FLAG", "wanted")

			p, err := tc.f(ProviderConfig{
				Label:   tc.name,
				Options: map[string]string{"env_passthrough": "VAULT_EXTRA_FLAG"},
			})
			if err != nil {
				t.Fatalf("factory rejected env_passthrough: %v", err)
			}
			got, err := p.Fetch(context.Background(), tc.path)
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if !strings.Contains(got, "VAULT_EXTRA_FLAG=wanted") {
				t.Error("env_passthrough had no effect on the vendor driver")
			}
		})
	}
}

// The reason bitwarden and onepassword are compiled drivers rather than
// exec blocks: the CLI's own words do not tell an operator what to do.
func TestVendorDriversTranslateTheirFailures(t *testing.T) {
	t.Run("bitwarden locked vault", func(t *testing.T) {
		stubBin(t, "bw", `echo "mac failed." >&2; exit 1`)
		p := mustProvider(t, BitwardenFactory, ProviderConfig{Label: "bw"})
		_, err := p.Fetch(context.Background(), "app/key")
		if err == nil {
			t.Fatal("want an error")
		}
		if !strings.Contains(err.Error(), "bw unlock") {
			t.Errorf("error should name the fix; got %v", err)
		}
		// The CLI's own output is kept, never replaced: guessing wrong
		// about which failure this is must not hide what it said.
		if !strings.Contains(err.Error(), "mac failed") {
			t.Errorf("the original message should survive; got %v", err)
		}
	})

	t.Run("1password not signed in", func(t *testing.T) {
		stubBin(t, "op", `echo "error: not signed in" >&2; exit 1`)
		p := mustProvider(t, OnePasswordFactory, ProviderConfig{Label: "op"})
		_, err := p.Fetch(context.Background(), "Private/Alibaba/credential")
		if err == nil || !strings.Contains(err.Error(), "op signin") {
			t.Errorf("error should name the fix; got %v", err)
		}
	})

	t.Run("1password path becomes an op:// uri", func(t *testing.T) {
		stubBin(t, "op", `echo "$2"`)
		p := mustProvider(t, OnePasswordFactory, ProviderConfig{Label: "op"})
		got, err := p.Fetch(context.Background(), "Private/Alibaba/credential")
		if err != nil {
			t.Fatal(err)
		}
		if got != "op://Private/Alibaba/credential" {
			t.Errorf("got %q; the vendor's own addressing should be reassembled", got)
		}
	})
}

func TestResolverRoutesBootstrapSchemesUnchanged(t *testing.T) {
	t.Setenv("LOBSLAW_TEST_SECRET", "from-env")
	dir := t.TempDir()
	file := filepath.Join(dir, "s")
	if err := os.WriteFile(file, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := NewResolver(map[string]Provider{}, 0)
	if got, err := r.Resolve("env:LOBSLAW_TEST_SECRET"); err != nil || got != "from-env" {
		t.Errorf("env: = %q, %v", got, err)
	}
	if got, err := r.Resolve("file:" + file); err != nil || got != "from-file" {
		t.Errorf("file: = %q, %v", got, err)
	}
	if got, err := r.Resolve(""); err != nil || got != "" {
		t.Errorf("empty ref = %q, %v", got, err)
	}
	// A literal is still refused, so a plaintext secret cannot be
	// committed by accident. That decision predates this package and
	// survives it.
	if _, err := r.Resolve("just-a-literal"); err == nil {
		t.Error("a literal must not be accepted as a reference")
	}
}

func TestResolverUnknownSchemeNamesWhatIsConfigured(t *testing.T) {
	t.Parallel()

	r := NewResolver(map[string]Provider{"bw": stubProvider{}}, 0)
	_, err := r.Resolve("vault:app/key")
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, types.ErrUnknownSecretScheme) {
		t.Errorf("should wrap ErrUnknownSecretScheme; got %v", err)
	}
	if !strings.Contains(err.Error(), "bw") {
		t.Errorf("error should list what IS configured; got %v", err)
	}
}

type stubProvider struct{ calls *int }

func (s stubProvider) Fetch(context.Context, string) (string, error) {
	if s.calls != nil {
		*s.calls++
	}
	return "v", nil
}

// One boot resolves the same reference several times — the chat driver,
// the capability probe and doctor all read the same provider key — and
// on a CLI-backed vault each of those is a separate process.
func TestResolverCachesWithinTTL(t *testing.T) {
	t.Parallel()

	calls := 0
	r := NewResolver(map[string]Provider{"bw": stubProvider{calls: &calls}}, time.Minute)
	for range 3 {
		if _, err := r.Resolve("bw:app/key"); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Errorf("provider called %d times; want 1", calls)
	}

	// A different reference is a different secret.
	if _, err := r.Resolve("bw:app/other"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("provider called %d times; want 2", calls)
	}
}

func TestResolverExpiresCache(t *testing.T) {
	t.Parallel()

	calls := 0
	r := NewResolver(map[string]Provider{"bw": stubProvider{calls: &calls}}, time.Nanosecond)
	for range 2 {
		if _, err := r.Resolve("bw:app/key"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	if calls != 2 {
		t.Errorf("provider called %d times; an expired entry should be refetched", calls)
	}
}

// The bootstrap floor. cmd/lobslaw resolves the memory key before
// node.New, so before any provider can exist — and the error has to say
// that, because "unknown scheme: bw" is a confusing thing to read when
// bw is configured and working further down the same file.
func TestBootstrapRefusesVaultRefsWithAnExplanation(t *testing.T) {
	t.Setenv("LOBSLAW_TEST_BOOT", "ok")

	if got, err := Bootstrap("env:LOBSLAW_TEST_BOOT"); err != nil || got != "ok" {
		t.Errorf("env: should still work: %q %v", got, err)
	}
	_, err := Bootstrap("bw:memory/key")
	if err == nil {
		t.Fatal("a vault ref must not be accepted here")
	}
	for _, want := range []string{"env:", "file:", "memory.encryption.key_ref"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q; got %v", want, err)
		}
	}
}

// pkg/config duplicates the reserved list because it sits below this
// package. Asserted from here, which can see both, rather than hoped
// for — the same move internal/gateway makes for the queue-mode names.
func TestReservedSchemesAgreeWithConfig(t *testing.T) {
	t.Parallel()

	fromConfig := config.ReservedSecretSchemes()
	if len(fromConfig) != len(BootstrapSchemes) {
		t.Fatalf("config reserves %v, secrets bootstraps %v", fromConfig, BootstrapSchemes)
	}
	for _, s := range fromConfig {
		if !IsBootstrapScheme(s) {
			t.Errorf("config reserves %q but it is not a bootstrap scheme here", s)
		}
	}
}

func TestFromConfigBuildsAndRejects(t *testing.T) {
	t.Setenv("LOBSLAW_TEST_BW_SESSION", "sess")

	r, err := FromConfig(config.SecretsConfig{
		Providers: []config.SecretProviderConfig{
			{Label: "pass", Driver: "exec", Command: []string{"true", pathPlaceholder}},
			{Label: "bw", Driver: "bitwarden",
				Env:       map[string]string{"BW_CONFIG_DIR": "/etc/lobslaw/bw"},
				SecretEnv: map[string]string{"BW_SESSION": "env:LOBSLAW_TEST_BW_SESSION"},
			},
		},
	}, nil, nil)
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if _, ok := r.providers["pass"]; !ok {
		t.Error("pass provider was not built")
	}

	// An unknown driver fails at boot naming the drivers that exist.
	_, err = FromConfig(config.SecretsConfig{
		Providers: []config.SecretProviderConfig{{Label: "x", Driver: "hashicorp"}},
	}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "exec") {
		t.Errorf("error should list available drivers; got %v", err)
	}

	// A provider credential that is itself a vault ref is refused,
	// because the vault it names cannot exist yet.
	_, err = FromConfig(config.SecretsConfig{
		Providers: []config.SecretProviderConfig{
			{Label: "bw", Driver: "bitwarden", SecretEnv: map[string]string{"BW_SESSION": "op:a/b/c"}},
		},
	}, nil, nil)
	if err == nil {
		t.Error("a provider credential from another vault must be refused")
	}
}

// env is plaintext and secret_env is references, split exactly as
// [mcp.servers.<name>] splits them. An earlier version resolved every
// env value as a reference, which made a non-secret setting —
// BITWARDENCLI_APPDATA_DIR, NODE_EXTRA_CA_CERTS — impossible to
// configure at all, because a bare path is not a valid reference.
func TestFromConfigSplitsPlaintextFromReferences(t *testing.T) {
	t.Setenv("LOBSLAW_TEST_SESSION", "resolved-session")

	r, err := FromConfig(config.SecretsConfig{
		Providers: []config.SecretProviderConfig{{
			Label: "bw", Driver: "exec", Command: []string{"true"},
			Env:       map[string]string{"BW_CONFIG_DIR": "/var/lib/bw", "OTHER": "plain"},
			SecretEnv: map[string]string{"BW_SESSION": "env:LOBSLAW_TEST_SESSION"},
		}},
	}, nil, nil)
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	env := r.providers["bw"].(*execProvider).env
	if env["BW_CONFIG_DIR"] != "/var/lib/bw" {
		t.Errorf("plaintext env was not passed through: %q", env["BW_CONFIG_DIR"])
	}
	if env["BW_SESSION"] != "resolved-session" {
		t.Errorf("secret_env was not resolved: %q", env["BW_SESSION"])
	}
}

// If both name the same variable the reference wins, which is the only
// ordering that cannot silently downgrade a secret to a literal.
func TestSecretEnvBeatsPlaintextOnCollision(t *testing.T) {
	t.Setenv("LOBSLAW_TEST_SESSION2", "the-real-secret")

	r, err := FromConfig(config.SecretsConfig{
		Providers: []config.SecretProviderConfig{{
			Label: "bw", Driver: "exec", Command: []string{"true"},
			Env:       map[string]string{"TOKEN": "placeholder"},
			SecretEnv: map[string]string{"TOKEN": "env:LOBSLAW_TEST_SESSION2"},
		}},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.providers["bw"].(*execProvider).env["TOKEN"]; got != "the-real-secret" {
		t.Errorf("TOKEN = %q; the reference must win", got)
	}
}

// FromConfig is exported and takes a config struct a caller may not
// have validated, so it cannot rely on Config.Validate having run.
//
// Both failures here BUILD cleanly and go wrong later, which is the
// shape this package exists to stop shipping: a reserved label is
// unreachable because Resolve routes env: and file: to the bootstrap
// path before consulting the provider map at all, and a duplicate
// silently takes whichever came last.
func TestFromConfigRefusesReservedAndDuplicateLabels(t *testing.T) {
	t.Parallel()

	_, err := FromConfig(config.SecretsConfig{
		Providers: []config.SecretProviderConfig{
			{Label: "env", Driver: "exec", Command: []string{"true"}},
		},
	}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("a reserved label should be refused; got %v", err)
	}

	_, err = FromConfig(config.SecretsConfig{
		Providers: []config.SecretProviderConfig{
			{Label: "bw", Driver: "exec", Command: []string{"true"}},
			{Label: "BW", Driver: "exec", Command: []string{"false"}},
		},
	}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("a duplicate label should be refused, case-folded; got %v", err)
	}
}

// Errors carry the command's stderr because that is where every CLI
// puts the real reason. They must never carry stdout, which is the
// secret itself.
func TestExecErrorsNeverIncludeStdout(t *testing.T) {
	stubBin(t, "leakyvault", `echo "SUPER-SECRET-VALUE"; echo "boom" >&2; exit 3`)

	p := mustProvider(t, ExecFactory, ProviderConfig{Label: "v", Command: []string{"leakyvault"}})
	_, err := p.Fetch(context.Background(), "x")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "SUPER-SECRET-VALUE") {
		t.Errorf("the secret reached the error, and therefore the logs: %v", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("stderr should survive so the operator learns why: %v", err)
	}
}

// "bw:" is a typo, not a request. Reaching the backend with an empty
// item name returns whatever that CLI says about nothing, which is a
// long way from the config line that caused it.
func TestResolverRejectsAnEmptyPath(t *testing.T) {
	t.Parallel()

	r := NewResolver(map[string]Provider{"bw": stubProvider{}}, 0)
	_, err := r.Resolve("bw:")
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, types.ErrMissingSecret) {
		t.Errorf("should wrap ErrMissingSecret; got %v", err)
	}
	if !strings.Contains(err.Error(), "bw") {
		t.Errorf("error should name the provider; got %v", err)
	}
}

// Measured against the real Bitwarden CLI, which emits two Node
// deprecation warnings — about 180 characters of "the punycode module
// is deprecated" — BEFORE the sentence that matters. Keeping the head
// would preserve the warnings, cut off "You are not logged in.", and
// leave the hint unable to fire because the substring it matches on had
// been thrown away.
func TestExecKeepsTheTailOfNoisyStderr(t *testing.T) {
	stubBin(t, "noisyvault", `
		i=0; while [ $i -lt 30 ]; do echo "DeprecationWarning: something is deprecated" >&2; i=$((i+1)); done
		echo "You are not logged in." >&2
		exit 1`)

	p := mustProvider(t, ExecFactory, ProviderConfig{Label: "v", Command: []string{"noisyvault"}})
	_, err := p.Fetch(context.Background(), "x")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "You are not logged in") {
		t.Errorf("the operative error was truncated away:\n%v", err)
	}
	if !utf8.ValidString(err.Error()) {
		t.Error("truncation produced invalid UTF-8")
	}
}

// The same noise must not stop a vendor hint firing, which is the whole
// reason those drivers are compiled rather than exec blocks.
func TestVendorHintSurvivesNoisyStderr(t *testing.T) {
	stubBin(t, "bw", `
		i=0; while [ $i -lt 30 ]; do echo "DeprecationWarning: punycode is deprecated" >&2; i=$((i+1)); done
		echo "You are not logged in." >&2
		exit 1`)

	p := mustProvider(t, BitwardenFactory, ProviderConfig{Label: "bw"})
	_, err := p.Fetch(context.Background(), "app/key")
	if err == nil || !strings.Contains(err.Error(), "bw unlock") {
		t.Errorf("hint should still fire through the noise; got %v", err)
	}
}

// Recognition reads the WHOLE stderr; display reads a truncated copy.
//
// Both real CLIs settle this between them. Bitwarden puts its
// identifying sentence last, after Node deprecation warnings;
// 1Password puts its first, in an 800-character message that ends with
// a generic "error initializing client:". Matching against the
// displayed string would have broken one of them whichever end the cap
// kept.
func TestVendorHintMatchesUntruncatedStderr(t *testing.T) {
	// The identifying line is buried in the middle, so neither end of a
	// truncated copy contains it.
	stubBin(t, "bw", `
		i=0; while [ $i -lt 12 ]; do echo "warning: noise line to pad the head out a long way" >&2; i=$((i+1)); done
		echo "You are not logged in." >&2
		i=0; while [ $i -lt 12 ]; do echo "trailing noise to pad the tail out a long way too" >&2; i=$((i+1)); done
		exit 1`)

	p := mustProvider(t, BitwardenFactory, ProviderConfig{Label: "bw"})
	_, err := p.Fetch(context.Background(), "app/key")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "bw unlock") {
		t.Errorf("hint should fire on a sentence truncation removed from view:\n%v", err)
	}
	// And the displayed message is still bounded.
	if len(err.Error()) > 1200 {
		t.Errorf("displayed error is %d bytes; the cap is not holding", len(err.Error()))
	}
}

// Both ends survive, because which end carries the meaning depends on
// the CLI.
func TestTruncateKeepsHeadAndTail(t *testing.T) {
	t.Parallel()

	s := "HEAD-MARKER" + strings.Repeat("x", 4000) + "TAIL-MARKER"
	got := truncate(s, 700)
	if !strings.Contains(got, "HEAD-MARKER") {
		t.Error("head was dropped; 1Password leads with the useful line")
	}
	if !strings.Contains(got, "TAIL-MARKER") {
		t.Error("tail was dropped; Bitwarden ends with the useful line")
	}
	if len(got) > 800 {
		t.Errorf("result is %d bytes; the cap is not holding", len(got))
	}
}
