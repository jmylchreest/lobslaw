package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parseDotenvString is a tiny test-only adapter.
func parseDotenvString(t *testing.T, input string) ([]dotenvPair, error) {
	t.Helper()
	return parseDotenv(strings.NewReader(input))
}

func TestParseDotenvEmpty(t *testing.T) {
	t.Parallel()
	pairs, err := parseDotenvString(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 0 {
		t.Errorf("empty input → no pairs; got %v", pairs)
	}
}

func TestParseDotenvPlain(t *testing.T) {
	t.Parallel()
	pairs, err := parseDotenvString(t, "FOO=bar\nBAZ=qux\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 2 {
		t.Fatalf("want 2 pairs, got %d: %v", len(pairs), pairs)
	}
	if pairs[0].key != "FOO" || pairs[0].value != "bar" {
		t.Errorf("pair[0] = %+v", pairs[0])
	}
	if pairs[1].key != "BAZ" || pairs[1].value != "qux" {
		t.Errorf("pair[1] = %+v", pairs[1])
	}
}

func TestParseDotenvCommentsAndBlanks(t *testing.T) {
	t.Parallel()
	in := `
# a header comment
FOO=bar

# another comment
BAZ=qux
`
	pairs, err := parseDotenvString(t, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 2 {
		t.Errorf("want 2; got %d", len(pairs))
	}
}

func TestParseDotenvExportPrefix(t *testing.T) {
	t.Parallel()
	pairs, err := parseDotenvString(t, "export FOO=bar\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 || pairs[0].key != "FOO" || pairs[0].value != "bar" {
		t.Errorf("export prefix not stripped: %+v", pairs)
	}
}

func TestParseDotenvDoubleQuoted(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{`FOO="simple"`, "simple"},
		{`FOO="with spaces"`, "with spaces"},
		{`FOO="has \"quotes\" inside"`, `has "quotes" inside`},
		{`FOO="newline\nhere"`, "newline\nhere"},
		{`FOO="tab\there"`, "tab\there"},
		{`FOO="back\\slash"`, `back\slash`},
		{`FOO="unknown\xescape"`, `unknown\xescape`}, // unknown escapes kept literal
	}
	for _, tc := range cases {
		pairs, err := parseDotenvString(t, tc.in)
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if len(pairs) != 1 || pairs[0].value != tc.want {
			t.Errorf("%q: got %q, want %q", tc.in, pairs[0].value, tc.want)
		}
	}
}

func TestParseDotenvSingleQuoted(t *testing.T) {
	t.Parallel()
	// Single-quoted values are LITERAL — no escapes interpreted.
	pairs, err := parseDotenvString(t, `FOO='literal \n no escape'`)
	if err != nil {
		t.Fatal(err)
	}
	if pairs[0].value != `literal \n no escape` {
		t.Errorf("single-quote escapes should not be processed; got %q", pairs[0].value)
	}
}

func TestParseDotenvInlineComment(t *testing.T) {
	t.Parallel()
	pairs, err := parseDotenvString(t, "FOO=bar # tail comment\nBAZ=qux\t# tail via tab")
	if err != nil {
		t.Fatal(err)
	}
	if pairs[0].value != "bar" {
		t.Errorf("inline comment after space not stripped: %q", pairs[0].value)
	}
	if pairs[1].value != "qux" {
		t.Errorf("inline comment after tab not stripped: %q", pairs[1].value)
	}
}

func TestParseDotenvQuotedCommentNotStripped(t *testing.T) {
	t.Parallel()
	// # inside a quoted value is part of the value, not a comment.
	pairs, err := parseDotenvString(t, `FOO="keep # this"`)
	if err != nil {
		t.Fatal(err)
	}
	if pairs[0].value != "keep # this" {
		t.Errorf("# inside quotes should be literal; got %q", pairs[0].value)
	}
}

func TestParseDotenvInvalidKey(t *testing.T) {
	t.Parallel()
	cases := []string{
		"1FOO=bar",       // starts with digit
		"FOO-BAR=baz",    // hyphen not allowed
		"=value",         // empty key
		"FOO BAR=x",      // space in key
	}
	for _, in := range cases {
		_, err := parseDotenvString(t, in)
		if err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}

func TestParseDotenvUnterminatedQuotes(t *testing.T) {
	t.Parallel()
	if _, err := parseDotenvString(t, `FOO="unterminated`); err == nil {
		t.Error("unterminated double quote should error")
	}
	if _, err := parseDotenvString(t, `FOO='unterminated`); err == nil {
		t.Error("unterminated single quote should error")
	}
}

func TestParseDotenvContentAfterClosingQuote(t *testing.T) {
	t.Parallel()
	// Garbage after a quoted value should fail — catches typos.
	if _, err := parseDotenvString(t, `FOO="x" garbage`); err == nil {
		t.Error("content after closing quote should be rejected")
	}
	// But comments are allowed.
	if _, err := parseDotenvString(t, `FOO="x" # trailing comment`); err != nil {
		t.Errorf("trailing comment should be allowed: %v", err)
	}
}

func TestParseDotenvEmptyValue(t *testing.T) {
	t.Parallel()
	pairs, err := parseDotenvString(t, "FOO=\nBAR=\"\"\nBAZ=''")
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 3 {
		t.Fatalf("want 3; got %d", len(pairs))
	}
	for i, p := range pairs {
		if p.value != "" {
			t.Errorf("pair[%d] = %q, want empty", i, p.value)
		}
	}
}

func TestParseDotenvLineNumberInErrors(t *testing.T) {
	t.Parallel()
	// Third line is the broken one.
	in := "FOO=ok\nBAR=ok\ngarbage without equals\n"
	_, err := parseDotenvString(t, in)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "line 3") {
		t.Errorf("error should include line number; got %q", err.Error())
	}
}

// --- LoadDotenv integration (actual env manipulation) ---

func TestLoadDotenvMissingFileIsNoError(t *testing.T) {
	t.Parallel()
	if err := LoadDotenv("/no/such/path/.env"); err != nil {
		t.Errorf("missing file should be a no-op, got %v", err)
	}
}

func TestLoadDotenvSetsEnv(t *testing.T) {
	t.Setenv("LOBSLAW_DOTENV_TEST_UNIQUE", "")
	_ = os.Unsetenv("LOBSLAW_DOTENV_TEST_UNIQUE")

	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("LOBSLAW_DOTENV_TEST_UNIQUE=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := LoadDotenv(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("LOBSLAW_DOTENV_TEST_UNIQUE"); got != "from-file" {
		t.Errorf("env not populated: got %q", got)
	}
	// Cleanup so later tests don't see this.
	_ = os.Unsetenv("LOBSLAW_DOTENV_TEST_UNIQUE")
}

// TestLoadDotenvExistingEnvWins — if the operator set the var in
// their shell before starting lobslaw, .env must NOT clobber it.
// Unix convention: outer env is authoritative.
func TestLoadDotenvExistingEnvWins(t *testing.T) {
	t.Setenv("LOBSLAW_DOTENV_TEST_EXISTING", "from-shell")
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("LOBSLAW_DOTENV_TEST_EXISTING=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := LoadDotenv(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("LOBSLAW_DOTENV_TEST_EXISTING"); got != "from-shell" {
		t.Errorf("shell env should win over .env; got %q", got)
	}
}

func TestLoadDotenvEmptyPathUsesDiscovery(t *testing.T) {
	// Point discovery chain at a clean sandbox with no .env anywhere.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("LOBSLAW_ENV", "")
	// chdir somewhere without .env
	oldWd, _ := os.Getwd()
	_ = os.Chdir(dir)
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	if err := LoadDotenv(""); err != nil {
		t.Errorf("no .env anywhere should be silent no-op; got %v", err)
	}
}

func TestLoadDotenvExplicitEnvVar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.env")
	if err := os.WriteFile(path, []byte("LOBSLAW_DOTENV_VIA_ENV=yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Unsetenv("LOBSLAW_DOTENV_VIA_ENV")
	t.Setenv("LOBSLAW_ENV", path)

	if err := LoadDotenv(""); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOBSLAW_DOTENV_VIA_ENV") != "yes" {
		t.Error("explicit LOBSLAW_ENV should be honoured")
	}
	_ = os.Unsetenv("LOBSLAW_DOTENV_VIA_ENV")
}

func TestLoadDotenvSyntaxErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("BROKEN no equals here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := LoadDotenv(path)
	if err == nil {
		t.Error("syntax error should propagate")
	}
}
