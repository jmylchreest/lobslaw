package clawhub

import (
	"os"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/binaries"
)

// Satisfier.Satisfy is SatisfyOpts with a ZERO SatisfyOptions:
//
//	func (s *Satisfier) Satisfy(ctx, name, installs) (SatisfyResult, error) {
//	    return s.SatisfyOpts(ctx, name, installs, SatisfyOptions{})
//	}
//
// So calling it from here silently discards whatever
// WithSatisfyOptions was given. Install did exactly that while
// installFromBody passed i.satisfyOptions, which meant the same tool
// behaved differently depending on which entry point was used:
//
//	clawhub_install slug=...            -> InstallBySlug -> installFromBody -> honoured
//	clawhub_install name=... version=.. -> Install                          -> discarded
//
// The builtin sets BootstrapMissingManagers from the agent's
// bootstrap_managers argument, and WithSatisfyOptions' own doc comment
// names that caller as the reason it exists — so the option was
// discarded on the path it was written for.
//
// A source-level guard because the satisfier is a concrete type, not
// an interface, so there is nothing to inject a recorder into. The
// invariant is mechanical and the drift was too: an options-less call
// compiles, passes every test, and quietly ignores the operator.
func TestNoInstallPathDiscardsSatisfyOptions(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("install.go")
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(src), "\n") {
		code, _, _ := strings.Cut(line, "//")
		if strings.Contains(code, ".satisfier.Satisfy(") {
			t.Errorf("install.go:%d calls Satisfy, which drops SatisfyOptions — "+
				"use SatisfyOpts(ctx, bin, specs, i.satisfyOptions):\n  %s",
				i+1, strings.TrimSpace(line))
		}
	}
}

// WithSatisfyOptions must copy rather than mutate: the builtin derives
// a per-call installer from a shared one, and mutating in place would
// leak one agent's bootstrap_managers into every later install.
func TestWithSatisfyOptionsCopies(t *testing.T) {
	t.Parallel()

	base := &Installer{}
	derived := base.WithSatisfyOptions(satisfyOptionsForTest())

	if base.satisfyOptions == derived.satisfyOptions {
		t.Skip("zero options are equal by value; nothing to distinguish")
	}
	if base.satisfyOptions != (zeroSatisfyOptions()) {
		t.Error("WithSatisfyOptions mutated the receiver; a per-call option would leak")
	}
	if derived == base {
		t.Error("WithSatisfyOptions returned the same installer")
	}
}

func satisfyOptionsForTest() binaries.SatisfyOptions {
	return binaries.SatisfyOptions{BootstrapMissingManagers: true}
}
func zeroSatisfyOptions() binaries.SatisfyOptions { return binaries.SatisfyOptions{} }
