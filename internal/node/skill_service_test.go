package node

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/skills"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Installing skills on a running cluster. The BYTES travel, not a
// path: the CLI runs on somebody's laptop and the cluster is
// elsewhere, so a service taking a directory would be reading one that
// exists perfectly well on the wrong machine.

func skillSvc(t *testing.T, policy skills.SigningPolicy, verifier *skills.Verifier) *skillService {
	t.Helper()
	svc, _ := skillSvcNode(t, policy, verifier)
	return svc
}

func skillSvcNode(t *testing.T, policy skills.SigningPolicy, verifier *skills.Verifier) (*skillService, *Node) {
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
	_, inmem := raft.NewInmemTransport("svc-node")
	node, err := memory.NewRaft(memory.RaftConfig{
		NodeID: "svc-node", LocalAddr: "svc-node",
		DataDir: dir, Bootstrap: true, Transport: inmem,
	}, memory.NewFSM(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = node.Shutdown()
		_ = store.Close()
	})
	if err := node.WaitForLeader(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	skillStore, err := memory.NewSkillStore(node, store)
	if err != nil {
		t.Fatal(err)
	}
	return &skillService{store: skillStore, policy: policy, verifier: verifier, sharing: memory.NewSharingStore(node, store)}, &Node{raft: node, store: store, skillStore: skillStore, skillSigningPolicy: policy, skillVerifier: verifier}
}

func bundleRequest(manifest, handler string) *lobslawv1.ImportSkillRequest {
	return &lobslawv1.ImportSkillRequest{
		Name: "tidy", Version: "1.2.3",
		Tier:         lobslawv1.SkillTier_SKILL_TIER_OPERATOR,
		ManifestYaml: []byte(manifest),
		Files:        map[string][]byte{"handler.py": []byte(handler)},
		Activate:     true,
	}
}

func plainManifest(handler string) string {
	sum := sha256.Sum256([]byte(handler))
	return "name: tidy\nversion: 1.2.3\nruntime: python\nhandler: handler.py\n" +
		"handler_sha256: " + hex.EncodeToString(sum[:]) + "\n"
}

func TestABundleImportsOverTheWire(t *testing.T) {
	t.Parallel()
	svc := skillSvc(t, skills.SigningOff, nil)
	resp, err := svc.ImportSkill(context.Background(), bundleRequest(plainManifest("print('hi')"), "print('hi')"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetSkill().GetName() != "tidy" || !resp.GetSkill().GetActive() {
		t.Errorf("record = %+v", resp.GetSkill())
	}
}

// Import then export must produce the same bytes, or a signed skill
// cannot survive the trip.
func TestTheRoundTripOverTheServiceIsByteIdentical(t *testing.T) {
	t.Parallel()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := "print('hi')"
	// Not already-normalised: a comment and trailing whitespace, both
	// of which a re-encode anywhere on this path would tidy away.
	sum := sha256.Sum256([]byte(handler))
	manifest := "# publisher: example.com\nversion: 1.2.3\nname: tidy\n" +
		"runtime: python\nhandler: handler.py\n" +
		"handler_sha256: " + hex.EncodeToString(sum[:]) + "   \n"
	sig := ed25519.Sign(priv, []byte(manifest))

	verifier := skills.NewVerifier()
	if err := verifier.AddKey("example", pub); err != nil {
		t.Fatal(err)
	}
	svc := skillSvc(t, skills.SigningRequire, verifier)

	req := bundleRequest(manifest, handler)
	req.Tier = lobslawv1.SkillTier_SKILL_TIER_SIGNED
	req.ManifestSig = sig
	if _, err := svc.ImportSkill(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	out, err := svc.ExportSkill(context.Background(),
		&lobslawv1.ExportSkillRequest{Name: "tidy", Version: "1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out.GetManifestYaml()) != manifest {
		t.Errorf("manifest changed:\n got %q\nwant %q", out.GetManifestYaml(), manifest)
	}
	if !ed25519.Verify(pub, out.GetManifestYaml(), out.GetManifestSig()) {
		t.Error("the signature no longer verifies after a round trip through the service")
	}
	if string(out.GetFiles()["handler.py"]) != handler {
		t.Errorf("handler = %q", out.GetFiles()["handler.py"])
	}
}

// --- validation at the door -------------------------------------------

// Parsed through the REAL loader before storing, so an import is held
// to exactly the standard a load is. Otherwise the skill replicates to
// every node and fails to load on all of them.
func TestAnUnsignedBundleIsRefusedUnderSigningRequire(t *testing.T) {
	t.Parallel()
	svc := skillSvc(t, skills.SigningRequire, skills.NewVerifier())
	_, err := svc.ImportSkill(context.Background(),
		bundleRequest(plainManifest("print('hi')"), "print('hi')"))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v (err = %v)", status.Code(err), err)
	}
}

// A signed manifest pinning no handler digest covers no executable
// content. Verifying the signature by hand here would admit it; going
// through ParseWithPolicy does not.
func TestASignedBundleWithNoHandlerDigestIsRefused(t *testing.T) {
	t.Parallel()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	manifest := "name: tidy\nversion: 1.2.3\nruntime: python\nhandler: handler.py\n"
	verifier := skills.NewVerifier()
	if err := verifier.AddKey("example", pub); err != nil {
		t.Fatal(err)
	}
	svc := skillSvc(t, skills.SigningRequire, verifier)

	req := bundleRequest(manifest, "print('hi')")
	req.ManifestSig = ed25519.Sign(priv, []byte(manifest))
	_, err = svc.ImportSkill(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v (err = %v)", status.Code(err), err)
	}
	if !strings.Contains(err.Error(), "handler_sha256") {
		t.Errorf("err = %q; it does not say what is missing", err)
	}
}

// A bundle arriving over the wire is less trustworthy than one read
// from a local directory, not more.
func TestABundlePathCannotEscape(t *testing.T) {
	t.Parallel()
	svc := skillSvc(t, skills.SigningOff, nil)
	req := bundleRequest(plainManifest("print('hi')"), "print('hi')")
	req.Files["../escape.sh"] = []byte("rm -rf /")

	_, err := svc.ImportSkill(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v (err = %v)", status.Code(err), err)
	}
}

func TestImportRequiresNameVersionAndManifest(t *testing.T) {
	t.Parallel()
	svc := skillSvc(t, skills.SigningOff, nil)
	base := bundleRequest(plainManifest("x"), "x")
	for name, mutate := range map[string]func(*lobslawv1.ImportSkillRequest){
		"no name":     func(r *lobslawv1.ImportSkillRequest) { r.Name = "" },
		"no version":  func(r *lobslawv1.ImportSkillRequest) { r.Version = "" },
		"no manifest": func(r *lobslawv1.ImportSkillRequest) { r.ManifestYaml = nil },
	} {
		req := bundleRequest(string(base.GetManifestYaml()), "x")
		mutate(req)
		if _, err := svc.ImportSkill(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: code = %v", name, status.Code(err))
		}
	}
}

// --- listing and removal ----------------------------------------------

func TestListAndRemove(t *testing.T) {
	t.Parallel()
	svc := skillSvc(t, skills.SigningOff, nil)
	ctx := context.Background()
	if _, err := svc.ImportSkill(ctx, bundleRequest(plainManifest("x"), "x")); err != nil {
		t.Fatal(err)
	}

	listed, err := svc.ListSkills(ctx, &lobslawv1.ListSkillsRequest{ActiveOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.GetSkills()) != 1 {
		t.Fatalf("listed %d", len(listed.GetSkills()))
	}

	if _, err := svc.RemoveSkill(ctx, &lobslawv1.RemoveSkillRequest{
		Name: "tidy", Version: "1.2.3",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ExportSkill(ctx, &lobslawv1.ExportSkillRequest{
		Name: "tidy", Version: "1.2.3",
	}); status.Code(err) != codes.NotFound {
		t.Errorf("code = %v after removal, want NotFound", status.Code(err))
	}
}

// "Not found" and "too large" send an operator to different places —
// one is a typo, the other is a bundle needing a file moved to
// storage.
func TestSkillStoreErrorsMapToDistinctCodes(t *testing.T) {
	t.Parallel()
	svc := skillSvc(t, skills.SigningOff, nil)
	_, err := svc.ExportSkill(context.Background(),
		&lobslawv1.ExportSkillRequest{Name: "nope", Version: "1.0.0"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("unknown skill: code = %v", status.Code(err))
	}
}

// A node without raft has no store, and every method has to say so
// rather than panicking on a nil.
func TestEveryMethodRefusesWithoutAStore(t *testing.T) {
	t.Parallel()
	svc := &skillService{}
	ctx := context.Background()
	calls := map[string]func() error{
		"import": func() error {
			_, err := svc.ImportSkill(ctx, bundleRequest(plainManifest("x"), "x"))
			return err
		},
		"export": func() error {
			_, err := svc.ExportSkill(ctx, &lobslawv1.ExportSkillRequest{Name: "a", Version: "1"})
			return err
		},
		"list": func() error {
			_, err := svc.ListSkills(ctx, &lobslawv1.ListSkillsRequest{})
			return err
		},
		"remove": func() error {
			_, err := svc.RemoveSkill(ctx, &lobslawv1.RemoveSkillRequest{Name: "a", Version: "1"})
			return err
		},
	}
	for name, call := range calls {
		if code := status.Code(call()); code != codes.FailedPrecondition {
			t.Errorf("%s: code = %v, want FailedPrecondition", name, code)
		}
	}
}

// --- rollback ----------------------------------------------------------

// Every version imported is still in the log, so going back to one is
// a matter of saying which — no bundle, no re-import, no re-parse.

func importVersion(t *testing.T, svc *skillService, version, handler string) {
	t.Helper()
	sum := sha256.Sum256([]byte(handler))
	manifest := "name: tidy\nversion: " + version + "\nruntime: python\nhandler: handler.py\n" +
		"handler_sha256: " + hex.EncodeToString(sum[:]) + "\n"
	req := bundleRequest(manifest, handler)
	req.Version = version
	if _, err := svc.ImportSkill(context.Background(), req); err != nil {
		t.Fatalf("import %s: %v", version, err)
	}
}

func activeVersion(t *testing.T, svc *skillService) string {
	t.Helper()
	listed, err := svc.ListSkills(context.Background(), &lobslawv1.ListSkillsRequest{ActiveOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.GetSkills()) != 1 {
		t.Fatalf("%d versions in force, want exactly 1", len(listed.GetSkills()))
	}
	return listed.GetSkills()[0].GetVersion()
}

func TestRollbackRestoresAPriorVersion(t *testing.T) {
	t.Parallel()
	svc := skillSvc(t, skills.SigningOff, nil)
	importVersion(t, svc, "1.0.0", "print('one')")
	importVersion(t, svc, "2.0.0", "print('two')")

	if got := activeVersion(t, svc); got != "2.0.0" {
		t.Fatalf("active = %q before rollback", got)
	}
	resp, err := svc.ActivateSkill(context.Background(),
		&lobslawv1.ActivateSkillRequest{Name: "tidy", Version: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetAlreadyActive() {
		t.Error("reported already-active for a version that was not in force")
	}
	if got := activeVersion(t, svc); got != "1.0.0" {
		t.Errorf("active = %q after rollback, want 1.0.0", got)
	}
}

// Exactly one version in force. Leaving the old one active would give
// the loader two candidates at the same tier and let the tiebreak, not
// the operator, decide which runs.
func TestRollbackLeavesExactlyOneVersionInForce(t *testing.T) {
	t.Parallel()
	svc := skillSvc(t, skills.SigningOff, nil)
	for _, v := range []string{"1.0.0", "2.0.0", "3.0.0"} {
		importVersion(t, svc, v, "print('"+v+"')")
	}
	if _, err := svc.ActivateSkill(context.Background(),
		&lobslawv1.ActivateSkillRequest{Name: "tidy", Version: "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	// activeVersion fatals unless exactly one is in force.
	if got := activeVersion(t, svc); got != "1.0.0" {
		t.Errorf("active = %q", got)
	}
}

// The versions rolled back FROM must still be there, or a rollback is
// a one-way door and rolling forward again means re-importing.
func TestRollbackDoesNotDeleteTheVersionItLeft(t *testing.T) {
	t.Parallel()
	svc := skillSvc(t, skills.SigningOff, nil)
	importVersion(t, svc, "1.0.0", "print('one')")
	importVersion(t, svc, "2.0.0", "print('two')")
	if _, err := svc.ActivateSkill(context.Background(),
		&lobslawv1.ActivateSkillRequest{Name: "tidy", Version: "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ExportSkill(context.Background(),
		&lobslawv1.ExportSkillRequest{Name: "tidy", Version: "2.0.0"}); err != nil {
		t.Errorf("2.0.0 is gone after rolling back off it: %v", err)
	}
	// And rolling forward again works without re-importing.
	if _, err := svc.ActivateSkill(context.Background(),
		&lobslawv1.ActivateSkillRequest{Name: "tidy", Version: "2.0.0"}); err != nil {
		t.Errorf("cannot roll forward again: %v", err)
	}
	if got := activeVersion(t, svc); got != "2.0.0" {
		t.Errorf("active = %q after rolling forward", got)
	}
}

// An operator scripting a rollback should not have to special-case
// having already done it, and an error there would be
// indistinguishable from a rollback that failed.
func TestRollingBackToTheVersionInForceSucceeds(t *testing.T) {
	t.Parallel()
	svc := skillSvc(t, skills.SigningOff, nil)
	importVersion(t, svc, "1.0.0", "print('one')")

	resp, err := svc.ActivateSkill(context.Background(),
		&lobslawv1.ActivateSkillRequest{Name: "tidy", Version: "1.0.0"})
	if err != nil {
		t.Fatalf("rolling back to the current version failed: %v", err)
	}
	if !resp.GetAlreadyActive() {
		t.Error("did not report that the version was already in force")
	}
	if got := activeVersion(t, svc); got != "1.0.0" {
		t.Errorf("active = %q", got)
	}
}

func TestRollbackToAnUnknownVersionIsNotFound(t *testing.T) {
	t.Parallel()
	svc := skillSvc(t, skills.SigningOff, nil)
	importVersion(t, svc, "1.0.0", "print('one')")
	_, err := svc.ActivateSkill(context.Background(),
		&lobslawv1.ActivateSkillRequest{Name: "tidy", Version: "9.9.9"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("code = %v, want NotFound", status.Code(err))
	}
}

// Rolling back is how an operator ESCAPES a tightened signing policy
// that its current version no longer satisfies. Re-validating here
// would refuse the escape.
func TestRollbackDoesNotRevalidateAgainstThePolicy(t *testing.T) {
	t.Parallel()
	svc := skillSvc(t, skills.SigningOff, nil)
	importVersion(t, svc, "1.0.0", "print('one')")
	importVersion(t, svc, "2.0.0", "print('two')")

	// The policy tightens after both versions are already stored —
	// neither would import now.
	svc.policy = skills.SigningRequire
	svc.verifier = skills.NewVerifier()

	if _, err := svc.ActivateSkill(context.Background(),
		&lobslawv1.ActivateSkillRequest{Name: "tidy", Version: "1.0.0"}); err != nil {
		t.Fatalf("a tightened policy blocked a rollback to an already-stored version: %v", err)
	}
	if got := activeVersion(t, svc); got != "1.0.0" {
		t.Errorf("active = %q", got)
	}
}

func TestActivateRefusesWithoutAStore(t *testing.T) {
	t.Parallel()
	svc := &skillService{}
	_, err := svc.ActivateSkill(context.Background(),
		&lobslawv1.ActivateSkillRequest{Name: "a", Version: "1"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", status.Code(err))
	}
}

// --- size limits -------------------------------------------------------

// An oversized payload must fail at import NAMING THE PATH.
//
// The gRPC receive ceiling was raised above the bundle limit precisely
// so this error is the one that surfaces: at the default limits a
// bundle at the cap is exactly gRPC's own default, and the operator
// would get "message too large" — true, unactionable, and naming
// nothing. The error that helps says which file.
func TestAnOversizedFileIsRefusedNamingThePath(t *testing.T) {
	t.Parallel()
	svc := skillSvc(t, skills.SigningOff, nil)
	svc.store.SetLimits(1024, 1<<20)

	req := bundleRequest(plainManifest("print('hi')"), "print('hi')")
	req.Files["vendor/blob.bin"] = make([]byte, 2048)

	_, err := svc.ImportSkill(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v (err = %v)", status.Code(err), err)
	}
	if !strings.Contains(err.Error(), "vendor/blob.bin") {
		t.Errorf("err = %q; it does not name the offending file", err)
	}
}

// The whole-bundle limit is a separate bound: many small files can
// exceed it while no single one does, and its message says so rather
// than pointing at an innocent file.
func TestAnOversizedBundleIsRefused(t *testing.T) {
	t.Parallel()
	svc := skillSvc(t, skills.SigningOff, nil)
	svc.store.SetLimits(1024, 2048)

	req := bundleRequest(plainManifest("print('hi')"), "print('hi')")
	for _, name := range []string{"a.bin", "b.bin", "c.bin"} {
		req.Files[name] = make([]byte, 900)
	}
	_, err := svc.ImportSkill(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v (err = %v)", status.Code(err), err)
	}
	if !strings.Contains(err.Error(), "bundle") {
		t.Errorf("err = %q; it should name the bundle, not a file", err)
	}
}

// A bundle at the limit still imports. A cap that refuses what it
// permits is worse than no cap, and the raised gRPC ceiling exists so
// this case reaches the store at all.
func TestABundleAtTheLimitStillImports(t *testing.T) {
	t.Parallel()
	svc := skillSvc(t, skills.SigningOff, nil)
	handler := "print('hi')"
	manifest := plainManifest(handler)
	svc.store.SetLimits(4096, len(manifest)+len(handler)+4096)

	req := bundleRequest(manifest, handler)
	req.Files["pad.bin"] = make([]byte, 4096)
	if _, err := svc.ImportSkill(context.Background(), req); err != nil {
		t.Errorf("a bundle at the limit was refused: %v", err)
	}
}
