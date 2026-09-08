package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// export answers issue #200's first half: a generated file survives
// only until retention's sweep gets to it, and there was no way for a
// user to say "keep this one". Copying it into export/ — a directory
// the sweep in artifact_sweep.go structurally cannot reach, because
// that sweep only ever walks compute.GeneratedDir — is that mechanism.
//
// The gateway fetch endpoint that lets a REST caller pull the exported
// bytes off the node is issue #200's other half and is NOT part of
// this file; export only has to get the copy onto the same mount a
// channel (Telegram, Slack) can already attach from.

// ExportConfig wires the export builtin.
type ExportConfig struct {
	// Resolver supplies the artifact mount. Export never calls
	// Resolver.Resolve — it copies a file that already exists, it does
	// not create a new artifact — but Mounts and DefaultMount are
	// exactly the mount-lookup seam speak, generate_image and
	// generate_video already depend on, and reusing it means export
	// finds the same root generated/ was written under without a
	// second config shape to keep in sync.
	Resolver *compute.ArtifactResolver
}

// RegisterExportBuiltin installs the export tool. Errors at
// registration rather than at call time: a Resolver-less export could
// never locate a source or a destination, so the honest failure is a
// boot-time one, same as speak and generate_image refusing to register
// without a resolver.
func RegisterExportBuiltin(b *Builtins, cfg ExportConfig) error {
	if cfg.Resolver == nil {
		return errors.New("export: Resolver required; nowhere to copy from or to")
	}
	return b.Register("export", newExportHandler(cfg))
}

