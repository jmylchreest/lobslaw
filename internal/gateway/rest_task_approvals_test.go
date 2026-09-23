package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/auth"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type taskAPISpy struct {
	TaskApprovalAPI
	owner string
	calls int
}

func (s *taskAPISpy) ListTaskApproval(_ context.Context, q *pb.ListTaskApprovalRequest) (*pb.ListTaskApprovalResponse, error) {
	s.owner = q.Owner
	s.calls++
	return &pb.ListTaskApprovalResponse{}, nil
}
func (s *taskAPISpy) DecideTaskApproval(_ context.Context, q *pb.DecideTaskApprovalRequest) (*pb.DecideTaskApprovalResponse, error) {
	s.owner = q.Owner
	s.calls++
	return &pb.DecideTaskApprovalResponse{}, nil
}
func TestTaskApprovalRESTRequiresIdentityEvenWithoutRequireAuth(t *testing.T) {
	v, e := auth.NewValidator(auth.Config{AllowHS256: true, HS256Secret: restTestSecret})
	if e != nil {
		t.Fatal(e)
	}
	api := new(taskAPISpy)
	server := NewServer(RESTConfig{JWTValidator: v, TaskApprovals: api}, nil)
	r := httptest.NewRequest(http.MethodGet, "/v1/task-approvals", nil)
	w := httptest.NewRecorder()
	server.handleTaskApprovals(w, r)
	if w.Code != http.StatusUnauthorized || api.calls != 0 {
		t.Fatal("anonymous approval access")
	}
	r.Header.Set("Authorization", "Bearer "+mintValidJWT(t, "owner"))
	w = httptest.NewRecorder()
	server.handleTaskApprovals(w, r)
	if w.Code != http.StatusOK || api.owner != "user:test-user" {
		t.Fatalf("owner=%q status=%d body=%s", api.owner, w.Code, w.Body.String())
	}
}
func TestTaskApprovalRESTDoesNotAcceptCallerChosenOwner(t *testing.T) {
	v, e := auth.NewValidator(auth.Config{AllowHS256: true, HS256Secret: restTestSecret})
	if e != nil {
		t.Fatal(e)
	}
	api := new(taskAPISpy)
	server := NewServer(RESTConfig{JWTValidator: v, TaskApprovals: api}, nil)
	for _, body := range []string{`{"revision":1,"choice":"once","owner":"user:bob"}`, `{"revision":1,"choice":"once"}{"choice":"operation"}`} {
		r := httptest.NewRequest(http.MethodPost, "/v1/task-approvals/task/decide", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+mintValidJWT(t, "owner"))
		w := httptest.NewRecorder()
		server.handleTaskApprovals(w, r)
		if w.Code != http.StatusBadRequest || api.calls != 0 {
			t.Fatalf("unsafe body accepted: %s %d", body, w.Code)
		}
	}
}
