// Package sharing defines portable skill releases independently of their host.
// Backends transport bytes; verification and installation remain caller concerns.
package sharing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

const (
	Format = "lobslaw-skill-share"
	// Keeps unary RPCs and the entire installation transaction bounded.
	MaxBytes     = 1 << 20
	MaxFiles     = 128
	MaxSchedules = 32
)

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
var inputPattern = regexp.MustCompile(`\{\{([a-zA-Z0-9_.-]+)\}\}`)

type Package struct {
	Format            string            `json:"format"`
	Schema            uint32            `json:"schema"`
	Name              string            `json:"name"`
	Version           string            `json:"version"`
	Manifest          []byte            `json:"manifest"`
	ManifestSignature []byte            `json:"manifest_signature,omitempty"`
	Files             map[string][]byte `json:"files"`
	Schedules         []Schedule        `json:"schedules,omitempty"`
	Inputs            []string          `json:"inputs,omitempty"`
	Origin            *Origin           `json:"origin,omitempty"`
}

// Origin is provenance, not a verified publisher identity or a policy grant.
type Origin struct {
	Reference string `json:"reference"`
	Catalog   string `json:"catalog"`
	Digest    string `json:"digest"`
	Format    string `json:"format"`
}

// Schedule is a template, not a runtime record. Source owners, addresses,
// claims, approvals and run history have no representation in this format.
type Schedule struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	Cron     string `json:"cron"`
	Timezone string `json:"timezone"`
	Prompt   string `json:"prompt"`
	NotifyOn string `json:"notify_on"`
}

type envelope struct {
	Package   Package `json:"package"`
	Digest    string  `json:"digest"`
	Publisher string  `json:"publisher,omitempty"`
	Signature []byte  `json:"signature,omitempty"`
}

// Artifact has no exported mutable content. Backends receive a complete,
// validated envelope and cannot silently discard schedule declarations.
type Artifact struct {
	raw    []byte
	digest string
}

func (a Artifact) Bytes() []byte    { return bytes.Clone(a.raw) }
func (a Artifact) Digest() string   { return a.digest }
func (a Artifact) Package() Package { var e envelope; _ = json.Unmarshal(a.raw, &e); return e.Package }
func (a Artifact) Publisher() string {
	var e envelope
	_ = json.Unmarshal(a.raw, &e)
	return e.Publisher
}

func Hash(raw []byte) string { h := sha256.Sum256(raw); return "sha256:" + hex.EncodeToString(h[:]) }

func Build(p Package) (Artifact, error) {
	if err := validate(p); err != nil {
		return Artifact{}, err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return Artifact{}, err
	}
	return encode(envelope{Package: p, Digest: Hash(body)})
}

func encode(e envelope) (Artifact, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return Artifact{}, err
	}
	if len(raw) > MaxBytes {
		return Artifact{}, errors.New("sharing: package exceeds 1 MiB encoded limit")
	}
	return Artifact{raw: raw, digest: e.Digest}, nil
}

func Decode(raw []byte) (Artifact, error) {
	if len(raw) > MaxBytes {
		return Artifact{}, errors.New("sharing: package exceeds size limit")
	}
	var e envelope
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&e); err != nil {
		return Artifact{}, fmt.Errorf("sharing: decode: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return Artifact{}, errors.New("sharing: trailing content")
	}
	a, err := Build(e.Package)
	if err != nil {
		return Artifact{}, err
	}
	if a.digest != e.Digest {
		return Artifact{}, errors.New("sharing: content digest mismatch")
	}
	if (e.Publisher == "") != (len(e.Signature) == 0) || (len(e.Signature) > 0 && len(e.Signature) != ed25519.SignatureSize) {
		return Artifact{}, errors.New("sharing: malformed signature")
	}
	canonical, err := encode(e)
	if err != nil {
		return Artifact{}, err
	}
	// One encoding: rejects duplicate keys, ambiguous parsings and unsigned extras.
	if !bytes.Equal(bytes.TrimSpace(raw), canonical.raw) {
		return Artifact{}, errors.New("sharing: noncanonical envelope")
	}
	return canonical, nil
}

func validate(p Package) error {
	if err := validateOrigin(p.Origin); err != nil {
		return err
	}
	if p.Format != Format || p.Schema != 1 {
		return errors.New("sharing: unsupported package format or schema")
	}
	if !identifier.MatchString(p.Name) || !identifier.MatchString(p.Version) || len(p.Manifest) == 0 {
		return errors.New("sharing: invalid skill identity or empty manifest")
	}
	if len(p.Files) > MaxFiles || len(p.Schedules) > MaxSchedules || len(p.Inputs) > 64 {
		return errors.New("sharing: too many files, schedules or inputs")
	}
	for name := range p.Files {
		if !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, "\\\x00:") || name == "manifest.yaml" || name == "manifest.yaml.sig" {
			return fmt.Errorf("sharing: unsafe or reserved file %q", name)
		}
	}
	inputs := make(map[string]bool)
	for _, key := range p.Inputs {
		if !identifier.MatchString(key) || inputs[key] {
			return errors.New("sharing: invalid or duplicate input")
		}
		inputs[key] = true
	}
	seen := make(map[string]bool)
	for _, s := range p.Schedules {
		if !identifier.MatchString(s.Key) || seen[s.Key] || s.Name == "" || s.Prompt == "" || len(s.Prompt) > 16<<10 {
			return errors.New("sharing: invalid or duplicate schedule")
		}
		seen[s.Key] = true
		if err := validateSchedule(s, inputs); err != nil {
			return err
		}
	}
	return nil
}

