package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Skills in the cluster store.
//
// Files were playing three unrelated roles pointed at one directory:
// AUTHORITY (what the cluster believes is installed), INTERCHANGE
// (install, share, back up) and EXECUTION SUBSTRATE (what the
// interpreter and Landlock see). Separating authority from execution
// substrate is the whole change — today they are the same directory,
// which is why a skill exists only where a mount happens to be
// materialised.
//
// Authority moves here. Interchange becomes import/export. The
// execution substrate becomes a derived per-node cache, which the
// self-taught materialiser already built.
//
// THE MANIFEST BYTES ARE STORED VERBATIM. That is the single most
// important decision in this file. A signature is a detached ed25519
// signature over the exact manifest file, so parsing into a proto and
// re-serialising on export changes the bytes and breaks verification
// permanently — a skill that genuinely was signed would be rejected by
// every SigningRequire deployment after one round trip.

// Skill size limits.
//
// Every raft apply replicates to every node and lives in snapshots
// thereafter, so the store is the wrong place for multi-megabyte
// payloads. Manifests, handlers and text references are kilobytes and
// belong here; a sidecar binary does not, and stays in storage
// content-addressed with only its digest on the record.
const (
	DefaultMaxSkillFileBytes  = 1 << 20
	DefaultMaxSkillTotalBytes = 4 << 20
)

var (
	// ErrSkillTooLarge means a file or a bundle exceeded the limit.
	//
	// Refused at IMPORT rather than split or silently accepted, and
	// the error names the offending path — the person running the
	// import is the only one positioned to fix it, and "too large"
	// without saying which file leaves them guessing at a bundle they
	// may not have assembled by hand.
	ErrSkillTooLarge = errors.New("skills: payload is too large to replicate")

	// ErrSkillNotFound is returned for an unknown name or version.
	ErrSkillNotFound = errors.New("skills: not found")

	// ErrNoManifest means the directory is not a skill.
	ErrNoManifest = errors.New("skills: no manifest.yaml")
)

// ManifestFile and SignatureFile are the names on disk. Duplicated
// from internal/skills rather than imported: memory sits below it, and
// a dependency the other way would make the store depend on the
// package that reads it.
const (
	ManifestFile  = "manifest.yaml"
	SignatureFile = "manifest.yaml.sig"
)

// SkillStore holds imported skills and their payloads.
type SkillStore struct {
	raft  raftApplier
	store *Store

	maxFileBytes  int
	maxTotalBytes int
}

// NewSkillStore builds the store.
func NewSkillStore(raft raftApplier, store *Store) (*SkillStore, error) {
	if raft == nil || store == nil {
		return nil, errors.New("skills: raft and store are required")
	}
	return &SkillStore{
		raft:          raft,
		store:         store,
		maxFileBytes:  DefaultMaxSkillFileBytes,
		maxTotalBytes: DefaultMaxSkillTotalBytes,
	}, nil
}

// SetLimits overrides the size limits. Zero or negative keeps the
// default.
func (s *SkillStore) SetLimits(maxFile, maxTotal int) {
	if maxFile > 0 {
		s.maxFileBytes = maxFile
	}
	if maxTotal > 0 {
		s.maxTotalBytes = maxTotal
	}
}

// SkillKey addresses one version. Zero-padding is unnecessary here —
// unlike self-taught history, versions are semver strings rather than
// counters, and a prefix scan by name is what listing needs.
func SkillKey(name, version string) string { return name + "@" + version }

// Bundle is a skill's content, independent of where it came from.
//
// Separated from the directory so an import can arrive over the wire.
// `lobslaw skills import` runs on somebody's laptop and the cluster is
// elsewhere, so the bytes have to travel — a service that took a path
// would be reading a directory that does not exist on the node.
type Bundle struct {
	// Manifest is the manifest file's bytes, VERBATIM.
	Manifest []byte
	// Signature is the detached signature, empty when unsigned.
	Signature []byte
	// Files maps a relative path to its content.
	Files map[string][]byte
}

// ImportRequest is one skill being taken into the store.
type ImportRequest struct {
	// Dir is the skill directory, containing manifest.yaml. Ignored
	// when Bundle is set.
	Dir string
	// Bundle is the content directly, for an import that did not come
	// from a local directory.
	Bundle *Bundle
	// Name and Version come from the parsed manifest. Passed in rather
	// than parsed here so this package does not need to understand the
	// manifest schema — it stores bytes.
	Name    string
	Version string
	Tier    lobslawv1.SkillTier
	// Source records where it came from, for audit.
	Source     string
	ImportedBy string
	// Activate marks this the version in force.
	Activate bool
}

