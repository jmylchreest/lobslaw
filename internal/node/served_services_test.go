package node

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Every service in the schema must be one a node can serve.
//
// AgentService and ChannelService were declared, generated, and served
// by nobody — for long enough that the design docs described the
// dispatch path they were for as though it existed, and an executor
// carried a `var _ = lobslawv1.InvokeToolRequest{}` to keep an import
// alive for wiring that never arrived. A generated client got three
// methods that returned Unimplemented on every call, and the CLI could
// not even explain why, because an unregistered service has no entry
// in featureConfig.
//
// buf's breaking check cannot catch this: it guards against removing
// something, not against declaring something nothing implements. This
// is the check that would have.
//
// Registration is CONDITIONAL — a compute-only node registers no
// memory service — so this asserts a Register call exists in the tree
// for each service, not that one ran. That is the property worth
// pinning: an RPC nobody can ever serve is schema pretending to be a
// feature.
func TestEveryDeclaredServiceIsServed(t *testing.T) {
	t.Parallel()
	services := lobslawv1.File_lobslaw_v1_lobslaw_proto.Services()
	for i := range services.Len() {
		name := string(services.Get(i).Name())
		if !servedServices[name] {
			t.Errorf("%s is declared in the schema and registered by nothing — "+
				"wire it, or delete it", name)
		}
	}
	// And the other direction: a name left here after its service was
	// renamed would silently stop guarding it.
	for name := range servedServices {
		if services.ByName(protoreflect.Name(name)) == nil {
			t.Errorf("servedServices names %q, which is not in the schema", name)
		}
	}
}

// servedServices is every service some wiring path registers.
//
// Hand-maintained, and deliberately so: it is a statement that
// somebody checked, which is what the generated code cannot tell us.
// Grep for Register<name>Server to verify an entry.
var servedServices = map[string]bool{
	"NodeService":         true,
	"SkillService":        true,
	"SelfLearningService": true,
	"MemoryService":       true,
	"PolicyService":       true,
	"PlanService":         true,
	"AuditService":        true,
	"StorageService":      true,
	"EnrolmentService":    true,
	"TraceService":        true,
	"IdentityService":     true,
	"SessionService":      true,
}
