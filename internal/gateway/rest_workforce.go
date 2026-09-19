package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/workforce"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

var errWorkforceMethod = errors.New("method not allowed")

func (s *Server) registerWorkforceRoutes(mux *http.ServeMux) {
	for _, path := range []string{"/v1/projects", "/v1/projects/", "/v1/tasks/", "/v1/routines/", "/v1/triggers/", "/v1/attention"} {
		mux.HandleFunc(path, s.consoleRoute(s.handleWorkforce))
	}
}
func (s *Server) workforceError(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	switch {
	case errors.Is(err, errWorkforceMethod):
		code = http.StatusMethodNotAllowed
	case errors.Is(err, workforce.ErrForbidden):
		code = http.StatusForbidden
	case errors.Is(err, workforce.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, memory.ErrClaimConflict):
		code = http.StatusConflict
	case errors.Is(err, workforce.ErrUnavailable):
		code = http.StatusServiceUnavailable
	}
	s.jsonErr(w, code, err.Error())
}

func (s *Server) handleWorkforce(w http.ResponseWriter, r *http.Request) {
	authn, e := s.authenticateRequest(r)
	if e != nil || authn.Claims == nil {
		s.jsonErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if e = s.checkCookieCSRF(r, authn); e != nil {
		s.jsonErr(w, http.StatusForbidden, e.Error())
		return
	}
	if s.cfg.Workforce == nil {
		s.jsonErr(w, http.StatusServiceUnavailable, "compute-teams workforce is unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, consoleForwardBodyLimit)
	owner := s.principalOf(r)
	req := workforceRequest{service: s.cfg.Workforce, request: r, owner: owner, claims: authn.Claims}
	result, e := req.dispatch()
	if e != nil {
		s.workforceError(w, e)
		return
	}
	if download, ok := result.(*workforce.ArtifactDownload); ok {
		s.serveWorkforceArtifact(w, r, download)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/messages") && r.Method == http.MethodPost {
		task, ok := result.(*workforce.Task)
		if !ok {
			s.jsonErr(w, http.StatusInternalServerError, "missing task")
			return
		}
		ctx, stop := s.bindStream(r.Context(), authn.LoginID)
		defer stop()
		s.streamWorkforceChat(w, r.WithContext(ctx), owner, task)
		return
	}
	respondJSON(w, http.StatusOK, result)
}

type workforceRequest struct {
	service *workforce.Service
	request *http.Request
	owner   string
	claims  *types.Claims
}

func (q workforceRequest) decode(v any) error {
	if e := json.NewDecoder(q.request.Body).Decode(v); e != nil {
		return workforce.ErrInvalid
	}
	return nil
}
func (q workforceRequest) dispatch() (any, error) {
	parts := strings.Split(strings.TrimPrefix(q.request.URL.Path, "/v1/"), "/")
	switch parts[0] {
	case "projects":
		return q.projects(parts[1:])
	case "tasks":
		return q.tasks(parts[1:])
	case "routines":
		return q.routines(parts[1:])
	case "triggers":
		return q.fire(parts[1:])
	case "attention":
		if len(parts) == 1 && q.request.Method == http.MethodGet {
			items, e := q.service.Attention(q.request.Context(), q.owner)
			return map[string]any{"items": items}, e
		}
	}
	return nil, workforce.ErrNotFound
}

func (q workforceRequest) projects(parts []string) (any, error) {
	ctx := q.request.Context()
	method := q.request.Method
	s := q.service
	if len(parts) == 0 {
		switch method {
		case http.MethodGet:
			projects, e := s.ListProjects(ctx, q.owner)
			return map[string]any{"projects": projects}, e
		case http.MethodPost:
			var p workforce.Project
			if e := q.decode(&p); e != nil {
				return nil, e
			}
			return s.CreateProject(ctx, q.owner, p)
		default:
			return nil, errWorkforceMethod
		}
	}
	id := parts[0]
	if e := s.AuthorizeProject(ctx, q.owner, id); e != nil {
		return nil, e
	}
	if len(parts) == 2 {
		return q.projectCollection(id, parts[1])
	}
	if len(parts) != 1 {
		return nil, workforce.ErrNotFound
	}
	switch method {
	case http.MethodGet:
		return s.GetProject(ctx, q.owner, id)
	case http.MethodPatch:
		return q.patchProject(id)
	default:
		return nil, errWorkforceMethod
	}
}
func (q workforceRequest) patchProject(id string) (any, error) {
	p, e := q.service.GetProject(q.request.Context(), q.owner, id)
	if e != nil {
		return nil, e
	}
	var raw json.RawMessage
	if e = q.decode(&raw); e != nil {
		return nil, e
	}
	var revision struct {
		Revision *uint64 `json:"revision"`
	}
	if e = json.Unmarshal(raw, &revision); e != nil || revision.Revision == nil {
		return nil, workforce.ErrInvalid
	}
	if e = json.Unmarshal(raw, p); e != nil {
		return nil, workforce.ErrInvalid
	}
	p.ID = id
	return q.service.UpdateProject(q.request.Context(), q.owner, *p)
}
func (q workforceRequest) projectCollection(id, kind string) (any, error) {
	if q.request.Method == http.MethodGet {
		return q.projectList(id, kind)
	}
	if q.request.Method != http.MethodPost {
		return nil, errWorkforceMethod
	}
	ctx := q.request.Context()
	s := q.service
	switch kind {
	case "tasks":
		var t workforce.Task
		if e := q.decode(&t); e != nil {
			return nil, e
		}
		return s.CreateTask(ctx, q.owner, id, t, q.claims)
	case "routines":
		var routine workforce.Routine
		if e := q.decode(&routine); e != nil {
			return nil, e
		}
		return s.CreateRoutine(ctx, q.owner, id, routine)
	case "triggers":
		var trigger workforce.Trigger
		if e := q.decode(&trigger); e != nil {
			return nil, e
		}
		return s.CreateTrigger(ctx, q.owner, id, trigger)
	case "messages":
		var body struct {
			Message string `json:"message"`
			BotID   string `json:"bot_id"`
		}
		if e := q.decode(&body); e != nil {
			return nil, e
		}
		return s.Chat(ctx, q.owner, id, body.Message, body.BotID, q.claims)
	default:
		return nil, workforce.ErrNotFound
	}
}
func (q workforceRequest) projectList(id, kind string) (any, error) {
	ctx := q.request.Context()
	s := q.service
	switch kind {
	case "tasks":
		tasks, e := s.ListTasks(ctx, q.owner, id)
		return map[string]any{"tasks": tasks}, e
	case "routines":
		routines, e := s.ListRoutines(ctx, q.owner, id)
		return map[string]any{"routines": routines}, e
	case "triggers":
		triggers, e := s.ListTriggers(ctx, q.owner, id)
		return map[string]any{"triggers": triggers}, e
	case "messages":
		messages, e := s.Messages(ctx, q.owner, id)
		return map[string]any{"messages": messages}, e
	default:
		return nil, workforce.ErrNotFound
	}
}
func (q workforceRequest) tasks(parts []string) (any, error) {
	if len(parts) == 0 {
		return nil, workforce.ErrNotFound
	}
	ctx := q.request.Context()
	id := parts[0]
	if len(parts) == 3 && parts[1] == "artifacts" && q.request.Method == http.MethodGet {
		return q.service.OpenTaskArtifact(ctx, q.owner, id, parts[2])
	}
	if len(parts) == 2 && parts[1] == "result" && q.request.Method == http.MethodGet {
		t, e := q.service.GetTask(ctx, q.owner, id)
		if e != nil {
			return nil, e
		}
		return map[string]string{"result": t.Result}, nil
	}
	if len(parts) != 1 {
		return nil, workforce.ErrNotFound
	}
	switch q.request.Method {
	case http.MethodGet:
		return q.service.GetTask(ctx, q.owner, id)
	case http.MethodPatch:
		var body struct {
			Revision uint64 `json:"revision"`
			Action   string `json:"action"`
			Answer   string `json:"answer"`
		}
		if e := q.decode(&body); e != nil {
			return nil, e
		}
		return q.service.ActTask(ctx, q.owner, id, body.Revision, body.Action, body.Answer, q.claims)
	default:
		return nil, errWorkforceMethod
	}
}
func (q workforceRequest) routines(parts []string) (any, error) {
	if len(parts) == 2 && parts[1] == "run" && q.request.Method == http.MethodPost {
		return q.service.RunRoutine(q.request.Context(), q.owner, parts[0], q.claims)
	}
	if len(parts) != 1 {
		return nil, workforce.ErrNotFound
	}
	if q.request.Method != http.MethodPatch {
		return nil, errWorkforceMethod
	}
	var body struct {
		workforce.Routine
		Action string `json:"action"`
	}
	if e := q.decode(&body); e != nil {
		return nil, e
	}
	return q.service.ActRoutine(q.request.Context(), q.owner, parts[0], body.Revision, body.Action, body.Routine, q.claims)
}
func (q workforceRequest) fire(parts []string) (any, error) {
	if len(parts) != 2 || parts[1] != "fire" {
		return nil, workforce.ErrNotFound
	}
	if q.request.Method != http.MethodPost {
		return nil, errWorkforceMethod
	}
	var body struct {
		EventID string          `json:"event_id"`
		Payload json.RawMessage `json:"payload"`
	}
	if e := q.decode(&body); e != nil {
		return nil, e
	}
	task, duplicate, e := q.service.Fire(q.request.Context(), q.owner, parts[0], body.EventID, string(body.Payload), q.claims)
	return map[string]any{"task": task, "duplicate": duplicate}, e
}

func writeSSE(w http.ResponseWriter, event string, payload any) {
	if flusher, ok := w.(http.Flusher); ok {
		sendSSE(w, flusher, event, payload)
	}
}
func (s *Server) streamWorkforceChat(w http.ResponseWriter, r *http.Request, owner string, task *workforce.Task) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ticker := time.NewTicker(workforce.PollInterval)
	defer ticker.Stop()
	deadline := time.NewTimer(workforce.DefaultTimeout + workforce.ClaimGrace)
	defer deadline.Stop()
	writeSSE(w, "start", map[string]string{"turn_id": task.ID, "task_id": task.ID})
	for {
		switch task.Status {
		case workforce.StatusDone:
			writeSSE(w, "reply", map[string]string{"text": task.Result, "task_id": task.ID})
			return
		case workforce.StatusApproval:
			writeSSE(w, "needs_confirmation", map[string]string{"reason": task.Question, "prompt_id": task.PromptID, "task_id": task.ID})
			return
		case workforce.StatusBlocked:
			writeSSE(w, "blocked", map[string]string{"reason": task.Question, "task_id": task.ID})
			return
		case workforce.StatusFailed, workforce.StatusCancelled:
			writeSSE(w, "error", map[string]string{"message": task.Error, "task_id": task.ID, "status": task.Status})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			writeSSE(w, "error", map[string]string{"message": "Work remains available in the task board", "task_id": task.ID})
			return
		case <-ticker.C:
			var e error
			task, e = s.cfg.Workforce.GetTask(r.Context(), owner, task.ID)
			if e != nil {
				writeSSE(w, "error", map[string]string{"message": e.Error()})
				return
			}
			writeSSE(w, "working", map[string]string{"task_id": task.ID, "status": task.Status})
		}
	}
}
