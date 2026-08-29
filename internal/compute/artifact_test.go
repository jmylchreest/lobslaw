package compute

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeMounts resolves one label to a temp dir.
type fakeMounts struct{ label, root string }

func (f fakeMounts) MountRoot(label string) (string, bool) {
	if label == f.label {
		return f.root, true
	}
	return "", false
}

func newResolver(t *testing.T) (*ArtifactResolver, string) {
	t.Helper()
	root := t.TempDir()
	return &ArtifactResolver{
		Mounts:       fakeMounts{label: "store", root: root},
		DefaultMount: "store",
	}, root
}

// The three delivery modes are a vendor accident. Above the resolver
// they must be indistinguishable — one path, in one mount.
func TestResolverNormalisesAllThreeDeliveryModes(t *testing.T) {
	t.Parallel()

	t.Run("inline bytes are written", func(t *testing.T) {
		t.Parallel()
		r, root := newResolver(t)
		got, err := r.Resolve(context.Background(), &Artifact{
			Kind: ArtifactInline, Bytes: []byte("VIDEO"), MIME: "video/mp4",
		}, "clip")
		if err != nil {
			t.Fatal(err)
		}
		if got.Mount != "store" {
			t.Errorf("mount = %q, want store", got.Mount)
		}
		if !strings.HasSuffix(got.Path, ".mp4") {
			t.Errorf("path %q lost its extension; the agent cannot tell what it is", got.Path)
		}
		b, err := os.ReadFile(filepath.Join(root, got.Path))
		if err != nil {
			t.Fatalf("artifact not on disk: %v", err)
		}
		if string(b) != "VIDEO" {
			t.Errorf("content = %q, want VIDEO", b)
		}
	})

	t.Run("a vendor URL is downloaded", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("DOWNLOADED"))
		}))
		t.Cleanup(srv.Close)

		r, root := newResolver(t)
		got, err := r.Resolve(context.Background(), &Artifact{
			Kind: ArtifactURL, URL: srv.URL, ExpiresAt: time.Now().Add(time.Hour),
		}, "clip")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(filepath.Join(root, got.Path))
		if string(b) != "DOWNLOADED" {
			t.Errorf("content = %q, want DOWNLOADED", b)
		}
		if got.Bytes != int64(len("DOWNLOADED")) {
			t.Errorf("Bytes = %d, want %d", got.Bytes, len("DOWNLOADED"))
		}
	})

	t.Run("an operator-owned mount is left alone", func(t *testing.T) {
		t.Parallel()
		r, root := newResolver(t)
		got, err := r.Resolve(context.Background(), &Artifact{
			Kind: ArtifactMount, Mount: "bucket", Path: "vids/a.mp4", MIME: "video/mp4",
		}, "clip")
		if err != nil {
			t.Fatal(err)
		}
		if got.Mount != "bucket" || got.Path != "vids/a.mp4" {
			t.Errorf("got %s/%s; a provider-written artifact must not be moved or re-downloaded",
				got.Mount, got.Path)
		}
		// Nothing should have been written into the default mount.
		if entries, _ := os.ReadDir(root); len(entries) != 0 {
			t.Errorf("mount artifact caused a write into the default mount: %v", entries)
		}
	})
}

// An expired URL is unrecoverable: the job succeeded, was billed, and
// the result is gone. It must say that plainly rather than surfacing
// as a puzzling 403 from a CDN.
func TestResolverReportsExpiryBeforeFetching(t *testing.T) {
	t.Parallel()
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	r, _ := newResolver(t)
	_, err := r.Resolve(context.Background(), &Artifact{
		Kind: ArtifactURL, URL: srv.URL, ExpiresAt: time.Now().Add(-time.Minute),
	}, "clip")
	if !errors.Is(err, ErrArtifactExpired) {
		t.Errorf("got %v, want ErrArtifactExpired", err)
	}
	if hits != 0 {
		t.Errorf("made %d request(s) against a URL already known to be expired", hits)
	}
}