// Import reads a skill directory into the store.
//
// Every file under Dir is stored, not a filtered subset. A skill's
// behaviour can depend on any file it ships — a prompt template, a
// rules document — and an importer that kept only the ones it
// recognised would produce a skill that materialises differently from
// the one somebody tested.
func (s *SkillStore) Import(ctx context.Context, req ImportRequest) (*lobslawv1.SkillRecord, error) {
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Version) == "" {
		return nil, errors.New("skills: import needs a name and a version")
	}
	bundle := req.Bundle
	if bundle == nil {
		var err error
		if bundle, err = ReadBundle(req.Dir); err != nil {
			return nil, err
		}
	}
	manifest, sig, payloads := bundle.Manifest, bundle.Signature, bundle.Files
	if len(manifest) == 0 {
		return nil, fmt.Errorf("%w: the bundle has no manifest", ErrNoManifest)
	}
	if err := s.checkSize(manifest, payloads); err != nil {
		return nil, err
	}

	// Blobs before the record. A record naming a digest that is not
	// stored is a skill that lists a file nobody can read; the reverse
	// — an orphan blob — is wasted bytes that the next import of the
	// same content reuses.
	files := make(map[string]string, len(payloads))
	for path, content := range payloads {
		digest := Digest(content)
		if err := s.putBlob(ctx, digest, content); err != nil {
			return nil, err
		}
		files[path] = digest
	}

	rec := &lobslawv1.SkillRecord{
		Name:         req.Name,
		Version:      req.Version,
		Tier:         req.Tier,
		ManifestYaml: manifest,
		ManifestSig:  sig,
		Files:        files,
		Source:       req.Source,
		ImportedBy:   req.ImportedBy,
		ImportedAt:   timestamppb.Now(),
		Active:       req.Activate,
	}
	if req.Activate {
		// One active version per (name, tier). Two would make the
		// registry's winner depend on iteration order, which is the
		// kind of nondeterminism that shows up as "it works on one
		// node".
		if err := s.deactivateOthers(ctx, req.Name, req.Tier, req.Version); err != nil {
			return nil, err
		}
	}
	if err := s.put(ctx, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// ReadBundle reads a skill directory into a Bundle.
//
// Exported because the CLI does this on the client side: the bytes
// travel, not the path.
func ReadBundle(dir string) (*Bundle, error) {
	dir = filepath.Clean(dir)
	manifest, err := os.ReadFile(filepath.Join(dir, ManifestFile)) //nolint:gosec // operator-supplied import path
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNoManifest, dir)
		}
		return nil, fmt.Errorf("skills: read manifest: %w", err)
	}
	// Read verbatim, never re-encoded. See the file comment.
	sig, err := os.ReadFile(filepath.Join(dir, SignatureFile)) //nolint:gosec // sibling of the manifest
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("skills: read signature: %w", err)
	}
	files, err := collect(dir)
	if err != nil {
		return nil, err
	}
	return &Bundle{Manifest: manifest, Signature: sig, Files: files}, nil
}

// collect reads every file under dir except the manifest and its
// signature, which live on the record itself.
func collect(dir string) (map[string][]byte, error) {
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ManifestFile || rel == SignatureFile {
			return nil
		}
		content, err := os.ReadFile(path) //nolint:gosec // walked from the import dir
		if err != nil {
			return err
		}
		out[rel] = content
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("skills: read %q: %w", dir, err)
	}
	return out, nil
}

// checkSize refuses an oversized bundle, naming the file.
func (s *SkillStore) checkSize(manifest []byte, payloads map[string][]byte) error {
	total := len(manifest)
	// Sorted so the error names the same file every time. An import
	// that failed on a different path each run would be maddening to
	// fix.
	paths := make([]string, 0, len(payloads))
	for p := range payloads {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		n := len(payloads[p])
		if n > s.maxFileBytes {
			return fmt.Errorf("%w: %s is %d bytes, limit is %d — a sidecar binary belongs in "+
				"storage, content-addressed, with only its digest on the record",
				ErrSkillTooLarge, p, n, s.maxFileBytes)
		}
		total += n
	}
	if total > s.maxTotalBytes {
		return fmt.Errorf("%w: the bundle is %d bytes, limit is %d",
			ErrSkillTooLarge, total, s.maxTotalBytes)
	}
	return nil
}

