package compute

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/turn"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestExecutionReceiptsSurviveRunnerAndWireAdapters(t *testing.T) {
	t.Parallel()
	for _, execution := range []string{"", turn.ReceiptExecuted, turn.ReceiptRefused, turn.ReceiptApprovalRequired, turn.ReceiptBudgetRequired, turn.ReceiptUnknown} {
		response := responseToTurn(&ProcessMessageResponse{ToolCalls: []ToolInvocation{{CallID: "call", ToolName: "write_file", ExecutionStatus: execution}}})
		raw, err := proto.Marshal(responseToProto(response))
		if err != nil {
			t.Fatal(err)
		}
		wire := new(pb.RunTurnResponse)
		if err := proto.Unmarshal(raw, wire); err != nil {
			t.Fatal(err)
		}
		got := responseFromProto(wire)
		if len(got.ToolCalls) != 1 || got.ToolCalls[0].ExecutionStatus != execution {
			t.Fatalf("status %q lost through adapters: %+v", execution, got)
		}
	}
}
