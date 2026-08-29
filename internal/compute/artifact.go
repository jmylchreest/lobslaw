package compute

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Artifacts arrive three different ways, and the difference is a
// vendor accident rather than something the agent should reason about:
//
//	Vendor URL, expiring   Wan returns video_url, good for 24h
//	Inline bytes           Veo without storageUri, base64 in the response
//	Operator-owned bucket  Bedrock REQUIRES s3OutputDataConfig; Veo accepts storageUri
//
// The third is a good fit rather than a nuisance: lobslaw already has
// a storage layer with operator-declared mounts, so a provider writing
// into one is the artifact landing exactly where the agent can already
// read it — no download, no expiry.
//
// Above the resolver every modality returns the same thing: a path
// inside a mount.

// ArtifactKind discriminates the three delivery modes.
type ArtifactKind string

const (
	// ArtifactURL is a vendor-hosted URL that EXPIRES. The clock is
	// the hazard: a job whose delivery is delayed past ExpiresAt is
	// lost, and no amount of retrying brings it back.
	ArtifactURL ArtifactKind = "url"

	// ArtifactInline is bytes in the response.
	ArtifactInline ArtifactKind = "inline"

	// ArtifactMount is already in operator storage; the provider wrote
	// it there. Nothing to fetch.
	ArtifactMount ArtifactKind = "mount"
)

// Artifact is a generated thing in whichever form its vendor produced.
type Artifact struct {
	Kind ArtifactKind

	URL       string    // ArtifactURL
	ExpiresAt time.Time // ArtifactURL; zero means unknown, not "never"

	Bytes []byte // ArtifactInline

	Mount string // ArtifactMount — mount label
	Path  string // ArtifactMount — path within the mount

	MIME string
}

// ResolvedArtifact is what every modality returns once the vendor
// difference has been normalised away.
type ResolvedArtifact struct {
	Mount string
	Path  string
	MIME  string
	Bytes int64
}

// MountWriter is the seam onto the storage layer. Mounts are
// filesystem-backed (local, NFS, and the object stores via rclone), so
// resolving a label to a root and writing under it is the whole
// contract — deliberately small, so this package does not grow a
// dependency on the storage service.
type MountWriter interface {
	// MountRoot resolves a mount label to a filesystem root, reporting
	// false when the label is unknown or not writable.
	MountRoot(label string) (string, bool)
}

// ErrArtifactExpired is returned when a URL artifact's expiry has
// already passed. Distinguished because it is unrecoverable and worth
// saying plainly: the job succeeded and the result was lost anyway.
var ErrArtifactExpired = errors.New("artifact: vendor URL expired before it was fetched")

// ArtifactResolver normalises the three delivery modes into a path
// inside a mount.
type ArtifactResolver struct {
	Mounts MountWriter
	HTTP   *http.Client

	// DefaultMount receives URL and inline artifacts. Required for
	// those two kinds; an ArtifactMount is already somewhere.
	DefaultMount string

	// MaxBytes caps a download. A generated video is large and a
	// hostile or broken provider is unbounded.
	MaxBytes int64
}

// DefaultMaxArtifactBytes bounds a single artifact. Generous enough
// for a few minutes of video, small enough that a runaway response
// cannot fill an operator's disk.
const DefaultMaxArtifactBytes = 512 << 20 // 512 MiB

// Resolve turns an artifact into a path the agent can read.
//
// The expiry check happens BEFORE the request rather than after a
// failure, so an expired URL reports what actually went wrong instead
// of surfacing as a confusing 403 from the vendor's CDN.
func (r *ArtifactResolver) Resolve(ctx context.Context, a *Artifact, name string) (*ResolvedArtifact, error) {
	if a == nil {
		return nil, errors.New("artifact: nil artifact")
	}
	switch a.Kind {
	case ArtifactMount:
		// The provider wrote into operator storage. Nothing to do —
		// and nothing to charge in bandwidth or disk either.
		if a.Mount == "" || a.Path == "" {
			return nil, fmt.Errorf("artifact: mount artifact missing mount (%q) or path (%q)", a.Mount, a.Path)
		}
		return &ResolvedArtifact{Mount: a.Mount, Path: a.Path, MIME: a.MIME}, nil

	case ArtifactInline:
		if len(a.Bytes) == 0 {
			return nil, errors.New("artifact: inline artifact carried no bytes")
		}
		return r.write(name, a.MIME, a.Bytes)

	case ArtifactURL:
		if a.URL == "" {
			return nil, errors.New("artifact: url artifact carried no URL")
		}
		if !a.ExpiresAt.IsZero() && time.Now().After(a.ExpiresAt) {
			return nil, fmt.Errorf("%w (expired %s ago)", ErrArtifactExpired,
				time.Since(a.ExpiresAt).Round(time.Second))
		}
		b, mime, err := r.download(ctx, a.URL)
		if err != nil {
			return nil, err
		}
		if a.MIME != "" {
			mime = a.MIME
		}
		return r.write(name, mime, b)

	default:
		return nil, fmt.Errorf("artifact: unknown kind %q", a.Kind)
	}
}

