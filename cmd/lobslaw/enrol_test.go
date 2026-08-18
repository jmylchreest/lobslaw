package main

import (
	"bytes"
	"crypto/x509"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// `cluster export-operator` hands over both halves of a keypair, so the
// PRIVATE KEY has to travel. Here it is born on the laptop and stays
// there; what crosses the wire is a request and a certificate, both
// public and both useless without the private half.

// --- the spelling ------------------------------------------------------

// British single-l is canonical. The double-l is aliased rather than
// rejected: it is the spelling half the world's muscle memory
// produces, and a typo that prints "unknown subcommand" teaches
// nothing.
func TestBothSpellingsReachTheSameCommand(t *testing.T) {
	for _, spelling := range []string{"enrol", "enroll"} {
		if got := findSubcmd([]string{spelling, "list"}, spelling); got != 0 {
			t.Errorf("%q was not recognised as a subcommand", spelling)
		}
	}
	// And the canonical one leads: it is what the usage teaches.
	if !strings.Contains(enrolUsage, "lobslaw enrol —") {
		t.Error("the usage does not lead with the single-l spelling")
	}
	if strings.Contains(enrolUsage, "enroll") {
		t.Error("the usage advertises the alias; it should teach one spelling")
	}
}

func TestEveryEnrolSubcommandIsInTheUsage(t *testing.T) {
	for sub := range enrolForms {
		if !strings.Contains(enrolUsage, sub) {
			t.Errorf("usage does not mention %q", sub)
		}
	}
}

// --- the key never moves -----------------------------------------------

// THE PROPERTY THE WHOLE FEATURE EXISTS FOR. The request carries a
// public key; the private half is returned separately, for the caller
// to write locally.
func TestTheRequestCarriesNoPrivateKey(t *testing.T) {
	csrDER, keyPEM, fingerprint, err := generateOperatorKey("alice")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(csrDER, keyPEM) {
		t.Fatal("the certificate request contains the private key")
	}
	// A CSR is parseable and self-signed — which is what proves the
	// requester holds the key without ever sending it.
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		t.Fatal(err)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("the request does not prove key possession: %v", err)
	}
	if csr.Subject.CommonName != "alice" {
		t.Errorf("subject = %q", csr.Subject.CommonName)
	}
	if !strings.HasPrefix(fingerprint, "SHA256:") {
		t.Errorf("fingerprint = %q", fingerprint)
	}
	if !strings.Contains(string(keyPEM), "PRIVATE KEY") {
		t.Error("no private key was produced for the caller to keep")
	}
}

// Two runs must differ, or every laptop would enrol the same key.
func TestEachRequestGeneratesAFreshKey(t *testing.T) {
	_, a, fpA, err := generateOperatorKey("alice")
	if err != nil {
		t.Fatal(err)
	}
	_, b, fpB, err := generateOperatorKey("alice")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) || fpA == fpB {
		t.Error("two enrolments produced the same key")
	}
}

// --- refusing before dialling ------------------------------------------

// Without the CA a laptop cannot tell the cluster from anything else
// answering on that address, and would enrol against an impostor.
func TestRequestRefusesWithoutTheClusterCA(t *testing.T) {
	noAmbientCluster(t)
	err := enrolRequest([]string{"--addr", "node:9091", "--name", "alice"})
	if err == nil {
		t.Fatal("a request with no --ca-cert was attempted")
	}
	if !strings.Contains(err.Error(), "--ca-cert") {
		t.Errorf("error %q does not name the missing material", err)
	}
	// And it refused BEFORE generating anything. Reading the CA later
	// would also fail, but only after a key had been written — which
	// then blocks the retry, because the command refuses to overwrite
	// one.
	dir := t.TempDir()
	_ = enrolRequest([]string{"--addr", "node:9091", "--name", "alice", "--out", dir})
	if _, serr := os.Stat(filepath.Join(dir, "operator-key.pem")); serr == nil {
		t.Error("a refused request left a key behind, which blocks the retry")
	}
}

