package main

import (
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A service that is switched OFF is not a broken cluster.
//
// Several services are registered only when the thing behind them
// exists — self-learning has no RPC surface when no artefact can be
// written, and the skill service none when there is no skill store.
// That is a deliberate property, stated in wire_self_taught.go as
// "absence, not a guarded call".
//
// From the CLI it arrived as:
//
//	rpc error: code = Unimplemented desc = unknown service
//	lobslaw.v1.SelfLearningService
//
// which reads like a cluster that is broken rather than one that was
// configured this way. `trace` already got this right — "tracing is
// OFF on this node — set [trace] enabled = true and restart" — and
// this is the same courtesy for the rest.

// featureConfig maps a gRPC service to the configuration that turns it
// on.
//
// Keyed by the service name gRPC puts in the error, so the mapping
// survives a command being renamed and breaks loudly if a service is.
var featureConfig = map[string]string{
	"lobslaw.v1.SelfLearningService": "self-learning is off on this node — set [self_learning] mode",
	"lobslaw.v1.SkillService":        "no skill store is wired on this node — see [skills]",
	"lobslaw.v1.AuditService":        "auditing is off on this node — enable [audit.local] or [audit.raft]",
	"lobslaw.v1.MemoryService":       "this node hosts no memory — set [memory] enabled = true",
	"lobslaw.v1.SessionService":      "this node hosts no session store — set [memory] enabled = true",
	"lobslaw.v1.IdentityService":     "this node hosts no memory — set [memory] enabled = true",
	"lobslaw.v1.EnrolmentService":    "enrolment is not configured — see [cluster.mtls] enrol_addr",
	"lobslaw.v1.PolicyService":       "this node runs no policy engine — set [memory] enabled = true",
	"lobslaw.v1.TraceService":        "tracing is off on this node — set [trace] enabled = true",
}

// explainUnimplemented rewrites an absent-service error into a
// sentence naming the feature and the setting that enables it.
//
// Anything else passes through untouched: a real failure must not be
// dressed up as a configuration note.
func explainUnimplemented(err error, source string) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unimplemented {
		return err
	}
	for svc, hint := range featureConfig {
		if !strings.Contains(st.Message(), svc) {
			continue
		}
		// The node is named because the answer is per-node: another
		// member of the same cluster may well have it switched on.
		return fmt.Errorf("%s (%s), so there is nothing to show", hint, source)
	}
	return err
}
