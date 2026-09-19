package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/workforce"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestProjectQuestionIsNotReportedAsFailedWork(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/projects/project/messages", nil)
	(&Server{}).streamWorkforceChat(w, r, "user:alice", &workforce.Task{ID: "task", Status: workforce.StatusBlocked, Question: "Which account should I use?"})
	if !strings.Contains(w.Body.String(), "event: blocked") || !strings.Contains(w.Body.String(), "Which account should I use?") || strings.Contains(w.Body.String(), "event: error") {
		t.Fatalf("human question lost or misclassified: %s", w.Body.String())
	}
}

type workforceRepo struct {
	mu   sync.Mutex
	rows map[string][]byte
}

func (r *workforceRepo) Get(_ context.Context, id string) (*workforce.State, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, ok := r.rows[id]
	if !ok {
		return nil, workforce.ErrNotFound
	}
	var st workforce.State
	e := json.Unmarshal(raw, &st)
	return &st, e
}
func (r *workforceRepo) List(context.Context) ([]*workforce.State, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []*workforce.State{}
	for _, raw := range r.rows {
		var st workforce.State
		if e := json.Unmarshal(raw, &st); e != nil {
			return nil, e
		}
		out = append(out, &st)
	}
	return out, nil
}
func (r *workforceRepo) Put(_ context.Context, st *workforce.State, rev uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var prev workforce.State
	if raw := r.rows[st.Project.ID]; raw != nil {
		if e := json.Unmarshal(raw, &prev); e != nil {
			return e
		}
	}
	if prev.Revision != rev {
		return memory.ErrClaimConflict
	}
	st.Revision = rev + 1
	raw, e := json.Marshal(st)
	if e != nil {
		return e
	}
	r.rows[st.Project.ID] = raw
	return nil
}

func TestWorkforceRemoteOwnershipMutationAndStream(t *testing.T) {
	t.Parallel()
	repo := &workforceRepo{rows: map[string][]byte{}}
	bots := &memBots{recs: map[string]*lobslawv1.BotRecord{"worker": {Id: "worker", Owner: "user:alice", Enabled: true}, "bob": {Id: "bob", Owner: "user:bob", Enabled: true}}}
	runner := &captureRunner{}
	svc := workforce.New(workforce.Config{Repository: repo, Bots: bots, Runner: runner})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Run(ctx)
	backend := startWebREST(t, runner, func(c *RESTConfig) { c.Workforce = svc })
	front := startWebREST(t, nil, func(c *RESTConfig) { c.RemoteConsole = testConsoleClient(t, backend) })
	auth := http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}}
	base := webBaseURL(front)
	response := doJSON(t, http.MethodPost, base+"/v1/projects", `{"name":"owned","owner":"user:bob","bot_ids":["worker"],"coordinator_bot_id":"worker"}`, auth)
	var p workforce.Project
	if e := json.NewDecoder(response.Body).Decode(&p); e != nil {
		t.Fatal(e)
	}
	if response.StatusCode != http.StatusOK || p.Owner != "user:alice" {
		t.Fatal(response.StatusCode, p)
	}
	bob, e := svc.CreateProject(ctx, "user:bob", workforce.Project{Name: "private", BotIDs: []string{"bob"}, CoordinatorBotID: "bob"})
	if e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{"", "/tasks", "/messages", "/routines", "/triggers"} {
		r := doJSON(t, http.MethodGet, base+"/v1/projects/"+bob.ID+path, "", auth)
		if r.StatusCode != http.StatusForbidden {
			t.Fatal(path, r.StatusCode)
		}
	}
	r := doJSON(t, http.MethodGet, base+"/v1/projects", "", auth)
	raw, e := io.ReadAll(r.Body)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(raw), bob.ID) {
		t.Fatal("owner list leaked")
	}
	r = doJSON(t, http.MethodPost, base+"/v1/projects/"+p.ID+"/messages", `{"message":"hello"}`, auth)
	raw, e = io.ReadAll(r.Body)
	if e != nil {
		t.Fatal(e)
	}
	if r.StatusCode != http.StatusOK || !strings.Contains(string(raw), "event: reply") {
		t.Fatal(r.StatusCode, string(raw))
	}
	request := runner.lastRequest()
	if request.Claims.UserID != "alice" || request.Principal.String() != "bot:worker" {
		t.Fatal(request)
	}
	r = doJSON(t, http.MethodGet, base+"/v1/attention", "", auth)
	raw, e = io.ReadAll(r.Body)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(raw), "deliverable") || strings.Contains(string(raw), bob.ID) {
		t.Fatal(string(raw))
	}
}