// Digest is the content address of a payload.
func Digest(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// putBlob stores a payload, skipping the write when the content is
// already there.
//
// Content-addressed, so "already there" is a byte-for-byte fact rather
// than a guess. Skipping matters: re-importing a skill whose reference
// documents did not change should not replicate them again to every
// node.
func (s *SkillStore) putBlob(ctx context.Context, digest string, content []byte) error {
	if _, err := s.store.Get(BucketSkillBlobs, digest); err == nil {
		return nil
	}
	return s.apply(ctx, lobslawv1.LogOp_LOG_OP_PUT, digest,
		&lobslawv1.LogEntry_SkillBlob{SkillBlob: &lobslawv1.SkillBlob{
			Digest: digest, Content: content,
		}})
}

// Blob reads a payload by digest.
func (s *SkillStore) Blob(digest string) ([]byte, error) {
	raw, err := s.store.Get(BucketSkillBlobs, digest)
	if err != nil {
		return nil, fmt.Errorf("%w: blob %s", ErrSkillNotFound, digest)
	}
	var b lobslawv1.SkillBlob
	if err := proto.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("skills: decode blob %s: %w", digest, err)
	}
	// Verified on read, not trusted. A blob whose bytes no longer hash
	// to its key is corruption, and returning it would hand a modified
	// handler to the interpreter with the digest still looking right.
	if got := Digest(b.Content); got != digest {
		return nil, fmt.Errorf("skills: blob %s does not match its digest (got %s)", digest, got)
	}
	return b.Content, nil
}

// Get reads one version.
func (s *SkillStore) Get(name, version string) (*lobslawv1.SkillRecord, error) {
	raw, err := s.store.Get(BucketSkills, SkillKey(name, version))
	if err != nil {
		return nil, fmt.Errorf("%w: %s@%s", ErrSkillNotFound, name, version)
	}
	var rec lobslawv1.SkillRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("skills: decode %s@%s: %w", name, version, err)
	}
	return &rec, nil
}

// List returns every stored skill, newest import first.
func (s *SkillStore) List() ([]*lobslawv1.SkillRecord, error) {
	var out []*lobslawv1.SkillRecord
	err := s.store.ForEach(BucketSkills, func(_ string, raw []byte) error {
		var rec lobslawv1.SkillRecord
		if err := proto.Unmarshal(raw, &rec); err != nil {
			return nil //nolint:nilerr // one unreadable record must not hide the rest
		}
		out = append(out, &rec)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("skills: list: %w", err)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].GetImportedAt().AsTime().After(out[j].GetImportedAt().AsTime())
	})
	return out, nil
}

// Active returns the version in force for each name.
func (s *SkillStore) Active() ([]*lobslawv1.SkillRecord, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	out := make([]*lobslawv1.SkillRecord, 0, len(all))
	for _, rec := range all {
		if rec.GetActive() {
			out = append(out, rec)
		}
	}
	return out, nil
}

// deactivateOthers clears the active flag on every other version of
// this skill at this tier.
func (s *SkillStore) deactivateOthers(ctx context.Context, name string, tier lobslawv1.SkillTier, keep string) error {
	all, err := s.List()
	if err != nil {
		return err
	}
	for _, rec := range all {
		if rec.GetName() != name || rec.GetTier() != tier || rec.GetVersion() == keep {
			continue
		}
		if !rec.GetActive() {
			continue
		}
		updated := proto.Clone(rec).(*lobslawv1.SkillRecord)
		updated.Active = false
		if err := s.put(ctx, updated); err != nil {
			return err
		}
	}
	return nil
}

