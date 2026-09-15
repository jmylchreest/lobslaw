package node

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"log/slog"
	"testing"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/archive"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/internal/skills"
	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestArchiveAuthorizationRequiresCertificateRoleAndDataGrant(t *testing.T) {
	store := crossOwnerTestStore(t)
	n := &Node{policyEngine: policy.NewEngine(store, slog.Default())}
	n.cfg.Users = []config.UserConfig{{ID: "alice", Roles: []string{"operator"}}}
	cert := &x509.Certificate{Subject: pkix.Name{
		CommonName: "alice", OrganizationalUnit: []string{mtls.OperatorOU},
	}}
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}},
	})
	if err := n.authorizeArchive(ctx, "archive:import"); err == nil {
		t.Fatal("machine operator certificate granted personal data authority")
	}
	seedRule(t, store, &lobslawv1.PolicyRule{
		Id: "archive-operators", Subject: "role:operator", Action: "archive:import",
		Resource: "memory:*", Effect: "allow", Priority: 50,
	})
	if err := n.authorizeArchive(ctx, "archive:import"); err != nil {
		t.Fatal(err)
	}
	if err := n.authorizeArchive(context.Background(), "archive:import"); err == nil {
		t.Fatal("unverified caller accepted")
	}
	if err := n.authorizeArchive(ctx, "archive:export"); err == nil {
		t.Fatal("import grant conferred export authority")
	}
	n.cfg.Users = nil
	if err := n.authorizeArchive(ctx, "archive:import"); err == nil {
		t.Fatal("certificate conferred the operator data role")
	}
}

func TestArchiveRestoreModeDoesNotStartGatewaysOrScheduler(t *testing.T) {
	cfg := Config{
		RestoreMode: true,
		Functions:   []types.NodeFunction{types.FunctionCompute, types.FunctionMemory},
	}
	cfg.Gateway.Enabled = true
	if gateGateway(cfg) {
		t.Fatal("restore mode started a gateway")
	}
	for _, stage := range nodeWireStages() {
		if stage.Name == "scheduler" && (stage.Gate == nil || stage.Gate(cfg)) {
			t.Fatal("restore mode started the scheduler")
		}
	}
}

func TestArchiveSkillsValidateOriginalSignatureAndIdentity(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	verifier := skills.NewVerifier()
	if err := verifier.AddKey("publisher", public); err != nil {
		t.Fatal(err)
	}
	n := &Node{skillSigningPolicy: skills.SigningOff, skillVerifier: verifier}
	handler := []byte("print('hello')")
	manifest := []byte(plainManifest(string(handler)))
	skill := &lobslawv1.SkillRecord{
		Name: "tidy", Version: "1.2.3", Tier: lobslawv1.SkillTier_SKILL_TIER_SIGNED,
		ManifestYaml: manifest, ManifestSig: ed25519.Sign(private, manifest),
		Files: map[string]string{"handler.py": memory.Digest(handler)},
	}
	record := func(kind, id string, msg proto.Message) archive.Record {
		t.Helper()
		data, err := protojson.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		return archive.Record{Kind: kind, ID: id, Data: data}
	}
	blob := record("skill-blobs", memory.Digest(handler), &lobslawv1.SkillBlob{Digest: memory.Digest(handler), Content: handler})
	ctx := context.Background()
	if err := n.validateArchiveSkills(ctx, []archive.Record{blob, record("skills", "tidy@1.2.3", skill)}); err != nil {
		t.Fatal(err)
	}
	skill.Name = "impersonated"
	if err := n.validateArchiveSkills(ctx, []archive.Record{blob, record("skills", "impersonated@1.2.3", skill)}); err == nil {
		t.Fatal("record identity disagreed with the signed manifest")
	}
	skill.Name = "tidy"
	skill.ManifestYaml = append(skill.ManifestYaml, []byte("# modified")...)
	if err := n.validateArchiveSkills(ctx, []archive.Record{blob, record("skills", "tidy@1.2.3", skill)}); err == nil {
		t.Fatal("unverified signed tier accepted when destination signing policy is off")
	}
}