// The name reaches us through a job the MODEL prompted for, so it is
// untrusted. A traversing name must not escape the mount.
func TestResolverContainsHostileNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"../../etc/passwd",
		"/etc/passwd",
		`..\..\windows\system32\cfg`,
		"..",
		"",
	} {
		r, root := newResolver(t)
		got, err := r.Resolve(context.Background(), &Artifact{
			Kind: ArtifactInline, Bytes: []byte("x"), MIME: "video/mp4",
		}, name)
		if err != nil {
			t.Fatalf("name %q: %v", name, err)
		}
		full := filepath.Join(root, got.Path)
		rel, err := filepath.Rel(root, full)
		if err != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("name %q escaped the mount: %s", name, full)
		}
		if _, err := os.Stat(full); err != nil {
			t.Errorf("name %q: nothing written: %v", name, err)
		}
	}
}

// A generated video is large and a broken provider is unbounded.
func TestResolverCapsDownloadSize(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 4096))
	}))
	t.Cleanup(srv.Close)

	r, _ := newResolver(t)
	r.MaxBytes = 1024
	_, err := r.Resolve(context.Background(), &Artifact{Kind: ArtifactURL, URL: srv.URL}, "big")
	if err == nil {
		t.Fatal("an oversized artifact was accepted")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("error should name the cap, got: %v", err)
	}
	// Truncating to the cap would be worse than failing: a half-written
	// video looks like a successful generation.
	if ClassifyFailure(err) != FailurePermanent {
		t.Errorf("an oversized artifact is not worth retrying elsewhere; got %s", ClassifyFailure(err))
	}
}

// A misconfigured resolver must fail loudly at the point of use rather
// than dropping the artifact of a job that has already been paid for.
func TestResolverRefusesWithNowhereToWrite(t *testing.T) {
	t.Parallel()
	r := &ArtifactResolver{Mounts: fakeMounts{label: "store", root: t.TempDir()}}
	if _, err := r.Resolve(context.Background(),
		&Artifact{Kind: ArtifactInline, Bytes: []byte("x")}, "a"); err == nil {
		t.Error("resolved an inline artifact with no default mount configured")
	}

	r2 := &ArtifactResolver{Mounts: fakeMounts{label: "other", root: t.TempDir()}, DefaultMount: "store"}
	if _, err := r2.Resolve(context.Background(),
		&Artifact{Kind: ArtifactInline, Bytes: []byte("x")}, "a"); err == nil {
		t.Error("resolved into a mount that does not exist")
	}
}

// A prompt-derived name ends in a full stop whenever the prompt did,
// and filepath.Ext("x.") is "." rather than "" — so the type suffix was
// skipped and a delivered MP4 landed with no extension at all. Nothing
// downstream could identify it by name.
func TestSafeArtifactNameAppendsExtAfterTrailingDot(t *testing.T) {
	for _, tc := range []struct {
		name, in, mime, want string
	}{
		{"trailing dot", "a green triangle spinning.", "video/mp4", "generated/a green triangle spinning.mp4"},
		{"several dots", "clip...", "video/mp4", "generated/clip.mp4"},
		{"real extension kept", "clip.webm", "video/mp4", "generated/clip.webm"},
		{"no dot", "clip", "video/mp4", "generated/clip.mp4"},
		{"only dots", "...", "video/mp4", "generated/artifact.mp4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := safeArtifactName(tc.in, tc.mime); got != tc.want {
				t.Errorf("safeArtifactName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The name is derived from model-supplied text, so it has to be
// bounded. A video's name was the entire prompt: 242 characters of
// spaces and punctuation, one longer prompt from failing to write.
func TestArtifactFileNameIsBounded(t *testing.T) {
	long := "A flat bright green triangle spinning slowly and smoothly around its center " +
		"on a clean neutral light-gray background. Simple minimalist 3D render."
	got := ArtifactFileName(long, "video")
	if got != "a-flat-bright-green-triangle" {
		t.Errorf("ArtifactFileName = %q", got)
	}
	if ArtifactFileName("!!! ???", "video") != "video" {
		t.Error("unsluggable input should fall back")
	}
}
