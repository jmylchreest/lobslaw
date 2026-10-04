package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/console"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// TaskApprovalAPI is owner-facing; it cannot create execution claims or obtain
// private continuation/claim tokens. The runner uses the typed peer service.
type TaskApprovalAPI = console.TaskApprovalAPI

func (s *Server) handleTaskApprovals(w http.ResponseWriter, r *http.Request) {
	authn, ok := s.authenticateTaskOwner(w, r)
	if !ok {
		return
	}
	owner := canonicalUserPrincipal(authn.Claims.UserID)
	if owner == "" {
		http.Error(w, "owner required", http.StatusForbidden)
		return
	}
	api := s.cfg.TaskApprovals
	if s.cfg.RemoteConsole != nil {
		api = &remoteConsoleTasks{client: s.cfg.RemoteConsole, identity: consoleIdentity(authn.Claims)}
	}
	if api == nil {
		http.Error(w, "task approvals unavailable", http.StatusServiceUnavailable)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/task-approvals")
	var out proto.Message
	var err error
	if path == "" && r.Method == http.MethodGet {
		limit := 0
		if raw := r.URL.Query().Get("limit"); raw != "" {
			limit, err = strconv.Atoi(raw)
			if err != nil || limit < 0 || limit > 100 {
				http.Error(w, "limit must be 0..100", http.StatusBadRequest)
				return
			}
		}
		out, err = api.ListTaskApproval(r.Context(), &pb.ListTaskApprovalRequest{Owner: owner, Limit: int32(limit), AfterId: r.URL.Query().Get("after")})
	} else {
		parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
		switch {
		case len(parts) == 1 && parts[0] != "" && r.Method == http.MethodGet:
			out, err = api.GetTaskApproval(r.Context(), &pb.GetTaskApprovalRequest{Id: parts[0], Owner: owner})
		case len(parts) == 2 && parts[0] != "" && r.Method == http.MethodPost:
			body, revision, decodeErr := decodeTaskDecision(w, r)
			if decodeErr != nil {
				http.Error(w, decodeErr.Error(), http.StatusBadRequest)
				return
			}
			switch parts[1] {
			case "decide":
				choices := map[string]pb.TaskApprovalChoice{"once": pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_ONCE, "operation": pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_OPERATION, "risk_labels": pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_RISK_LABELS, "deny": pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_DENY, "budget_extension": pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_BUDGET_EXTENSION}
				choice, ok := choices[body.Choice]
				if !ok {
					http.Error(w, "unknown approval choice", http.StatusBadRequest)
					return
				}
				out, err = api.DecideTaskApproval(r.Context(), &pb.DecideTaskApprovalRequest{Id: parts[0], Owner: owner, Revision: revision, Choice: choice, ExtraBudget: body.ExtraBudget})
			case "cancel":
				out, err = api.CancelTaskApproval(r.Context(), &pb.CancelTaskApprovalRequest{Id: parts[0], Owner: owner, Revision: revision})
			case "recover":
				out, err = api.RecoverTaskApproval(r.Context(), &pb.RecoverTaskApprovalRequest{Id: parts[0], Owner: owner, Revision: revision, AcknowledgeDuplicateRisk: body.AcknowledgeDuplicateRisk})
			default:
				http.NotFound(w, r)
				return
			}
		default:
			http.Error(w, "unsupported task approval route or method", http.StatusMethodNotAllowed)
			return
		}
	}
	if err != nil {
		code := http.StatusServiceUnavailable
		switch status.Code(err) {
		case codes.NotFound:
			code = http.StatusNotFound
		case codes.PermissionDenied:
			code = http.StatusForbidden
		case codes.InvalidArgument:
			code = http.StatusBadRequest
		case codes.Aborted, codes.FailedPrecondition:
			code = http.StatusConflict
		case codes.ResourceExhausted:
			code = http.StatusRequestEntityTooLarge
		}
		http.Error(w, status.Convert(err).Message(), code)
		return
	}
	raw, err := protojson.Marshal(out)
	if err != nil {
		http.Error(w, "encode task approval", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(raw)
}

func (s *Server) authenticateTaskOwner(w http.ResponseWriter, r *http.Request) (requestAuth, bool) {
	authn, err := s.authenticateRequest(r)
	if err != nil || authn.Claims == nil || authn.Claims.UserID == "" || authn.Claims.UserID == "anon" {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return requestAuth{}, false
	}
	if err := s.checkCookieCSRF(r, authn); err != nil {
		http.Error(w, "csrf: origin mismatch", http.StatusForbidden)
		return requestAuth{}, false
	}
	return authn, true
}

const taskDecisionBodyLimit int64 = 4096

type taskDecisionBody struct {
	ExtraBudget              *pb.TaskBudget `json:"extra_budget"`
	Revision                 json.Number    `json:"revision"`
	Choice                   string         `json:"choice"`
	AcknowledgeDuplicateRisk bool           `json:"acknowledge_duplicate_risk"`
}

func decodeTaskDecision(w http.ResponseWriter, r *http.Request) (taskDecisionBody, uint64, error) {
	var body taskDecisionBody
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, taskDecisionBodyLimit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return body, 0, status.Error(codes.InvalidArgument, "invalid decision")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return body, 0, status.Error(codes.InvalidArgument, "one JSON object required")
	}
	revision, err := strconv.ParseUint(string(body.Revision), 10, 64)
	if err != nil || revision == 0 {
		return body, 0, status.Error(codes.InvalidArgument, "valid revision required")
	}
	return body, revision, nil
}
