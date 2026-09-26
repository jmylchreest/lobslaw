package clawhub

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/sharing"
)

func TestShareSourceFetchesSlugWithoutInstalling(t *testing.T) {
	prose := []byte("---\nname: gog\nmetadata: {clawdbot: {requires: {bins: [not-installed]}, install: [{kind: brew, formula: should-not-run}]}}\n---\nUse gog.\n")
	bundle := makeZipBundle(t, map[string][]byte{"SKILL.md": prose, "_meta.json": []byte(`{"slug":"gog","version":"1.2.3"}`)})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/download" || r.URL.Query().Get("slug") != "gog" {
			t.Errorf("wrong request %s", r.URL)
		}
		_, _ = w.Write(bundle)
	}))
	defer server.Close()
	source, err := NewShareSource(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	var fetcher sharing.Source = source
	a, err := fetcher.Fetch(context.Background(), "clawhub:steipete/gog")
	if err != nil {
		t.Fatal(err)
	}
	p := a.Package()
	if p.Name != "gog" || p.Version != "1.2.3" || !bytes.Equal(p.Files["SKILL.md"], prose) {
		t.Fatal("conversion lost identity or original instructions")
	}
	if a.Publisher() != "" || p.Origin == nil || p.Origin.Digest != sharing.Hash(bundle) {
		t.Fatal("source provenance missing or publisher trust fabricated")
	}
	if !strings.Contains(string(p.Manifest), "requires_binary:") || !strings.Contains(string(p.Manifest), "body: SKILL.md") {
		t.Fatal("missing runtime requirements or instructions")
	}
	again, err := fetcher.Fetch(context.Background(), "clawhub:steipete/gog")
	if err != nil || again.Digest() != a.Digest() {
		t.Fatal("fetch isn't deterministic", err)
	}
}

func TestShareSourceChecksCatalogIdentityAndDigest(t *testing.T) {
	bundle := makeBundle(t, map[string]string{"manifest.yaml": "name: demo\nversion: 1.0.0\nruntime: prose\nbody: SKILL.md\n", "SKILL.md": "Instructions"})
	for _, tc := range []struct {
		name, sha string
		fail      bool
	}{{"demo", sha256Hex(bundle), false}, {"different", sha256Hex(bundle), true}, {"demo", strings.Repeat("0", 64), true}} {
		t.Run(tc.name+tc.sha[:6], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/bundle" {
					_, _ = w.Write(bundle)
					return
				}
				_ = json.NewEncoder(w).Encode(SkillEntry{Name: tc.name, Version: "1.0.0", BundleURL: "http://" + r.Host + "/bundle", BundleSHA256: tc.sha})
			}))
			defer server.Close()
			source, err := NewShareSource(server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = source.Fetch(context.Background(), "clawhub:demo@1.0.0")
			if (err != nil) != tc.fail {
				t.Fatalf("unexpected result %v", err)
			}
		})
	}
}

func TestShareSourceRejectsExpandedSizeAndTraversal(t *testing.T) {
	for _, files := range []map[string][]byte{
		{"SKILL.md": bytes.Repeat([]byte("x"), sharing.MaxBytes+1)},
		{"../escape": []byte("bad")},
	} {
		bundle := makeZipBundle(t, files)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(bundle) }))
		source, err := NewShareSource(server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := source.Fetch(context.Background(), "clawhub:demo"); err == nil {
			t.Fatal("unsafe bundle accepted")
		}
		server.Close()
	}
}

func TestShareSourceRejectsUntrustedCatalogSignature(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(SkillEntry{Name: "demo", Version: "1.0.0", BundleURL: "http://" + r.Host + "/bundle", BundleSHA256: strings.Repeat("0", 64), SignedBy: "unknown", Signature: "invalid"})
	}))
	defer server.Close()
	source, err := NewShareSource(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Fetch(context.Background(), "clawhub:demo@1.0.0"); err == nil {
		t.Fatal("present catalogue signature ignored without trust keys")
	}
}

func TestShareSourceVerifiesSignedCatalogBeforeConversion(t *testing.T) {
	bundle := makeBundle(t, map[string]string{"manifest.yaml": "name: demo\nversion: 1.0.0\nruntime: prose\nbody: SKILL.md\n", "SKILL.md": "Instructions"})
	verifier, key := newFakeVerifier(t, "publisher")
	for _, tampered := range []bool{false, true} {
		t.Run(fmt.Sprint(tampered), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/bundle" {
					_, _ = w.Write(bundle)
					return
				}
				entry := &SkillEntry{Name: "demo", Version: "1.0.0", BundleURL: "http://" + r.Host + "/bundle", BundleSHA256: sha256Hex(bundle), SignedBy: "publisher"}
				signEntry(t, key, entry)
				if tampered {
					// A well-formed signature over different bytes, not a parse error.
					entry.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, []byte("different catalogue entry")))
				}
				_ = json.NewEncoder(w).Encode(entry)
			}))
			defer server.Close()
			source, err := NewShareSource(server.URL, verifier)
			if err != nil {
				t.Fatal(err)
			}
			_, err = source.Fetch(t.Context(), "clawhub:demo@1.0.0")
			if tampered {
				if err == nil || !strings.Contains(err.Error(), "signature") {
					t.Fatalf("tampered catalogue signature accepted: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
