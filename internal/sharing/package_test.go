package sharing

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
)

func example() Package {
	return Package{Format: Format, Schema: 1, Name: "weather", Version: "0.0.0",
		Manifest:  []byte("# exact bytes\nname: weather\nruntime: prose\nbody: SKILL.md\n"),
		Files:     map[string][]byte{"SKILL.md": []byte("Check the weather.")},
		Schedules: []Schedule{{Key: "check", Name: "Weather check", Cron: "0 */3 * * *", Timezone: "Europe/London", Prompt: "Use weather for {{location}}", NotifyOn: "never"}},
		Inputs:    []string{"location"}}
}

func TestPackageRoundTripAndTampering(t *testing.T) {
	p := example()
	a, err := Build(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(a.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Package().Manifest, p.Manifest) || got.Digest() != a.Digest() {
		t.Fatal("content changed")
	}
	bad := bytes.Replace(a.Bytes(), []byte("Weather check"), []byte("Other check!!"), 1)
	if _, err := Decode(bad); err == nil {
		t.Fatal("tampered content accepted")
	}
	copy := a.Bytes()
	copy[0] = '!'
	if _, err := Decode(a.Bytes()); err != nil {
		t.Fatal("artifact bytes are mutable")
	}
}

func TestSignedPackageCoversSchedules(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Build(example())
	if err != nil {
		t.Fatal(err)
	}
	a, err = Sign(a, "alice", priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(a, map[string]ed25519.PublicKey{"alice": pub}, true); err != nil {
		t.Fatal(err)
	}
	if err := Verify(a, nil, true); err == nil {
		t.Fatal("unknown publisher trusted")
	}
	p := a.Package()
	p.Schedules[0].Prompt = "Do something else"
	b, err := Build(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(b, map[string]ed25519.PublicKey{"alice": pub}, true); err == nil {
		t.Fatal("unsigned rebuilt content trusted")
	}
}

func TestRejectUnsafeAndUnboundContent(t *testing.T) {
	for _, name := range []string{"../secret", "/secret", `a\b`, "manifest.yaml", "a/../b", ".", "a\x00b"} {
		t.Run(name, func(t *testing.T) {
			p := example()
			p.Files[name] = []byte("bad")
			if _, err := Build(p); err == nil {
				t.Fatal("unsafe file accepted")
			}
		})
	}
	p := example()
	p.Inputs = nil
	if _, err := Build(p); err == nil {
		t.Fatal("undeclared input accepted")
	}
	p = example()
	p.Schedules[0].Timezone = ""
	if _, err := Build(p); err == nil {
		t.Fatal("implicit timezone accepted")
	}
}

func TestFileBackendPreservesArtifactAndNeverOverwrites(t *testing.T) {
	a, err := Build(example())
	if err != nil {
		t.Fatal(err)
	}
	target := "file:" + filepath.Join(t.TempDir(), "weather.lobskill")
	var publisher Publisher = FileBackend{}
	var source Source = FileBackend{}
	r, err := publisher.Publish(context.Background(), a, target)
	if err != nil {
		t.Fatal(err)
	}
	b, err := source.Fetch(context.Background(), r.Reference)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("backend changed bytes")
	}
	if _, err := publisher.Publish(context.Background(), a, target); err == nil {
		t.Fatal("overwrote existing file")
	}
	if _, err := source.Fetch(context.Background(), "https://example.com/a"); err == nil {
		t.Fatal("file backend accepted remote URL")
	}
	info, err := os.Stat(target[5:])
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatal("file permissions")
	}
}

func TestScheduleRejectsHostDependentTimezone(t *testing.T) {
	p := example()
	p.Schedules[0].Timezone = "Local"
	if _, err := Build(p); err == nil {
		t.Fatal("host-dependent Local timezone is not portable")
	}
}