// Activate makes a stored version the one in force.
//
// This is the whole of a rollback: every version imported is still in
// the log, so going back to one is a matter of saying which, not of
// re-importing anything. Nothing is re-verified and no bytes move — the
// record being activated was already parsed through the loader when it
// arrived.
//
// SCOPED TO THE TIER, like every other activation. Activating an
// operator version does not disturb a signed version of the same name;
// which of those wins is a precedence question the loader answers, and
// answering it here as well would give one skill two authorities.
//
// Activating the version already in force succeeds and reports it. An
// operator scripting a rollback should not have to special-case having
// already done it, and an error there would be indistinguishable from
// a rollback that failed.
func (s *SkillStore) Activate(ctx context.Context, name, version string) (rec *lobslawv1.SkillRecord, alreadyActive bool, err error) {
	rec, err = s.Get(name, version)
	if err != nil {
		return nil, false, err
	}
	if rec.GetActive() {
		return rec, true, nil
	}

	updated := proto.Clone(rec).(*lobslawv1.SkillRecord)
	updated.Active = true
	if err := s.put(ctx, updated); err != nil {
		return nil, false, err
	}
	// AFTER the new one is active, not before. The other order leaves a
	// window in which no version of the skill is in force, and a node
	// materialising in that window drops a working skill from disk.
	if err := s.deactivateOthers(ctx, name, updated.GetTier(), version); err != nil {
		return nil, false, err
	}
	return updated, false, nil
}

// Remove deletes one version.
//
// Blobs are left behind. They are content-addressed and shared, so
// removing them here would need a reference count across every record
// — and an orphan blob costs bytes while a missing one costs a working
// skill. Garbage collection is a separate, safer pass.
func (s *SkillStore) Remove(ctx context.Context, name, version string) error {
	if _, err := s.Get(name, version); err != nil {
		return err
	}
	return s.apply(ctx, lobslawv1.LogOp_LOG_OP_DELETE, SkillKey(name, version),
		&lobslawv1.LogEntry_Skill{Skill: &lobslawv1.SkillRecord{Name: name, Version: version}})
}

// Export writes a record back to a directory, byte-identical.
//
// The manifest and its signature go out exactly as they came in. That
// is the whole point of storing them verbatim: `import` then `export`
// must produce a directory whose signature still verifies, or a signed
// corpus cannot survive the move to the store.
func (s *SkillStore) Export(name, version, dir string) error {
	rec, err := s.Get(name, version)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), rec.GetManifestYaml(), 0o600); err != nil {
		return err
	}
	if len(rec.GetManifestSig()) > 0 {
		if err := os.WriteFile(filepath.Join(dir, SignatureFile), rec.GetManifestSig(), 0o600); err != nil {
			return err
		}
	}
	// Sorted, so an export is reproducible and a diff between two runs
	// is empty rather than reordered.
	paths := make([]string, 0, len(rec.GetFiles()))
	for p := range rec.GetFiles() {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, rel := range paths {
		if reason := refuseExportPath(rel); reason != "" {
			return fmt.Errorf("skills: export %s@%s: %s", name, version, reason)
		}
		content, err := s.Blob(rec.GetFiles()[rel])
		if err != nil {
			return err
		}
		dest := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(dest, content, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// refuseExportPath stops a stored record writing outside its
// directory.
//
// Checked on the way OUT as well as in. A record is replicated state
// that a compromised or buggy importer on another node could have
// written, so trusting it here would make the export path a way to
// turn a bad record into arbitrary file writes.
func refuseExportPath(rel string) string {
	cleaned := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(cleaned) || cleaned == ".." ||
		strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Sprintf("file %q is outside the skill directory", rel)
	}
	return ""
}

func (s *SkillStore) put(ctx context.Context, rec *lobslawv1.SkillRecord) error {
	return s.apply(ctx, lobslawv1.LogOp_LOG_OP_PUT, SkillKey(rec.GetName(), rec.GetVersion()),
		&lobslawv1.LogEntry_Skill{Skill: rec})
}

func (s *SkillStore) apply(_ context.Context, op lobslawv1.LogOp, id string, payload any) error {
	entry := &lobslawv1.LogEntry{Op: op, Id: id}
	switch p := payload.(type) {
	case *lobslawv1.LogEntry_Skill:
		entry.Payload = p
	case *lobslawv1.LogEntry_SkillBlob:
		entry.Payload = p
	default:
		return fmt.Errorf("skills: unsupported payload %T", payload)
	}
	data, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("skills: marshal: %w", err)
	}
	res, err := s.raft.Apply(data, skillApplyTimeout)
	if err != nil {
		return fmt.Errorf("skills: apply: %w", err)
	}
	if applyErr, ok := res.(error); ok && applyErr != nil {
		return fmt.Errorf("skills: apply: %w", applyErr)
	}
	return nil
}