func TestRequestNeedsAnAddressAndAName(t *testing.T) {
	noAmbientCluster(t)
	if err := enrolRequest([]string{"--name", "alice", "--ca-cert", "/tmp/ca.pem"}); err == nil {
		t.Error("a request with no --addr was attempted")
	}
	if err := enrolRequest([]string{"--addr", "node:9091", "--ca-cert", "/tmp/ca.pem"}); err == nil {
		t.Error("a request with no --name was attempted")
	}
}

// That file may be the key to a credential still valid somewhere, and
// replacing it is not recoverable.
func TestRequestRefusesToOverwriteAnExistingKey(t *testing.T) {
	noAmbientCluster(t)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "operator-key.pem")
	if err := os.WriteFile(keyPath, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := enrolRequest([]string{"--addr", "node:9091", "--name", "alice",
		"--ca-cert", caPath, "--out", dir})
	if err == nil {
		t.Fatal("an existing key was overwritten")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error %q does not explain the refusal", err)
	}
	if got, _ := os.ReadFile(keyPath); string(got) != "existing" {
		t.Error("the existing key was modified")
	}
}

// An approval without a verified fingerprint is somebody clicking yes
// to a request they have not checked is the one they were told about.
func TestApprovalRequiresAFingerprint(t *testing.T) {
	noAmbientCluster(t)
	err := enrolDecide([]string{"some-id"}, true)
	if err == nil {
		t.Fatal("an approval with no --fingerprint was attempted")
	}
	if !strings.Contains(err.Error(), "--fingerprint") {
		t.Errorf("error %q does not say what is missing", err)
	}
}

// Denial does not need one: refusing a request you cannot identify is
// the safe direction, and requiring a fingerprint to say no would
// leave junk requests un-closable.
func TestDenialDoesNotRequireAFingerprint(t *testing.T) {
	noAmbientCluster(t)
	err := enrolDecide([]string{"some-id"}, false)
	if err == nil {
		t.Fatal("expected a connection failure, not success")
	}
	if strings.Contains(err.Error(), "--fingerprint") {
		t.Errorf("denial demanded a fingerprint: %v", err)
	}
}

func TestDecideNeedsExactlyOneId(t *testing.T) {
	noAmbientCluster(t)
	if err := enrolDecide(nil, false); err == nil {
		t.Error("a decision with no id was attempted")
	}
	if err := enrolDecide([]string{"a", "b"}, false); err == nil {
		t.Error("a decision with two ids was attempted")
	}
}

// --- what the operator reads -------------------------------------------

// A mismatched fingerprint means the node is describing a key this
// laptop did not generate. Printed as a refusal, not a footnote.
func TestAMismatchedFingerprintIsAWarningNotAFootnote(t *testing.T) {
	var buf bytes.Buffer
	printRequestSubmitted(&buf, &lobslawv1.SubmitEnrolmentResponse{
		Id: "abc123", Fingerprint: "SHA256:aa:bb",
	}, "SHA256:cc:dd", "/keys/operator-key.pem")

	out := buf.String()
	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "Do not approve") {
		t.Errorf("a fingerprint mismatch was not called out:\n%s", out)
	}
}

