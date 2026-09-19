package workforce

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestTaskAttachmentPersistsAndDownloadsOnlyForOwner(t *testing.T) {
	t.Parallel()
	s, repo, _, p := fixture(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "report.pdf")
	content := "%PDF-1.7 actual generated report"
	if e := os.WriteFile(path, []byte(content), 0o600); e != nil {
		t.Fatal(e)
	}
	opens := 0
	s.cfg.OpenArtifact = func(ref string) (io.ReadCloser, error) {
		opens++
		if ref != "reports:generated/report.pdf" {
			t.Fatal("unsafe opener reference", ref)
		}
		return os.Open(path)
	}
	s.cfg.Runner = transcriptRunner{response: &turn.Response{Reply: "Report ready", Attachments: []types.Attachment{{Kind: types.AttachmentDocument, Reference: "reports:generated/report.pdf", LocalPath: "/private/LOCAL-PATH-SECRET", Filename: "report.pdf", MimeType: "application/pdf", Size: len(content)}}}}
	s.cfg.SaveTranscript = func(context.Context, *Task, []turn.Message) error { return nil }
	task, e := s.Chat(ctx, p.Owner, p.ID, "generate report", "", nil)
	if e != nil {
		t.Fatal(e)
	}
	s.WorkOnce(ctx)
	task, e = s.GetTask(ctx, p.Owner, task.ID)
	if e != nil || task.Status != StatusDone || len(task.Artifacts) != 2 {
		t.Fatal(task, e)
	}
	public, _ := json.Marshal(task)
	if strings.Contains(string(public), "reports:") || strings.Contains(string(public), "LOCAL-PATH-SECRET") {
		t.Fatal("private locator leaked", string(public))
	}
	st, _ := repo.Get(ctx, p.ID)
	raw, _ := json.Marshal(st)
	if strings.Contains(string(raw), "LOCAL-PATH-SECRET") || strings.Contains(string(raw), content) {
		t.Fatal("path or binary persisted")
	}
	s = New(s.cfg)
	s.cfg.Runner = &testRunner{}
	for range MaxRetainedChats + 2 {
		if _, e = s.Chat(ctx, p.Owner, p.ID, "hello", "", nil); e != nil {
			t.Fatal(e)
		}
		s.WorkOnce(ctx)
	}
	var file Artifact
	for _, a := range task.Artifacts {
		if !strings.HasPrefix(a.Reference, "/v1/tasks/"+task.ID+"/artifacts/") {
			t.Fatal(a.Reference)
		}
		if a.Kind == "document" {
			file = a
		}
	}
	if _, e = s.OpenTaskArtifact(ctx, "user:bob", task.ID, file.ID); !errors.Is(e, ErrForbidden) {
		t.Fatal(e)
	}
	if opens != 0 {
		t.Fatal("opened before authorization")
	}
	download, e := s.OpenTaskArtifact(ctx, p.Owner, task.ID, file.ID)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = download.Reader.Close() }()
	got, e := io.ReadAll(download.Reader)
	if e != nil || string(got) != content || download.Artifact.Size != int64(len(content)) {
		t.Fatal(string(got), e, download.Artifact)
	}
	st, _ = repo.Get(ctx, p.ID)
	st.Executions[task.ID].ArtifactReferences[file.ID] = "https://example.test/private?token=PRIVATE-TOKEN"
	if e = repo.Put(ctx, st, st.Revision); e != nil {
		t.Fatal(e)
	}
	before := opens
	if _, e = s.OpenTaskArtifact(ctx, p.Owner, task.ID, file.ID); !errors.Is(e, ErrNotFound) || opens != before {
		t.Fatal("stored unsafe reference reached opener", e)
	}
}