func validateSchedule(s Schedule, inputs map[string]bool) error {
	if s.Timezone == "" || s.Timezone == "Local" || strings.ContainsAny(s.Timezone, " \t\r\n") || strings.Contains(s.Cron, "TZ=") {
		return errors.New("sharing: schedule requires a separate explicit timezone")
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return fmt.Errorf("sharing: timezone: %w", err)
	}
	if _, err := cron.ParseStandard("CRON_TZ=" + s.Timezone + " " + s.Cron); err != nil {
		return fmt.Errorf("sharing: cron: %w", err)
	}
	if s.NotifyOn != "always" && s.NotifyOn != "match" && s.NotifyOn != "never" {
		return errors.New("sharing: invalid notification mode")
	}
	for _, m := range inputPattern.FindAllStringSubmatch(s.Prompt, -1) {
		if !inputs[m[1]] {
			return fmt.Errorf("sharing: undeclared input %q", m[1])
		}
	}
	return nil
}

// Bind only substitutes declared prompt inputs. It never rewrites signed files,
// evaluates templates, or interprets input as a shell command.
func Bind(p Package, inputs map[string]string) ([]Schedule, error) {
	if err := validate(p); err != nil {
		return nil, err
	}
	if len(inputs) != len(p.Inputs) {
		return nil, errors.New("sharing: supply exactly the declared inputs")
	}
	for _, key := range p.Inputs {
		if value, ok := inputs[key]; !ok || value == "" || len(value) > 4096 {
			return nil, fmt.Errorf("sharing: input %q is required and bounded", key)
		}
	}
	out := append([]Schedule(nil), p.Schedules...)
	for i := range out {
		out[i].Prompt = inputPattern.ReplaceAllStringFunc(out[i].Prompt, func(key string) string { return inputs[key[2:len(key)-2]] })
		if len(out[i].Prompt) > 32<<10 {
			return nil, errors.New("sharing: bound prompt too large")
		}
	}
	return out, nil
}

func signingBytes(digest, publisher string) []byte {
	return []byte(Format + "/signature/v1\n" + publisher + "\n" + digest + "\n")
}

func Sign(a Artifact, publisher string, key ed25519.PrivateKey) (Artifact, error) {
	if !identifier.MatchString(publisher) || len(key) != ed25519.PrivateKeySize {
		return Artifact{}, errors.New("sharing: publisher and Ed25519 private key required")
	}
	valid, err := Decode(a.raw)
	if err != nil {
		return Artifact{}, err
	}
	return encode(envelope{Package: valid.Package(), Digest: valid.digest, Publisher: publisher, Signature: ed25519.Sign(key, signingBytes(valid.digest, publisher))})
}

func Verify(a Artifact, keys map[string]ed25519.PublicKey, required bool) error {
	if _, err := Decode(a.raw); err != nil {
		return err
	}
	var e envelope
	_ = json.Unmarshal(a.raw, &e)
	if e.Publisher == "" {
		if required {
			return errors.New("sharing: publisher signature required")
		}
		return nil
	}
	key := keys[e.Publisher]
	if len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, signingBytes(e.Digest, e.Publisher), e.Signature) {
		return errors.New("sharing: publisher is untrusted or signature is invalid")
	}
	return nil
}

// VerifyWith uses the existing trust store without coupling the format to it.
func VerifyWith(a Artifact, required bool, verify func([]byte, []byte) (string, bool)) error {
	if _, err := Decode(a.raw); err != nil {
		return err
	}
	var e envelope
	_ = json.Unmarshal(a.raw, &e)
	if e.Publisher == "" {
		if required {
			return errors.New("sharing: publisher signature required")
		}
		return nil
	}
	if verify != nil {
		if signer, ok := verify(signingBytes(e.Digest, e.Publisher), e.Signature); ok && signer == e.Publisher {
			return nil
		}
	}
	return errors.New("sharing: publisher is untrusted or signature is invalid")
}

func validateOrigin(origin *Origin) error {
	if origin != nil && (len(origin.Reference) > 512 || len(origin.Catalog) > 2048 || len(origin.Format) > 32 || len(origin.Digest) != 71 || !strings.HasPrefix(origin.Digest, "sha256:")) {
		return errors.New("sharing: invalid source provenance")
	}
	return nil
}
