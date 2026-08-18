package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Getting a credential onto a laptop without moving a private key.
//
// `cluster export-operator` generates the keypair centrally and hands
// over both halves, so the private key has to travel — over scp, a USB
// stick, or worst of all a chat channel, where it lands in a message
// store, a phone backup, and this cluster's own transcripts.
//
// Here the key is born on the laptop and stays there. What crosses the
// wire is a certificate request and, later, a certificate: both public,
// both useless to anyone who does not hold the private half.

const enrolUsage = `lobslaw enrol — ask a cluster for an operator credential

  enrol request --addr <host:port> --name <you>   generate a key and ask
  enrol status  --addr <host:port> --id <id>      has it been answered yet
  enrol list                                      requests waiting (operator)
  enrol approve <id>                              admit one (operator)
  enrol deny <id>                                 refuse one (operator)

request and status need no credential — that is the point. They talk to
the node's enrolment listener, which is separate from the cluster one.

list, approve and deny need an existing operator credential and use the
usual --context / --addr flags.

Your private key is written next to the context and never leaves this
machine. The fingerprint printed by "request" is what the approver sees;
if the two differ, something is between you and the cluster.`

// enrolForms pairs each subcommand with its implementation.
//
// A table so the ROUTING is a value a test can assert, matching every
// other dispatcher.
var enrolForms = map[string]func([]string) error{
	"request": enrolRequest,
	"status":  enrolStatus,
	"list":    enrolList,
	"approve": func(a []string) error { return enrolDecide(a, true) },
	"deny":    func(a []string) error { return enrolDecide(a, false) },
}

// enrolNames are the spellings this command answers to.
//
// British English single-l is canonical throughout — enrol, enrolling,
// enrolment. The double-l is aliased rather than rejected: it is the
// spelling half the world's muscle memory produces, and a typo that
// prints "unknown subcommand" teaches nothing.
var enrolNames = []string{"enrol", "enroll"}

func dispatchEnrol(args []string) bool {
	var idx int
	var matched string
	for _, name := range enrolNames {
		if i := findSubcmd(args, name); i >= 0 {
			idx, matched = i, name
			break
		}
	}
	if matched == "" {
		return false
	}
	sub := args[idx+1:]
	if len(sub) == 0 {
		fmt.Fprintln(os.Stderr, enrolUsage)
		os.Exit(2)
	}
	run, ok := enrolForms[sub[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown enrol subcommand %q\n\n%s\n", sub[0], enrolUsage)
		os.Exit(2)
	}
	if err := run(sub[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "enrol %s: %v\n", sub[0], err)
		os.Exit(1)
	}
	return true
}

// --- the laptop side ---------------------------------------------------

func enrolRequest(args []string) error {
	fs := flag.NewFlagSet("enrol request", flag.ExitOnError)
	addr := fs.String("addr", envOr("LOBSLAW_ENROL_ADDR", ""), "host:port of a node's enrolment listener")
	name := fs.String("name", "", "the name to be known by; the approver may change it")
	caFile := fs.String("ca-cert", "", "cluster CA used to verify the node")
	out := fs.String("out", "", "directory for the key and, once issued, the certificate")
	wait := fs.Duration("wait", 0, "poll until answered, up to this long (0 = do not wait)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch {
	case *addr == "":
		return errors.New("--addr is required: the node's enrolment listener")
	case strings.TrimSpace(*name) == "":
		return errors.New("--name is required")
	case *caFile == "":
		// The one piece of material enrolment cannot avoid needing. A
		// laptop that trusted whatever answered would enrol against an
		// impostor and never know.
		return errors.New("--ca-cert is required: without it this laptop cannot tell your cluster " +
			"from anything else that answers on that address")
	}

	dir := *out
	if dir == "" {
		dir = defaultInitDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	keyPath := filepath.Join(dir, "operator-key.pem")
	if _, err := os.Stat(keyPath); err == nil {
		// Refused rather than overwritten. That file may be the key to
		// a credential still valid somewhere, and replacing it is not
		// recoverable.
		return fmt.Errorf("%s already exists; move it aside if you really want a new key", keyPath)
	}

	csrDER, keyPEM, fingerprint, err := generateOperatorKey(*name)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", keyPath, err)
	}

	client, closeConn, err := enrolClient(*addr, *caFile)
	if err != nil {
		return err
	}
	defer closeConn()

	ctx, cancel := enrolContext()
	defer cancel()
	res, err := client.SubmitEnrolment(ctx, &lobslawv1.SubmitEnrolmentRequest{
		RequestedName: strings.TrimSpace(*name),
		CsrDer:        csrDER,
	})
	if err != nil {
		return err
	}

	printRequestSubmitted(os.Stdout, res, fingerprint, keyPath)
	if *wait <= 0 {
		return nil
	}
	return pollUntilAnswered(*addr, *caFile, res.GetId(), dir, *wait)
}

// generateOperatorKey makes the keypair and the request.
//
// Both here, on the laptop, which is the entire point: the private
// half is written to disk beside the context and never crosses the
// wire.
func generateOperatorKey(name string) (csrDER, keyPEM []byte, fingerprint string, err error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, "", fmt.Errorf("generate key: %w", err)
	}
	csrDER, err = x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: name}}, priv)
	if err != nil {
		return nil, nil, "", fmt.Errorf("build certificate request: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, "", fmt.Errorf("encode key: %w", err)
	}
	fingerprint, err = memory.Fingerprint(priv.Public())
	if err != nil {
		return nil, nil, "", err
	}
	return csrDER, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), fingerprint, nil
}

