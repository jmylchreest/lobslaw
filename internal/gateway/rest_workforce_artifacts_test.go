package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/internal/workforce"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type artifactResponseRunner struct{}

func (artifactResponseRunner) Run(context.Context, turn.Request) (*turn.Response, error) {
	return &turn.Response{Reply: "Report ready", Attachments: []types.Attachment{{Kind: types.AttachmentDocument, Reference: "reports:generated/report.pdf", Filename: "report.pdf", MimeType: "application/pdf", LocalPath: "/private/NEVER-EXPOSE"}}}, nil
}
func (r artifactResponseRunner) Resume(ctx context.Context, req turn.Request, _ []turn.Message) (*turn.Response, error) {
	return r.Run(ctx, req)
}

func TestWorkforceArtifactLocalAndRemoteDownload(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "report.pdf")
	content := "%PDF-1.7 generated report bytes\x00\xff"
	if e := os.WriteFile(path, []byte(content), 0o600); e != nil {
		t.Fatal(e)
	}
	var opens atomic.Int32
	var broken atomic.Bool
	svc := workforce.New(workforce.Config{Repository: &workforceRepo{rows: map[string][]byte{}}, Bots: &memBots{recs: map[string]*lobslawv1.BotRecord{"worker": {Id: "worker", Owner: "user:alice", Enabled: true}}}, Runner: artifactResponseRunner{}, OpenArtifact: func(reference string) (io.ReadCloser, error) {
		opens.Add(1)
		if reference != "reports:generated/report.pdf" {
			return nil, errors.New("unsafe reference")
		}
		if broken.Load() {
			return nil, errors.New("/private/NEVER-EXPOSE credential=SECRET")
		}
		return os.Open(path)
	}})
	p, e := svc.CreateProject(context.Background(), "user:alice", workforce.Project{Name: "reports", BotIDs: []string{"worker"}, CoordinatorBotID: "worker"})
	if e != nil {
		t.Fatal(e)
	}
	task, e := svc.CreateTask(context.Background(), p.Owner, p.ID, workforce.Task{Title: "report", Instructions: "generate"}, &types.Claims{UserID: "alice"})
	if e != nil {
		t.Fatal(e)
	}
	svc.WorkOnce(context.Background())
	task, e = svc.GetTask(context.Background(), p.Owner, task.ID)
	if e != nil {
		t.Fatal(e)
	}
	var reference string
	for _, artifact := range task.Artifacts {
		if artifact.Kind == "document" {
			reference = artifact.Reference
		}
	}
	if reference == "" {
		t.Fatal("attachment dropped")
	}
	backend := startWebREST(t, nil, func(c *RESTConfig) { c.Workforce = svc })
	front := startWebREST(t, nil, func(c *RESTConfig) { c.RemoteConsole = testConsoleClient(t, backend) })
	owner := http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}}
	foreign := http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "intruder", nil)}}
	for _, server := range []*Server{backend, front} {
		beforeForeign := opens.Load()
		resp := doJSON(t, http.MethodGet, webBaseURL(server)+reference, "", foreign)
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatal(resp.StatusCode, string(body))
		}
		if opens.Load() != beforeForeign {
			t.Fatal("foreign download opened artifact")
		}
		before := opens.Load()
		resp = doJSON(t, http.MethodGet, webBaseURL(server)+reference, "", owner)
		body, e = io.ReadAll(resp.Body)
		if e != nil || resp.StatusCode != http.StatusOK || string(body) != content {
			t.Fatal(resp.StatusCode, string(body), e)
		}
		if opens.Load() != before+1 {
			t.Fatal("unexpected opener access")
		}
		if !strings.Contains(resp.Header.Get("Content-Disposition"), "report.pdf") || resp.ContentLength != int64(len(content)) || resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Content-Type") != workforce.ArtifactDownloadMime {
			t.Fatal(resp.Header)
		}
		broken.Store(true)
		resp = doJSON(t, http.MethodGet, webBaseURL(server)+reference, "", owner)
		body, _ = io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusNotFound || strings.Contains(string(body), "NEVER-EXPOSE") || strings.Contains(string(body), "SECRET") {
			t.Fatal(resp.StatusCode, string(body))
		}
		broken.Store(false)
	}
}
