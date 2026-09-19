package node

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/config"
)

// The opener is a READ path over model-influenced input: the
// reference originates in a file a tool named after text the model
// chose. The resolver contains names on the way in, but write and
// read are separated by raft, a restart and possibly a different
// node, so the read side checks for itself.
func TestArtifactOpenerContainsReferences(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "generated"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "generated", "ok.mp3"), []byte("AUDIO"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A file that exists OUTSIDE the mount, which a traversing
	// reference would otherwise reach.
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	n := &Node{cfg: Config{Storage: config.StorageConfig{
		Mounts: []config.StorageMountConfig{{Label: "store", Type: "local", Path: root, Mode: "rw"}},
	}}}
	open := n.artifactOpener()

	t.Run("rejects symlink escape", func(t *testing.T) {
		if err := os.Symlink(outside, filepath.Join(root, "generated", "escape.txt")); err != nil {
			t.Fatal(err)
		}
		if rc, err := open("store:generated/escape.txt"); err == nil {
			_ = rc.Close()
			t.Fatal("opened an outside file through a symlink")
		}
	})

	t.Run("reads a contained reference", func(t *testing.T) {
		rc, err := open("store:generated/ok.mp3")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rc.Close() }()
		b, _ := io.ReadAll(rc)
		if string(b) != "AUDIO" {
			t.Errorf("read %q, want AUDIO", b)
		}
	})

	t.Run("refuses to escape", func(t *testing.T) {
		for _, ref := range []string{
			"store:../" + filepath.Base(filepath.Dir(outside)) + "/secret.txt",
			"store:../../etc/passwd",
			"store:/etc/passwd",
		} {
			rc, err := open(ref)
			if err == nil {
				_ = rc.Close()
				t.Errorf("reference %q was opened; it should not escape the mount", ref)
			}
		}
	})

	t.Run("rejects malformed and unknown", func(t *testing.T) {
		for _, ref := range []string{"", "no-colon", "store:", ":path", "nosuchmount:a.mp3"} {
			if rc, err := open(ref); err == nil {
				_ = rc.Close()
				t.Errorf("reference %q was accepted", ref)
			}
		}
	})
}

// A read-only mount is not somewhere generated files live, and it is
// not somewhere the channel should be reading them back from either.
func TestArtifactOpenerHonoursMountMode(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.mp3"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	n := &Node{cfg: Config{Storage: config.StorageConfig{
		Mounts: []config.StorageMountConfig{{Label: "ro", Type: "local", Path: root, Mode: "ro"}},
	}}}
	if rc, err := n.artifactOpener()("ro:a.mp3"); err == nil {
		_ = rc.Close()
		t.Error("opened a file from a read-only mount")
	} else if !strings.Contains(err.Error(), "unwritable") && !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error should explain the mount is not usable, got: %v", err)
	}
}