func printRequestSubmitted(w io.Writer, res *lobslawv1.SubmitEnrolmentResponse, localFP, keyPath string) {
	_, _ = fmt.Fprintf(w, "Request submitted: %s\n", res.GetId())
	_, _ = fmt.Fprintf(w, "Private key:       %s (never sent)\n", keyPath)
	_, _ = fmt.Fprintf(w, "\nFingerprint:       %s\n", localFP)
	if res.GetFingerprint() != localFP {
		// Printed as a refusal, not a footnote. Different fingerprints
		// mean the node is describing a key this laptop did not
		// generate, and approving that admits somebody else.
		_, _ = fmt.Fprintf(w,
			"\nWARNING: the node echoed a DIFFERENT fingerprint (%s).\n"+
				"Something is between you and the cluster. Do not approve this request.\n",
			res.GetFingerprint())
		return
	}
	_, _ = fmt.Fprintln(w, "\nRead that fingerprint to whoever is approving. They see the same string;")
	_, _ = fmt.Fprintln(w, "if it does not match, the request they are looking at is not yours.")
	if exp := res.GetExpiresAt(); exp != nil {
		_, _ = fmt.Fprintf(w, "\nExpires %s.\n", exp.AsTime().Format(time.RFC3339))
	}
}

// tlsClientConfig verifies the node and presents nothing.
func tlsClientConfig(pool *x509.CertPool, serverName string) *tls.Config {
	return &tls.Config{
		RootCAs:    pool,
		ServerName: serverName,
		MinVersion: tls.VersionTLS13,
	}
}

// splitHostPortLenient pulls the host out of an address for SNI.
func splitHostPortLenient(addr string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(addr)
	if err != nil {
		return "", "", fmt.Errorf("--addr %q must be host:port: %w", addr, err)
	}
	return host, port, nil
}

func enrolContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

// enrolClient dials the enrolment listener.
//
// Server-authenticated TLS with NO client certificate — this laptop
// has none, which is what it is asking for. The node is verified
// against the CA the operator supplied out of band; that is the only
// thing standing between an enrolment and an impostor.
func enrolClient(addr, caFile string) (lobslawv1.EnrolmentServiceClient, func(), error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read --ca-cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, nil, fmt.Errorf("%s is not a certificate", caFile)
	}
	host, _, err := splitHostPortLenient(addr)
	if err != nil {
		return nil, nil, err
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(
		credentials.NewTLS(tlsClientConfig(pool, host))))
	if err != nil {
		return nil, nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	return lobslawv1.NewEnrolmentServiceClient(conn), func() { _ = conn.Close() }, nil
}

