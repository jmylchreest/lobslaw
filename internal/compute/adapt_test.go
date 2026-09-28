package compute

import (
	"context"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestAdaptNilIsNil(t *testing.T) {
	t.Parallel()
	if Adapt(nil) != nil {
		t.Fatal("Adapt(nil) must stay nil so a compute-less node can still bind HTTP")
	}
}

func TestAgentImplementsRunner(t *testing.T) {
	t.Parallel()
	agent, err := NewAgent(AgentConfig{
		Provider: NewMockProvider(MockResponse{Content: "hello"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	var runner turn.Runner = agent
	resp, err := runner.Run(context.Background(), turn.Request{
		Message: "hi",
		Claims:  &types.Claims{UserID: "alice"},
		TurnID:  "t1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Reply != "hello" {
		t.Fatalf("reply = %+v", resp)
	}
}
