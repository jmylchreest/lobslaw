package workforce

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

const (
	MaxTaskAttachments        int   = 16
	MaxArtifactReferenceBytes int   = 1024
	MaxArtifactNameBytes      int   = 128
	MaxArtifactMimeBytes      int   = 128
	MaxArtifactDownloadBytes  int64 = 64 << 20
	ArtifactDownloadMime            = "application/octet-stream"
)

var artifactMountName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

type ArtifactDownload struct {
	Artifact Artifact
	Reader   io.ReadCloser
}

func artifactRoute(task, id string) string { return "/v1/tasks/" + task + "/artifacts/" + id }
func publicArtifacts(t *Task) []Artifact {
	out := slices.Clone(t.Artifacts)
	for i := range out {
		out[i].Reference = artifactRoute(t.ID, out[i].ID)
	}
	return out
}
func validArtifactReference(reference string) bool {
	mount, rel, ok := strings.Cut(reference, ":")
	return ok && len(reference) <= MaxArtifactReferenceBytes && artifactMountName.MatchString(mount) && rel != "." && fs.ValidPath(rel) && !strings.ContainsAny(rel, "\\\x00:?#%") && strings.IndexFunc(rel, unicode.IsControl) < 0
}
func artifactName(name string) string {
	if name == "" || len(name) > MaxArtifactNameBytes || name == "." || name == ".." || strings.ContainsAny(name, "/\\:?#") || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "artifact"
	}
	return name
}
func artifactMime(value string) string {
	if len(value) > MaxArtifactMimeBytes {
		return ArtifactDownloadMime
	}
	media, _, e := mime.ParseMediaType(value)
	if e != nil || !strings.Contains(media, "/") {
		return ArtifactDownloadMime
	}
	return media
}
func attachmentKind(kind types.AttachmentKind) string {
	switch kind {
	case types.AttachmentImage, types.AttachmentVoice, types.AttachmentAudio, types.AttachmentVideo, types.AttachmentDocument, types.AttachmentSticker:
		return string(kind)
	default:
		return string(types.AttachmentDocument)
	}
}

// Private mount references are stored separately from the public task view.
// Validation is all-or-nothing and repeated at download time; LocalPath is never
// persisted or used as an alternative to the configured mount opener.
func recordAttachments(t *Task, x *Execution, attachments []types.Attachment) error {
	if len(attachments) > MaxTaskAttachments {
		return fmt.Errorf("%w: too many task attachments", ErrInvalid)
	}
	references := make(map[string]string, len(x.ArtifactReferences)+len(attachments))
	existing := map[string]bool{}
	for id, ref := range x.ArtifactReferences {
		references[id] = ref
		existing[ref] = true
	}
	out := publicArtifacts(t)
	for _, a := range attachments {
		if !validArtifactReference(a.Reference) || a.Size < 0 || int64(a.Size) > MaxArtifactDownloadBytes {
			return fmt.Errorf("%w: attachment is not a bounded mount artifact", ErrInvalid)
		}
		if existing[a.Reference] {
			continue
		}
		if len(references) >= MaxTaskAttachments {
			return fmt.Errorf("%w: too many task attachments", ErrInvalid)
		}
		id := ids.New()
		out = append(out, Artifact{ID: id, Name: artifactName(a.Filename), Kind: attachmentKind(a.Kind), MimeType: artifactMime(a.MimeType), Size: int64(a.Size), Reference: artifactRoute(t.ID, id), CreatedAt: time.Now().UTC()})
		references[id] = a.Reference
		existing[a.Reference] = true
	}
	t.Artifacts = out
	x.ArtifactReferences = references
	return nil
}
func recordResult(t *Task) {
	id := ids.New()
	t.Artifacts = append(t.Artifacts, Artifact{ID: id, Name: "Result", Kind: "text", MimeType: "text/plain", Size: int64(len(t.Result)), Reference: artifactRoute(t.ID, id), CreatedAt: time.Now().UTC()})
}

func (s *Service) OpenTaskArtifact(ctx context.Context, owner, taskID, artifactID string) (*ArtifactDownload, error) {
	st, e := s.find(ctx, owner, taskID, "task")
	if e != nil {
		return nil, e
	}
	t := st.Tasks[taskID]
	var artifact *Artifact
	for _, a := range publicArtifacts(t) {
		if a.ID == artifactID {
			artifact = &a
			break
		}
	}
	if artifact == nil {
		return nil, ErrNotFound
	}
	var reference string
	if x := st.Executions[taskID]; x != nil {
		reference = x.ArtifactReferences[artifactID]
	}
	if reference == "" {
		if artifact.Kind != "text" || artifact.Name != "Result" {
			return nil, ErrNotFound
		}
		artifact.Size = int64(len(t.Result))
		return &ArtifactDownload{Artifact: *artifact, Reader: io.NopCloser(strings.NewReader(t.Result))}, nil
	}
	if !validArtifactReference(reference) {
		return nil, ErrNotFound
	}
	if s.cfg.OpenArtifact == nil {
		return nil, ErrUnavailable
	}
	reader, e := s.cfg.OpenArtifact(reference)
	if e != nil {
		return nil, ErrNotFound
	}
	if reader == nil {
		return nil, ErrUnavailable
	}
	stat, ok := reader.(interface{ Stat() (fs.FileInfo, error) })
	if !ok {
		_ = reader.Close()
		return nil, ErrUnavailable
	}
	info, e := stat.Stat()
	if e != nil || info == nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > MaxArtifactDownloadBytes {
		_ = reader.Close()
		return nil, ErrNotFound
	}
	artifact.Name = artifactName(artifact.Name)
	artifact.MimeType = artifactMime(artifact.MimeType)
	artifact.Size = info.Size()
	return &ArtifactDownload{Artifact: *artifact, Reader: reader}, nil
}