func enrolStatus(args []string) error {
	fs := flag.NewFlagSet("enrol status", flag.ExitOnError)
	addr := fs.String("addr", envOr("LOBSLAW_ENROL_ADDR", ""), "host:port of a node's enrolment listener")
	caFile := fs.String("ca-cert", "", "cluster CA used to verify the node")
	id := fs.String("id", "", "the request id printed by `enrol request`")
	out := fs.String("out", "", "directory to write the certificate into once issued")
	wait := fs.Duration("wait", 0, "poll until answered, up to this long")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *addr == "" || *caFile == "" || *id == "" {
		return errors.New("--addr, --ca-cert and --id are all required")
	}
	dir := *out
	if dir == "" {
		dir = defaultInitDir()
	}
	if *wait > 0 {
		return pollUntilAnswered(*addr, *caFile, *id, dir, *wait)
	}
	_, err := pollOnce(*addr, *caFile, *id, dir)
	return err
}

// pollUntilAnswered polls until the request is decided or the window
// closes.
func pollUntilAnswered(addr, caFile, id, dir string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		done, err := pollOnce(addr, caFile, id, dir)
		if err != nil || done {
			return err
		}
		if time.Now().After(deadline) {
			// Not an error. The request is still live and somebody may
			// yet answer it; saying so beats implying it failed.
			fmt.Printf("Still waiting after %s. Run `lobslaw enrol status --id %s` again later.\n", wait, id)
			return nil
		}
		time.Sleep(2 * time.Second)
	}
}

// pollOnce reports whether the request has been answered, writing the
// credential out when it has.
func pollOnce(addr, caFile, id, dir string) (bool, error) {
	client, closeConn, err := enrolClient(addr, caFile)
	if err != nil {
		return false, err
	}
	defer closeConn()

	ctx, cancel := enrolContext()
	defer cancel()
	res, err := client.PollEnrolment(ctx, &lobslawv1.PollEnrolmentRequest{Id: id})
	if err != nil {
		return false, err
	}

	switch res.GetState() {
	case lobslawv1.EnrolmentState_ENROLMENT_STATE_PENDING:
		return false, nil
	case lobslawv1.EnrolmentState_ENROLMENT_STATE_DENIED:
		return true, errors.New("the request was refused")
	case lobslawv1.EnrolmentState_ENROLMENT_STATE_EXPIRED:
		return true, errors.New("the request expired before anybody answered it; run `enrol request` again")
	case lobslawv1.EnrolmentState_ENROLMENT_STATE_ISSUED:
		return true, writeIssuedCredential(dir, res)
	}
	return false, fmt.Errorf("the node reported an unrecognised state %q", res.GetState())
}

// writeIssuedCredential lands the certificate and both roots.
//
// All three, because a certificate alone is not usable: the operator
// root is needed to present a chain, and the cluster root to verify
// the node on every later connection.
func writeIssuedCredential(dir string, res *lobslawv1.PollEnrolmentResponse) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	files := []struct {
		name string
		data []byte
	}{
		{"operator.pem", res.GetCertPem()},
		{"operator-ca.pem", res.GetCaPem()},
		{"ca.pem", res.GetClusterCaPem()},
	}
	for _, f := range files {
		if len(f.data) == 0 {
			return fmt.Errorf("the node issued a credential but sent no %s", f.name)
		}
		if err := os.WriteFile(filepath.Join(dir, f.name), f.data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", f.name, err)
		}
	}
	fmt.Printf("Approved. Issued to %q.\n\n", res.GetIssuedName())
	for _, f := range files {
		fmt.Printf("  %s\n", filepath.Join(dir, f.name))
	}
	fmt.Printf("\nAdd this to %s:\n\n", contextsPath())
	fmt.Print(operatorContextSnippet(
		filepath.Join(dir, "operator.pem"),
		filepath.Join(dir, "operator-key.pem"),
		filepath.Join(dir, "ca.pem")))
	return nil
}

// --- the operator side -------------------------------------------------

