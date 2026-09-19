package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/sharing"
	"github.com/jmylchreest/lobslaw/internal/skills"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func skillsPublish(args []string) error {
	fs := newFlagSet("skills publish", flag.ContinueOnError)
	var node liveNode
	node.bind(fs)
	to := fs.String("to", "", "destination file:<path>")
	dir := fs.String("dir", "", "local skill directory (offline)")
	schedules := fs.String("schedules", "", "explicit comma-separated schedule IDs")
	inputs := fs.String("inputs", "", "declared comma-separated prompt inputs")
	timezone := fs.String("source-timezone", "", "timezone for schedules lacking CRON_TZ")
	rest, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if *to == "" {
		return errors.New("--to file:<path> is required")
	}
	var a sharing.Artifact
	if *dir != "" {
		if len(rest) != 0 || *schedules != "" || *inputs != "" {
			return errors.New("--dir cannot be combined with a stored skill, schedules or inputs")
		}
		a, err = shareDirectory(*dir)
	} else {
		if len(rest) < 1 || len(rest) > 2 {
			return errors.New("supply a stored skill name and optional version, or --dir")
		}
		client, closeConn, e := skillClient(&node)
		if e != nil {
			return e
		}
		defer closeConn()
		ctx, cancel := node.ctx()
		defer cancel()
		req := &lobslawv1.ExportShareRequest{Name: rest[0], ScheduleIds: shareCSV(*schedules), Inputs: shareCSV(*inputs), SourceTimezone: *timezone}
		if len(rest) == 2 {
			req.Version = rest[1]
		}
		resp, e := client.ExportShare(ctx, req)
		if e != nil {
			return explainUnimplemented(e, node.addr)
		}
		a, err = sharing.Decode(resp.Artifact)
	}
	if err != nil {
		return err
	}
	return publishShare(context.Background(), sharing.FileBackend{}, a, *to)
}

func shareDirectory(dir string) (sharing.Artifact, error) {
	bundle, err := memory.ReadBundle(dir)
	if err != nil {
		return sharing.Artifact{}, err
	}
	parsed, err := skills.ParseWithPolicy(dir, skills.SigningOff, nil)
	if err != nil {
		return sharing.Artifact{}, err
	}
	return sharing.Build(sharing.Package{Format: sharing.Format, Schema: 1, Name: parsed.Name(), Version: parsed.Manifest.Version, Manifest: bundle.Manifest, ManifestSignature: bundle.Signature, Files: bundle.Files})
}

func shareCSV(value string) []string {
	if value == "" {
		return nil
	}
	out := strings.Split(value, ",")
	for i := range out {
		out[i] = strings.TrimSpace(out[i])
	}
	return out
}

func publishShare(ctx context.Context, p sharing.Publisher, a sharing.Artifact, ref string) error {
	receipt, err := p.Publish(ctx, a, ref)
	if err != nil {
		return err
	}
	fmt.Printf("%s\n%s\n", receipt.Reference, receipt.Digest)
	return nil
}

func skillsInspect(args []string) error {
	fs := newFlagSet("skills inspect", flag.ContinueOnError)
	rest, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("supply file:<package>")
	}
	a, err := (sharing.FileBackend{}).Fetch(context.Background(), rest[0])
	if err != nil {
		return err
	}
	p := a.Package()
	// JSON escapes terminal control characters in untrusted manifests/files.
	files := make(map[string]string, len(p.Files))
	for path, raw := range p.Files {
		files[path] = string(raw)
	}
	return printShareJSON(struct {
		Digest, Publisher, SignatureStatus, Name, Version, Manifest string
		Files                                                       map[string]string
		Schedules                                                   []sharing.Schedule
		Inputs                                                      []string
	}{a.Digest(), a.Publisher(), "not verified; destination trust policy is checked on install", p.Name, p.Version, string(p.Manifest), files, p.Schedules, p.Inputs})
}

