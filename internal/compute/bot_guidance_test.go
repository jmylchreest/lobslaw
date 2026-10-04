package compute

import (
	"context"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// A bot's standing brief must reach the model as trusted guidance, or
// every bot answers as the node's assistant.
func TestBotInstructionsReachTheSystemPrompt(t *testing.T) {
	t.Parallel()
	agent, err := NewAgent(AgentConfig{
		Provider: NewMockProvider(MockResponse{Content: "ok"}),
		Soul:     func() *types.SoulConfig { return &types.SoulConfig{Name: "assistant"} },
	})
	if err != nil {
		t.Fatal(err)
	}
	req := ProcessMessageRequest{
		Message: "who are you?",
		BotID:   "designer",
		Bot: &BotProfile{
			ID:           "designer",
			DisplayName:  "Designer",
			Instructions: "You handle visual and interaction design.",
		},
	}
	if err := agent.fillDefaults(context.Background(), &req); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Designer", "visual and interaction design"} {
		if !strings.Contains(req.SystemPrompt, want) {
			t.Fatalf("system prompt is missing %q:\n%s", want, req.SystemPrompt)
		}
	}
}