func TestAMatchingFingerprintTellsYouToReadItOut(t *testing.T) {
	var buf bytes.Buffer
	fp := "SHA256:aa:bb"
	printRequestSubmitted(&buf, &lobslawv1.SubmitEnrolmentResponse{
		Id: "abc123", Fingerprint: fp,
		ExpiresAt: timestamppb.Now(),
	}, fp, "/keys/operator-key.pem")

	out := buf.String()
	if strings.Contains(out, "WARNING") {
		t.Errorf("a matching fingerprint produced a warning:\n%s", out)
	}
	for _, want := range []string{"abc123", fp, "never sent", "Expires"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

// The fingerprint gets its own line, because it is the thing a human
// compares character by character.
func TestTheListingShowsFingerprintsProminently(t *testing.T) {
	var buf bytes.Buffer
	err := renderEnrolments(&buf, []*lobslawv1.EnrolmentRecord{{
		Id: "abc123", RequestedName: "alice", Fingerprint: "SHA256:aa:bb",
		State: lobslawv1.EnrolmentState_ENROLMENT_STATE_PENDING,
	}}, "prod:9090", false)
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"abc123", "alice", "SHA256:aa:bb", "prod:9090", "PENDING"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing is missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "--fingerprint") {
		t.Errorf("the listing does not tell the operator to verify:\n%s", out)
	}
}

// A rename must be visible, or an operator cannot tell what was
// actually admitted.
func TestARenameIsVisibleInTheListing(t *testing.T) {
	var buf bytes.Buffer
	err := renderEnrolments(&buf, []*lobslawv1.EnrolmentRecord{{
		Id: "abc123", RequestedName: "root", IssuedName: "alice",
		Fingerprint: "SHA256:aa:bb", DecidedBy: "user:owner",
		State: lobslawv1.EnrolmentState_ENROLMENT_STATE_ISSUED,
	}}, "prod:9090", false)
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "issued as") || !strings.Contains(out, "alice") {
		t.Errorf("the rename is not visible:\n%s", out)
	}
	if !strings.Contains(out, "user:owner") {
		t.Errorf("the listing does not say who decided:\n%s", out)
	}
}

func TestAnEmptyListingSaysSoAndNamesItsSource(t *testing.T) {
	var buf bytes.Buffer
	if err := renderEnrolments(&buf, nil, "prod:9090", false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "prod:9090") || !strings.Contains(out, "nothing waiting") {
		t.Errorf("empty listing:\n%s", out)
	}
}

// --- positionals before flags ------------------------------------------

// Go's flag package stops at the first non-flag argument, so
// `enrol approve <id> --fingerprint X` left BOTH the id and every flag
// in Args() — and the command rejected its own documented usage.
//
// `cluster export-operator alice --out ./alice` had the same bug, and
// its own error message told you to use the form that did not work.
func TestPositionalsMayComeBeforeFlags(t *testing.T) {
	for name, args := range map[string][]string{
		"before": {"the-id", "--fingerprint", "SHA256:aa"},
		"after":  {"--fingerprint", "SHA256:aa", "the-id"},
		"around": {"--fingerprint", "SHA256:aa", "the-id"},
	} {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(&bytes.Buffer{})
		fp := fs.String("fingerprint", "", "")

		rest, err := parseFlagsAndPositionals(fs, args)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(rest) != 1 || rest[0] != "the-id" {
			t.Errorf("%s: positionals = %v, want [the-id]", name, rest)
		}
		if *fp != "SHA256:aa" {
			t.Errorf("%s: the flag was not parsed (%q)", name, *fp)
		}
	}
}

// A flag whose VALUE looks like a positional must not be mistaken for
// one — this is why the parse loop exists rather than a naive split on
// leading non-flag arguments.
func TestAFlagValueIsNotMistakenForAPositional(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&bytes.Buffer{})
	out := fs.String("out", "", "")

	rest, err := parseFlagsAndPositionals(fs, []string{"alice", "--out", "./somewhere"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 || rest[0] != "alice" {
		t.Errorf("positionals = %v; a flag value was taken as one", rest)
	}
	if *out != "./somewhere" {
		t.Errorf("--out = %q", *out)
	}
}

func TestSeveralPositionalsKeepTheirOrder(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&bytes.Buffer{})
	fs.Bool("apply", false, "")

	rest, err := parseFlagsAndPositionals(fs, []string{"one", "--apply", "two", "three"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 3 || rest[0] != "one" || rest[1] != "two" || rest[2] != "three" {
		t.Errorf("positionals = %v", rest)
	}
}
