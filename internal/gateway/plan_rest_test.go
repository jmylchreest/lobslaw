package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// fakePlanService records the GetPlan request so tests can assert
// on window parsing without standing up a real Raft-backed service.
type fakePlanService struct {
	mu       sync.Mutex
	lastReq  *lobslawv1.GetPlanRequest
	response *lobslawv1.GetPlanResponse
	err      error
}

func (f *fakePlanService) GetPlan(_ context.Context, req *lobslawv1.GetPlanRequest) (*lobslawv1.GetPlanResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastReq = req
	if f.err != nil {
		return nil, f.err
	}
	if f.response != nil {
		return f.response, nil
	}
	return &lobslawv1.GetPlanResponse{
		Window: durationpb.New(24 * time.Hour),
	}, nil
}

// startRESTWithPlan brings up a Server with the fake PlanService
// wired. Agent is nil — /v1/plan doesn't need the agent.
func startRESTWithPlan(t *testing.T, plan PlanService) string {
	t.Helper()
	srv := NewServer(RESTConfig{
		Addr: "127.0.0.1:0",
		Plan: plan,
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Start(ctx) }()
	deadline := time.Now().Add(time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if srv.Addr() == "" {
		t.Fatal("server didn't bind")
	}
	return "http://" + srv.Addr()
}

func TestRESTPlanHappyPath(t *testing.T) {
	t.Parallel()
	plan := &fakePlanService{
		response: &lobslawv1.GetPlanResponse{
			Window: durationpb.New(12 * time.Hour),
			Commitments: []*lobslawv1.AgentCommitment{
				{
					Id:     "c1",
					Reason: "oven check",
					Status: "pending",
					DueAt:  timestamppb.New(time.Now().Add(time.Hour)),
				},
			},
			ScheduledTasks: []*lobslawv1.ScheduledTaskRecord{
				{
					Id:         "t1",
					Name:       "nightly-sync",
					Schedule:   "0 3 * * *",
					HandlerRef: "sync",
					NextRun:    timestamppb.New(time.Now().Add(3 * time.Hour)),
				},
			},
		},
	}
	url := startRESTWithPlan(t, plan)

	resp, err := http.Get(url + "/v1/plan")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var body planResponseJSON
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.WindowSeconds != (12 * time.Hour).Seconds() {
		t.Errorf("window: %f", body.WindowSeconds)
	}
	if len(body.Commitments) != 1 || body.Commitments[0].ID != "c1" {
		t.Errorf("commitments: %+v", body.Commitments)
	}
	if len(body.ScheduledTasks) != 1 || body.ScheduledTasks[0].Schedule != "0 3 * * *" {
		t.Errorf("tasks: %+v", body.ScheduledTasks)
	}
}

func TestRESTPlanWindowQueryParam(t *testing.T) {
	t.Parallel()
	plan := &fakePlanService{}
	url := startRESTWithPlan(t, plan)

	resp, err := http.Get(url + "/v1/plan?window=2h30m")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	plan.mu.Lock()
	defer plan.mu.Unlock()
	if plan.lastReq == nil || plan.lastReq.Window == nil {
		t.Fatal("handler didn't forward window")
	}
	if plan.lastReq.Window.AsDuration() != 2*time.Hour+30*time.Minute {
		t.Errorf("window: %v", plan.lastReq.Window.AsDuration())
	}
}

func TestRESTPlanInvalidWindowFallsThrough(t *testing.T) {
	t.Parallel()
	plan := &fakePlanService{}
	url := startRESTWithPlan(t, plan)

	resp, err := http.Get(url + "/v1/plan?window=nonsense")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	plan.mu.Lock()
	defer plan.mu.Unlock()
	if plan.lastReq == nil {
		t.Fatal("handler never called the service")
	}
	if plan.lastReq.Window != nil {
		t.Errorf("invalid window should be dropped; got %v", plan.lastReq.Window)
	}
}

func TestRESTPlanWrongMethod(t *testing.T) {
	t.Parallel()
	url := startRESTWithPlan(t, &fakePlanService{})
	resp, err := http.Post(url+"/v1/plan", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST should be 405; got %d", resp.StatusCode)
	}
}

// TestRESTPlanUnmountedWithoutService — without Plan wired, GET
// /v1/plan 404s from the default mux. Confirms we don't leak an
// endpoint in minimal deployments.
func TestRESTPlanUnmountedWithoutService(t *testing.T) {
	t.Parallel()
	url, _ := startREST(t, nil)
	resp, err := http.Get(url + "/v1/plan")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unmounted /v1/plan should 404; got %d", resp.StatusCode)
	}
}