func (r *ArtifactResolver) download(ctx context.Context, url string) ([]byte, string, error) {
	client := r.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", Permanent(fmt.Errorf("artifact: build request: %w", err))
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", Transient(fmt.Errorf("artifact: fetch: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, "", &DriverError{
			Class: ClassifyHTTPStatus(resp.StatusCode, ""),
			Err:   fmt.Errorf("artifact: fetch: HTTP %d", resp.StatusCode),
		}
	}

	max := r.MaxBytes
	if max <= 0 {
		max = DefaultMaxArtifactBytes
	}
	// Read one byte past the cap so an oversized artifact is detected
	// rather than silently truncated into a corrupt file.
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, "", Transient(fmt.Errorf("artifact: read: %w", err))
	}
	if int64(len(b)) > max {
		return nil, "", Permanent(fmt.Errorf("artifact: exceeds %d byte cap", max))
	}
	return b, resp.Header.Get("Content-Type"), nil
}

// write lands bytes in the default mount.
func (r *ArtifactResolver) write(name, mime string, b []byte) (*ResolvedArtifact, error) {
	if r.Mounts == nil {
		return nil, errors.New("artifact: no mount writer configured")
	}
	if r.DefaultMount == "" {
		return nil, errors.New("artifact: no default mount configured; nowhere to put a generated file")
	}
	root, ok := r.Mounts.MountRoot(r.DefaultMount)
	if !ok {
		return nil, fmt.Errorf("artifact: mount %q is not available for writing", r.DefaultMount)
	}

	rel := safeArtifactName(name, mime)
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return nil, fmt.Errorf("artifact: create dir: %w", err)
	}
	if err := os.WriteFile(full, b, 0o600); err != nil {
		return nil, fmt.Errorf("artifact: write: %w", err)
	}
	return &ResolvedArtifact{
		Mount: r.DefaultMount, Path: rel, MIME: mime, Bytes: int64(len(b)),
	}, nil
}

// safeArtifactName keeps a provider-influenced name from escaping the
// mount. The name reaches us via a job the model prompted for, so it
// is not trusted input: path separators and traversal are stripped
// rather than sanitised in place.
func safeArtifactName(name, mime string) string {
	base := filepath.Base(filepath.Clean("/" + strings.ReplaceAll(name, `\`, "/")))
	base = strings.TrimSpace(base)
	if base == "" || base == "." || base == "/" || base == ".." {
		base = "artifact"
	}
	// Trailing dots are trimmed before asking whether there is an
	// extension. A name ending in "." — which a prompt-derived name
	// does whenever the prompt ended in a full stop — has an Ext() of
	// "." rather than "", so the type suffix was skipped and the file
	// landed with nothing downstream could identify it by.
	base = strings.TrimRight(base, ".")
	if base == "" {
		base = "artifact"
	}
	if !hasFileExt(base) {
		if ext := extForMIME(mime); ext != "" {
			base += ext
		}
	}
	return filepath.Join("generated", base)
}

// hasFileExt reports whether base already ends in something that is
// plausibly a file extension.
//
// filepath.Ext alone is not enough for a name derived from a prompt.
// It returns everything after the LAST dot, so "a triangle on a grey
// background. Simple 3D render" has an "extension" 30 characters long
// and the real type suffix is never appended — the file lands as
// something nothing downstream can identify by name.
func hasFileExt(base string) bool {
	ext := filepath.Ext(base)
	if len(ext) < 2 || len(ext) > 6 {
		return false
	}
	for _, r := range ext[1:] {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// ArtifactFileName turns a prompt into a short slug for a generated
// file. Bounded at five words because the name is derived from
// model-supplied text of arbitrary length, and an unbounded one is a
// filesystem limit waiting to be hit.
func ArtifactFileName(s, fallback string) string {
	words := strings.Fields(s)
	if len(words) > 5 {
		words = words[:5]
	}
	var b strings.Builder
	for _, w := range words {
		for _, r := range w {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				b.WriteRune(r)
			case r >= 'A' && r <= 'Z':
				b.WriteRune(r + 32)
			}
		}
		b.WriteByte('-')
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		name = fallback
	}
	return name
}

// extForMIME covers the generation types. Deliberately a small table
// rather than mime.ExtensionsByType, which is platform-dependent and
// returns a nondeterministic list.
func extForMIME(mime string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0])) {
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "audio/mpeg":
		return ".mp3"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "audio/ogg":
		return ".ogg"
	default:
		return ""
	}
}
