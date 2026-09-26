package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// TaskApprovalAPI is owner-facing; it cannot create execution claims or obtain
// private continuation/claim tokens. The runner uses the typed peer service.
type TaskApprovalAPI interface {
	GetTaskApproval(context.Context, *pb.GetTaskApprovalRequest) (*pb.GetTaskApprovalResponse, error)
	ListTaskApproval(context.Context, *pb.ListTaskApprovalRequest) (*pb.ListTaskApprovalResponse, error)
	DecideTaskApproval(context.Context, *pb.DecideTaskApprovalRequest) (*pb.DecideTaskApprovalResponse, error)
	CancelTaskApproval(context.Context, *pb.CancelTaskApprovalRequest) (*pb.CancelTaskApprovalResponse, error)
	RecoverTaskApproval(context.Context, *pb.RecoverTaskApprovalRequest) (*pb.RecoverTaskApprovalResponse, error)
}

func (s *Server) handleTaskApprovals(w http.ResponseWriter, r *http.Request) {
	// These routes always require a bearer token, even on a gateway where normal
	// conversation endpoints permit anonymous access. Browser cookies are not used.
	claims, err := s.authenticate(r, true)
	if err != nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	owner := s.cfg.TaskIdentity.Resolve(claims.UserID).String()
	if owner == "" {
		http.Error(w, "owner required", http.StatusForbidden)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/task-approvals")
	var out proto.Message
	if path == "" && r.Method == http.MethodGet {
		limit := 0
		if raw := r.URL.Query().Get("limit"); raw != "" {
			limit, err = strconv.Atoi(raw)
			if err != nil || limit < 0 || limit > 100 {
				http.Error(w, "limit must be 0..100", http.StatusBadRequest)
				return
			}
		}
		out, err = s.cfg.TaskApprovals.ListTaskApproval(r.Context(), &pb.ListTaskApprovalRequest{Owner: owner, Limit: int32(limit), AfterId: r.URL.Query().Get("after")})
	} else {
		parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
		switch {
		case len(parts) == 1 && parts[0] != "" && r.Method == http.MethodGet:
			out, err = s.cfg.TaskApprovals.GetTaskApproval(r.Context(), &pb.GetTaskApprovalRequest{Id: parts[0], Owner: owner})
		case len(parts) == 2 && parts[0] != "" && r.Method == http.MethodPost:
			var body struct {
				ExtraBudget              *pb.TaskBudget `json:"extra_budget"`
				Revision                 uint64         `json:"revision"`
				Choice                   string         `json:"choice"`
				AcknowledgeDuplicateRisk bool           `json:"acknowledge_duplicate_risk"`
			}
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
			decoder.DisallowUnknownFields()
			if err = decoder.Decode(&body); err != nil {
				http.Error(w, "invalid decision", http.StatusBadRequest)
				return
			}
			if decoder.Decode(new(any)) != io.EOF {
				http.Error(w, "one JSON object required", http.StatusBadRequest)
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
				out, err = s.cfg.TaskApprovals.DecideTaskApproval(r.Context(), &pb.DecideTaskApprovalRequest{Id: parts[0], Owner: owner, Revision: body.Revision, Choice: choice, ExtraBudget: body.ExtraBudget})
			case "cancel":
				out, err = s.cfg.TaskApprovals.CancelTaskApproval(r.Context(), &pb.CancelTaskApprovalRequest{Id: parts[0], Owner: owner, Revision: body.Revision})
			case "recover":
				out, err = s.cfg.TaskApprovals.RecoverTaskApproval(r.Context(), &pb.RecoverTaskApprovalRequest{Id: parts[0], Owner: owner, Revision: body.Revision, AcknowledgeDuplicateRisk: body.AcknowledgeDuplicateRisk})
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