func enrolList(args []string) error {
	fs := flag.NewFlagSet("enrol list", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	all := fs.Bool("all", false, "include requests already answered")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	conn, err := node.dial()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	client := lobslawv1.NewEnrolmentServiceClient(conn)

	ctx, cancel := node.ctx()
	defer cancel()
	res, err := client.ListEnrolments(ctx, &lobslawv1.ListEnrolmentsRequest{PendingOnly: !*all})
	if err != nil {
		return err
	}
	return renderEnrolments(os.Stdout, res.GetEnrolments(), node.addr, *asJSON)
}

func renderEnrolments(w io.Writer, recs []*lobslawv1.EnrolmentRecord, source string, asJSON bool) error {
	if asJSON {
		out := make([]map[string]any, 0, len(recs))
		for _, r := range recs {
			out = append(out, map[string]any{
				"id": r.GetId(), "requested_name": r.GetRequestedName(),
				"fingerprint": r.GetFingerprint(), "state": r.GetState().String(),
				"decided_by": r.GetDecidedBy(), "issued_name": r.GetIssuedName(),
			})
		}
		return emitJSON(map[string]any{"source": source, "enrolments": out})
	}

	_, _ = fmt.Fprintln(w, source)
	if len(recs) == 0 {
		_, _ = fmt.Fprintln(w, "nothing waiting.")
		return nil
	}
	for _, r := range recs {
		_, _ = fmt.Fprintf(w, "\n  %s  %s  requested as %q\n",
			r.GetId(), strings.TrimPrefix(r.GetState().String(), "ENROLMENT_STATE_"), r.GetRequestedName())
		// The fingerprint on its own line, because it is the thing a
		// human has to compare character by character against what the
		// laptop printed.
		_, _ = fmt.Fprintf(w, "      %s\n", r.GetFingerprint())
		if r.GetDecidedBy() != "" {
			_, _ = fmt.Fprintf(w, "      decided by %s", r.GetDecidedBy())
			if r.GetIssuedName() != "" && r.GetIssuedName() != r.GetRequestedName() {
				_, _ = fmt.Fprintf(w, ", issued as %q", r.GetIssuedName())
			}
			_, _ = fmt.Fprintln(w)
		}
	}
	_, _ = fmt.Fprintf(w, "\nApprove with: lobslaw enrol approve <id> --fingerprint <the one they read you>\n")
	return nil
}

func enrolDecide(args []string, approve bool) error {
	verb := "deny"
	if approve {
		verb = "approve"
	}
	fs := flag.NewFlagSet("enrol "+verb, flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	name := fs.String("name", "", "issue under this name instead of the requested one")
	fingerprint := fs.String("fingerprint", "", "the fingerprint you verified; refuses if it does not match")
	validFor := fs.Duration("valid-for", 0, "certificate lifetime (0 = the node's default)")
	rest, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("exactly one request id required: lobslaw enrol %s <id>", verb)
	}
	if approve && strings.TrimSpace(*fingerprint) == "" {
		// Required for approval, optional for denial. An approval
		// without a verified fingerprint is somebody clicking yes to a
		// request they have not checked is the one they were told
		// about — which is the failure this whole flow exists to make
		// hard.
		return errors.New("--fingerprint is required to approve: read it back from whoever is enrolling, " +
			"and pass it here so a request that changed underneath you is refused")
	}

	conn, err := node.dial()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	client := lobslawv1.NewEnrolmentServiceClient(conn)

	req := &lobslawv1.DecideEnrolmentRequest{
		Id:          rest[0],
		Approve:     approve,
		Name:        strings.TrimSpace(*name),
		Fingerprint: strings.TrimSpace(*fingerprint),
	}
	if *validFor > 0 {
		req.ValidFor = durationpb.New(*validFor)
	}

	ctx, cancel := node.ctx()
	defer cancel()
	res, err := client.DecideEnrolment(ctx, req)
	if err != nil {
		return err
	}
	rec := res.GetEnrolment()
	if approve {
		fmt.Printf("Approved %s — issued to %q.\n", rec.GetId(), rec.GetIssuedName())
		fmt.Println("They can collect it with `lobslaw enrol status`.")
		return nil
	}
	fmt.Printf("Denied %s.\n", rec.GetId())
	return nil
}