func skillsSign(args []string) error {
	fs := newFlagSet("skills sign", flag.ContinueOnError)
	keyPath := fs.String("key", "", "base64 Ed25519 private key file (64 bytes decoded)")
	publisher := fs.String("publisher", "", "publisher identity in destination trust store")
	to := fs.String("to", "", "new destination file:<path>")
	manifest := fs.Bool("sign-manifest", false, "also sign the unchanged manifest (requires pinned file digests)")
	rest, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 || *keyPath == "" || *publisher == "" || *to == "" {
		return errors.New("supply --key, --publisher, --to and file:<source>")
	}
	raw, err := os.ReadFile(*keyPath)
	if err != nil {
		return err
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return errors.New("key must contain a base64 Ed25519 private key (64 bytes)")
	}
	a, err := (sharing.FileBackend{}).Fetch(context.Background(), rest[0])
	if err != nil {
		return err
	}
	if *manifest {
		p := a.Package()
		p.ManifestSignature = ed25519.Sign(ed25519.PrivateKey(key), p.Manifest)
		a, err = sharing.Build(p)
		if err != nil {
			return err
		}
	}
	a, err = sharing.Sign(a, *publisher, ed25519.PrivateKey(key))
	if err != nil {
		return err
	}
	return publishShare(context.Background(), sharing.FileBackend{}, a, *to)
}

type shareInputs map[string]string

func (v *shareInputs) String() string { return "key=value" }
func (v *shareInputs) Set(raw string) error {
	key, value, ok := strings.Cut(raw, "=")
	if !ok || key == "" {
		return errors.New("input must be key=value")
	}
	if *v == nil {
		*v = make(map[string]string)
	}
	if _, ok := (*v)[key]; ok {
		return fmt.Errorf("duplicate input %q", key)
	}
	(*v)[key] = value
	return nil
}

func skillsInstall(args []string) error {
	fs := newFlagSet("skills install", flag.ContinueOnError)
	var node liveNode
	node.bind(fs)
	owner := fs.String("owner", "", "destination user:<id>")
	apply := fs.Bool("apply", false, "apply the reviewed plan")
	expected := fs.String("expected-plan", "", "digest from preview")
	var inputs shareInputs
	fs.Var(&inputs, "input", "prompt binding key=value (repeatable)")
	rest, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 || *owner == "" {
		return errors.New("supply --owner user:<id> and file:<package>")
	}
	a, err := (sharing.FileBackend{}).Fetch(context.Background(), rest[0])
	if err != nil {
		return err
	}
	client, closeConn, err := skillClient(&node)
	if err != nil {
		return err
	}
	defer closeConn()
	ctx, cancel := node.ctx()
	defer cancel()
	resp, err := client.InstallShare(ctx, &lobslawv1.InstallShareRequest{Artifact: a.Bytes(), Owner: *owner, Inputs: inputs, Apply: *apply, ExpectedPlan: *expected})
	if err != nil {
		return explainUnimplemented(err, node.addr)
	}
	return printSharePlan(resp.PlanJson)
}

func skillsActivateInstall(args []string) error {
	fs := newFlagSet("skills activate-install", flag.ContinueOnError)
	var node liveNode
	node.bind(fs)
	owner := fs.String("owner", "", "destination user:<id>")
	apply := fs.Bool("apply", false, "apply the reviewed activation")
	expected := fs.String("expected-plan", "", "digest from preview")
	rest, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 || *owner == "" {
		return errors.New("supply --owner user:<id> and installation ID")
	}
	client, closeConn, err := skillClient(&node)
	if err != nil {
		return err
	}
	defer closeConn()
	ctx, cancel := node.ctx()
	defer cancel()
	resp, err := client.ActivateShare(ctx, &lobslawv1.ActivateShareRequest{InstallationId: rest[0], Owner: *owner, Apply: *apply, ExpectedPlan: *expected})
	if err != nil {
		return explainUnimplemented(err, node.addr)
	}
	return printSharePlan(resp.PlanJson)
}

func printShareJSON(value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return printSharePlan(raw)
}
func printSharePlan(raw []byte) error {
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return err
	}
	fmt.Println(out.String())
	return nil
}