// ExportToolDef describes the tool to the model. The example path is
// built from the same constants the handler validates against, so the
// description can never drift out of step with what the tool actually
// accepts.
func ExportToolDef() *types.ToolDef {
	example := filepath.Join(compute.GeneratedDir, "a-solid-green-cube-spinning.mp4")
	return &types.ToolDef{
		Name: "export",
		Path: compute.BuiltinScheme + "export",
		Description: fmt.Sprintf(
			"Copy a file out of %s/ into %s/, where the disk-based retention sweep can never reach it. "+
				"Use when a user wants to keep a generated image, video or speech file beyond retention's window. "+
				"path is the mount-relative location reported when the file was made, e.g. %q. "+
				"Returns the new path and its byte count. Copy, not move — the original stays where it was. "+
				"Exporting the same source again returns the same path rather than duplicating it; exporting a "+
				"DIFFERENT file that happens to share a name gets a numbered suffix rather than overwriting the first.",
			compute.GeneratedDir, compute.ExportDir, example),
		ParametersSchema: []byte(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Mount-relative path to the file to export. Absolute paths and \"..\" segments are refused."}
			},
			"required": ["path"],
			"additionalProperties": false
		}`),
		// A copy that never overwrites, confined to a mount the operator
		// already declared, is undoable by deleting the copy — the
		// definition RiskReversible covers ("writes inside a sandboxed
		// workspace").
		RiskTier: types.RiskReversible,
	}
}

func newExportHandler(cfg ExportConfig) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		path := strings.TrimSpace(args["path"])
		if path == "" {
			return compute.MarshalToolError("missing_arg", "path is required",
				"pass the mount-relative path reported when the file was generated")
		}

		root, ok := cfg.Resolver.Mounts.MountRoot(cfg.Resolver.DefaultMount)
		if !ok {
			return compute.MarshalToolError("mount_unavailable",
				fmt.Sprintf("artifact mount %q is not available", cfg.Resolver.DefaultMount), "")
		}

		src, errPayload, exitCode := confineToMount(root, path)
		if exitCode != 0 {
			return errPayload, exitCode, nil
		}

		// Lstat, never Stat: Stat follows a symlink and would report the
		// TARGET's mode, which is exactly the check a symlink escaping
		// the mount needs to fail.
		info, err := os.Lstat(src)
		switch {
		case os.IsNotExist(err):
			return compute.MarshalToolError("source_not_found",
				fmt.Sprintf("%q does not exist in the mount", path), "")
		case err != nil:
			return compute.MarshalToolError("stat_failed", err.Error(), "")
		case info.Mode()&os.ModeSymlink != 0:
			// Nothing the resolver writes is ever a symlink — every
			// generated file lands via os.WriteFile — so a symlink here
			// means something else put it there, and its target has not
			// been confirmed to sit inside the mount at all. IsRegular()
			// below would refuse it too (a symlink's own mode is never
			// regular), but naming the reason "it's a symlink" rather
			// than the generic "not a file" is worth the extra branch.
			return compute.MarshalToolError("symlink_refused",
				fmt.Sprintf("%q is a symlink; export does not follow it", path), "")
		case info.IsDir():
			return compute.MarshalToolError("not_a_file",
				fmt.Sprintf("%q is a directory", path), "pass a path to a single file")
		case !info.Mode().IsRegular():
			return compute.MarshalToolError("not_a_file",
				fmt.Sprintf("%q is not a regular file", path), "")
		}

		exportRoot := filepath.Join(root, compute.ExportDir)
		if err := os.MkdirAll(exportRoot, 0o755); err != nil {
			return compute.MarshalToolError("export_dir_failed", err.Error(), "")
		}

		dest, deduped, err := exportDestination(exportRoot, filepath.Base(src), src)
		if err != nil {
			return compute.MarshalToolError("collision_check_failed", err.Error(), "")
		}

		var size int64
		if deduped {
			// The bytes are already there under this name — a second copy
			// of identical content would only be waste, and repeating the
			// same export call needs to be safe rather than producing a
			// fresh suffix every time.
			fi, statErr := os.Stat(dest)
			if statErr != nil {
				return compute.MarshalToolError("stat_failed", statErr.Error(), "")
			}
			size = fi.Size()
		} else {
			n, copyErr := copyFile(src, dest)
			if copyErr != nil {
				return compute.MarshalToolError("copy_failed", copyErr.Error(), "")
			}
			size = n
		}

		relPath := filepath.Join(compute.ExportDir, filepath.Base(dest))
		mime := sniffMIME(dest)
		// Announced on every call, including a dedup hit: a channel that
		// missed the file the first time (a REST caller with nothing to
		// attach to at generation time) still gets it now.
		compute.CollectArtifact(ctx, types.Attachment{
			Kind:      compute.AttachmentKindForMIME(mime),
			MimeType:  mime,
			Size:      int(size),
			Reference: cfg.Resolver.DefaultMount + ":" + relPath,
			Filename:  filepath.Base(dest),
		})

		// Exactly the shape #200 specifies: the model gets a path and a
		// size, nothing else — the same "path is the result" reasoning
		// speak and generate_image use for bytes the model cannot read.
		out, _ := json.Marshal(map[string]any{
			"path":  relPath,
			"bytes": size,
		})
		return out, 0, nil
	}
}

// confineToMount turns a model-supplied, mount-relative path into a
// validated absolute path under root.
//
// safeArtifactName (artifact.go) is the wrong tool to reuse here: it
// reduces any input to a bare filename via filepath.Base, which
// destroys the generated/ prefix this function needs to preserve to
// find the source at all. This combines two patterns already proven
// elsewhere instead — the explicit ".." check MountResolver.
// resolveLabelLocked makes (lib_mounts.go) before the prefix check that
// actually confines the result, and the join-then-prefix-check
// artifactOpener uses to confine a read against the same kind of mount
// (wire_generation.go).
func confineToMount(root, path string) (full string, errPayload []byte, exitCode int) {
	// An absolute path is never a legitimate export argument — every
	// generated file is reported mount-relative — so treating one as
	// data to resolve inside the mount would be surprising even though
	// the join below cannot actually be escaped by it.
	if filepath.IsAbs(path) {
		payload, code, _ := compute.MarshalToolError("path_escape",
			fmt.Sprintf("%q is an absolute path", path),
			"pass a path relative to the artifact mount, e.g. \"generated/some-file.mp4\"")
		return "", payload, code
	}
	clean := filepath.Clean(path)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		payload, code, _ := compute.MarshalToolError("path_escape",
			fmt.Sprintf("%q escapes the artifact mount", path), "")
		return "", payload, code
	}
	full = filepath.Join(root, clean)
	// The check that actually confines the result. Clean above already
	// rules out a leading "..", but this is what still stops the copy if
	// a caller reaches this function with something that skipped Clean,
	// or if root resolves through a symlink whose target moved.
	if !strings.HasPrefix(full, filepath.Clean(root)+string(filepath.Separator)) {
		payload, code, _ := compute.MarshalToolError("path_escape",
			fmt.Sprintf("%q escapes the artifact mount", path), "")
		return "", payload, code
	}
	return full, nil, 0
}

// exportDestination decides where a copy of src lands under exportRoot.
//
// The un-suffixed name is tried first. If it is free, that is the
// destination. If it is occupied by a file with IDENTICAL content, that
// slot is reused rather than treated as a collision — otherwise the
// same export call, run twice, would produce a.mp4 then a_1.mp4 then
// a_2.mp4 forever. If it is occupied by something else, the lowest
// numbered suffix that is itself either free or a content match wins.
// No branch of this loop ever overwrites an existing file.
func exportDestination(exportRoot, base, src string) (dest string, deduped bool, err error) {
	for n := 0; ; n++ {
		name := base
		if n > 0 {
			name = suffixedName(base, n)
		}
		candidate := filepath.Join(exportRoot, name)
		info, statErr := os.Lstat(candidate)
		if os.IsNotExist(statErr) {
			return candidate, false, nil
		}
		if statErr != nil {
			return "", false, statErr
		}
		if info.Mode().IsRegular() {
			same, cmpErr := sameContent(src, candidate)
			if cmpErr != nil {
				return "", false, cmpErr
			}
			if same {
				return candidate, true, nil
			}
		}
		// Occupied by different content, or by something that is not a
		// plain file — the next suffix is tried rather than touching it.
	}
}

// suffixedName inserts "_n" before the extension: "a.mp4" at n=1 is
// "a_1.mp4". base is always already flat here — filepath.Base by
// construction — so there is no directory component to preserve.
func suffixedName(base string, n int) string {
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return fmt.Sprintf("%s_%d%s", stem, n, ext)
}

// sameContent reports whether two files hold identical bytes. Sizes
// are compared first as a cheap short-circuit — most collisions are a
// different file that happens to share a name, and comparing lengths
// costs nothing next to hashing a video twice for no reason.
func sameContent(a, b string) (bool, error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer func() { _ = fa.Close() }()
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer func() { _ = fb.Close() }()

	sa, err := fa.Stat()
	if err != nil {
		return false, err
	}
	sb, err := fb.Stat()
	if err != nil {
		return false, err
	}
	if sa.Size() != sb.Size() {
		return false, nil
	}

	ha := sha256.New()
	if _, err := io.Copy(ha, fa); err != nil {
		return false, err
	}
	hb := sha256.New()
	if _, err := io.Copy(hb, fb); err != nil {
		return false, err
	}
	return string(ha.Sum(nil)) == string(hb.Sum(nil)), nil
}

// copyFile copies src to dest, refusing to touch an existing file at
// dest. O_EXCL rather than trusting the caller's prior existence check:
// exportDestination and this call are not atomic with each other, and
// "never overwrite" is the one guarantee export makes about a
// collision — it needs to hold under a race, not only in the common
// case where nothing else touches export/ between the two calls.
func copyFile(src, dest string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		// A partial file left behind after a failed copy would be a
		// silent corruption sitting in export/ under a name that looks
		// finished; better to leave no trace of the attempt at all.
		_ = os.Remove(dest)
		if copyErr != nil {
			return 0, copyErr
		}
		return 0, closeErr
	}
	return n, nil
}

// sniffMIME peeks at the exported file's own bytes to pick a
// Content-Type for the channel-attachment announcement. The source is
// whatever a generation driver (or an inbound upload) produced, and
// neither necessarily carries a trustworthy extension, so reading the
// actual bytes beats guessing from the filename.
func sniffMIME(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	return http.DetectContentType(buf[:n])
}