func TestAttachmentBoundsAndUntrustedMetadata(t *testing.T) {
	t.Parallel()
	task := &Task{ID: "task"}
	execution := &Execution{}
	attachment := types.Attachment{Reference: "reports:generated/report.pdf", Filename: "/private/NAME-SECRET", MimeType: "application/pdf; token=PRIVATE-TOKEN"}
	if e := recordAttachments(task, execution, []types.Attachment{attachment}); e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(task)
	if strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "PRIVATE-TOKEN") || task.Artifacts[0].MimeType != "application/pdf" {
		t.Fatal(string(raw))
	}
	id := task.Artifacts[0].ID
	if e := recordAttachments(task, execution, []types.Attachment{attachment}); e != nil || len(task.Artifacts) != 1 || task.Artifacts[0].ID != id {
		t.Fatal("duplicate attachment", task, e)
	}
	many := make([]types.Attachment, MaxTaskAttachments+1)
	if e := recordAttachments(task, execution, many); !errors.Is(e, ErrInvalid) {
		t.Fatal("unbounded metadata", e)
	}
	s, repo, _, p := fixture(t)
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "oversize")
	f, e := os.Create(file)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Truncate(MaxArtifactDownloadBytes + 1); e != nil {
		t.Fatal(e)
	}
	_ = f.Close()
	s.cfg.OpenArtifact = func(string) (io.ReadCloser, error) { return os.Open(file) }
	s.cfg.Runner = transcriptRunner{response: &turn.Response{Reply: "report", Attachments: []types.Attachment{{Reference: attachment.Reference, Filename: "report.pdf"}}}}
	created, e := s.CreateTask(ctx, p.Owner, p.ID, Task{Title: "oversize", Instructions: "work"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	s.WorkOnce(ctx)
	st, _ := repo.Get(ctx, p.ID)
	for artifactID := range st.Executions[created.ID].ArtifactReferences {
		if _, e = s.OpenTaskArtifact(ctx, p.Owner, created.ID, artifactID); !errors.Is(e, ErrNotFound) {
			t.Fatal("oversized stream admitted", e)
		}
	}
}

func TestUnsafeAttachmentReferencesAreNeverOpenedOrExposed(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"https://example.test/report?token=PRIVATE-TOKEN", "/private/LOCAL-PATH-SECRET", "reports:../secret", "reports:/etc/passwd", "reports:generated/../secret", "reports:generated/report?token=PRIVATE-TOKEN"} {
		t.Run(ref, func(t *testing.T) {
			s, repo, _, p := fixture(t)
			s.cfg.OpenArtifact = func(string) (io.ReadCloser, error) { t.Fatal("unsafe reference opened"); return nil, ErrInvalid }
			s.cfg.Runner = transcriptRunner{response: &turn.Response{Reply: "report", Attachments: []types.Attachment{{Reference: ref, LocalPath: "/private/LOCAL-PATH-SECRET", Filename: "report.pdf"}}}}
			task, e := s.CreateTask(context.Background(), p.Owner, p.ID, Task{Title: "unsafe", Instructions: "work"}, nil)
			if e != nil {
				t.Fatal(e)
			}
			s.WorkOnce(context.Background())
			task, _ = s.GetTask(context.Background(), p.Owner, task.ID)
			if task.Status != StatusFailed {
				t.Fatal(task)
			}
			st, _ := repo.Get(context.Background(), p.ID)
			raw, _ := json.Marshal(st)
			if strings.Contains(string(raw), "PRIVATE-TOKEN") || strings.Contains(string(raw), "LOCAL-PATH-SECRET") {
				t.Fatal("unsafe metadata persisted")
			}
		})
	}
}

func TestAttentionAcknowledgementIsOwnedDurableAndResetOnRetry(t *testing.T) {
	t.Parallel()
	s, repo, _, p := fixture(t)
	ctx := context.Background()
	task, e := s.CreateTask(ctx, p.Owner, p.ID, Task{Title: "review", Instructions: "work"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.ActTask(ctx, p.Owner, task.ID, task.Revision, "acknowledge", "", nil); !errors.Is(e, ErrInvalid) {
		t.Fatal("active task acknowledged", e)
	}
	s.cfg.Runner = nil
	s.WorkOnce(ctx)
	task, _ = s.GetTask(ctx, p.Owner, task.ID)
	if _, e = s.ActTask(ctx, "user:bob", task.ID, task.Revision, "acknowledge", "", nil); !errors.Is(e, ErrForbidden) {
		t.Fatal(e)
	}
	task, e = s.ActTask(ctx, p.Owner, task.ID, task.Revision, "acknowledge", "", nil)
	if e != nil || !task.Acknowledged {
		t.Fatal(task, e)
	}
	s = New(Config{Repository: repo, Bots: testBots{}})
	items, e := s.Attention(ctx, p.Owner)
	if e != nil || len(items) != 0 {
		t.Fatal(items, e)
	}
	task, e = s.ActTask(ctx, p.Owner, task.ID, task.Revision, "retry", "", nil)
	if e != nil || task.Acknowledged {
		t.Fatal(task, e)
	}
	s.cfg.Runner = &testRunner{}
	s.WorkOnce(ctx)
	task, _ = s.GetTask(ctx, p.Owner, task.ID)
	if task.Status != StatusDone {
		t.Fatal(task)
	}
	task, e = s.ActTask(ctx, p.Owner, task.ID, task.Revision, "acknowledge", "", nil)
	if e != nil || !task.Acknowledged {
		t.Fatal(task, e)
	}
	items, e = s.Attention(ctx, p.Owner)
	if e != nil || len(items) != 0 {
		t.Fatal(items, e)
	}
}
