package main

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// A service registered only when the thing behind it exists answers
// "Unimplemented desc = unknown service lobslaw.v1.SelfLearningService",
// which reads as a broken cluster rather than one configured this way.
func TestAnAbsentServiceExplainsTheSettingThatEnablesIt(t *testing.T) {
	t.Parallel()
	err := status.Error(codes.Unimplemented,
		"unknown service lobslaw.v1.SelfLearningService")

	got := explainUnimplemented(err, "10.0.0.4:47443")
	if got == nil {
		t.Fatal("the error was swallowed")
	}
	if !strings.Contains(got.Error(), "self_learning") {
		t.Errorf("no setting named: %v", got)
	}
	// Per-node, because another member of the same cluster may have
	// it switched on — and the operator needs to know which one they
	// just ruled out.
	if !strings.Contains(got.Error(), "10.0.0.4:47443") {
		t.Errorf("the node was not named: %v", got)
	}
}

// A real failure dressed as a configuration note sends somebody
// editing config.toml over a network partition.
func TestOtherErrorsPassThroughUntouched(t *testing.T) {
	t.Parallel()
	for _, err := range []error{
		errors.New("connection refused"),
		status.Error(codes.Unavailable, "no healthy upstream"),
		status.Error(codes.PermissionDenied, "certificate expired"),
		// Unimplemented, but not a service this maps — a genuinely
		// unimplemented METHOD on a service that is present.
		status.Error(codes.Unimplemented, "unknown method Frobnicate"),
	} {
		if got := explainUnimplemented(err, "n:1"); !errors.Is(got, err) {
			t.Errorf("%v was rewritten to %v", err, got)
		}
	}
	if explainUnimplemented(nil, "n:1") != nil {
		t.Error("nil became an error")
	}
}

// The map is keyed by the service name gRPC puts in the error. A
// renamed service leaves a key matching nothing, and the command goes
// back to emitting the raw "unknown service" it exists to replace —
// silently, because a stale key still compiles.
func TestEveryMappedServiceNameIsReal(t *testing.T) {
	t.Parallel()
	real := map[string]bool{}
	for _, d := range []*grpc.ServiceDesc{
		&lobslawv1.NodeService_ServiceDesc,
		&lobslawv1.SkillService_ServiceDesc,
		&lobslawv1.SelfLearningService_ServiceDesc,
		&lobslawv1.MemoryService_ServiceDesc,
		&lobslawv1.PolicyService_ServiceDesc,
		&lobslawv1.AgentService_ServiceDesc,
		&lobslawv1.ChannelService_ServiceDesc,
		&lobslawv1.PlanService_ServiceDesc,
		&lobslawv1.AuditService_ServiceDesc,
		&lobslawv1.StorageService_ServiceDesc,
		&lobslawv1.EnrolmentService_ServiceDesc,
		&lobslawv1.TraceService_ServiceDesc,
		&lobslawv1.IdentityService_ServiceDesc,
		&lobslawv1.SessionService_ServiceDesc,
	} {
		real[d.ServiceName] = true
	}
	for svc := range featureConfig {
		if !real[svc] {
			t.Errorf("featureConfig names %q, which is not a registered service", svc)
		}
	}
}
