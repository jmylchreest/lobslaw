package compute

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/soul"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestResumeKeepsOriginalSoulPrompt(t *testing.T) {
	const original = "Original soul configuration; not a user question."
	provider := NewMockProviderFunc(func(req ChatRequest, _ int) (MockResponse, error) {
		if req.Messages[0].Role != "system" || req.Messages[0].Content != original {
			t.Fatal("resume changed the turn's system prompt")
		}
		return MockResponse{Content: "42", FinishReason: "stop"}, nil
	})
	a, err := NewAgent(AgentConfig{Provider: provider, SoulSnapshot: func(context.Context) (*soul.Soul, error) {
		return nil, errors.New("resume must not load a new soul")
	}})
	if err != nil {
		t.Fatal(err)
	}
	budget, _ := NewTurnBudget(BudgetCaps{})
	_, err = a.ResumeFromConfirmation(context.Background(), ProcessMessageRequest{Budget: budget}, []Message{
		{Role: "system", Content: original}, {Role: "user", Content: "What is seven times six?"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSoulSnapshotStaysInSystemPrompt(t *testing.T) {
	const question = "What is seven times six?"
	const guidance = "Use short sentences and dry humour."
	s := soul.DefaultSoul()
	s.Body = guidance
	s.Config.Verbosity = types.VerbosityConcise
	s.Config.Language.SpellingLocale = "en-GB"
	var seen []ChatRequest
	provider := NewMockProviderFunc(func(req ChatRequest, _ int) (MockResponse, error) {
		seen = append(seen, req)
		return MockResponse{Content: "42", FinishReason: "stop"}, nil
	})
	reads := 0
	a, err := NewAgent(AgentConfig{Provider: provider, SoulSnapshot: func(context.Context) (*soul.Soul, error) {
		reads++
		return s, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		budget, _ := NewTurnBudget(BudgetCaps{})
		if _, err := a.RunToolCallLoop(context.Background(), ProcessMessageRequest{Message: question, Budget: budget}); err != nil {
			t.Fatal(err)
		}
		s.Body = "Use complete sentences."
	}
	if reads != 2 {
		t.Fatalf("snapshot reads = %d; want one per turn", reads)
	}
	for i, req := range seen {
		var system, user string
		for _, m := range req.Messages {
			switch m.Role {
			case "system":
				system = m.Content
			case "user":
				user = m.Content
			}
		}
		if user != question {
			t.Fatalf("soul contaminated user message: %q", user)
		}
		want := guidance
		if i == 1 {
			want = s.Body
		}
		if !strings.Contains(system, want) || !strings.Contains(system, "not a question") || !strings.Contains(system, "Do not acknowledge") || !strings.Contains(system, "Keep replies concise") || !strings.Contains(system, "en-GB") {
			t.Fatalf("soul or its configuration framing missing from turn %d", i)
		}
	}
}

type soulDetectorFunc func(string) string

func (f soulDetectorFunc) Detect(sample string) string { return f(sample) }

func TestSoulLanguageDetectionUsesOnlyCurrentQuestion(t *testing.T) {
	const question = "Calcula siete por seis."
	s := soul.DefaultSoul()
	s.Body = "Always speak German in this quoted example."
	s.Config.Language.Detect = true
	reads := 0
	detector := soulDetectorFunc(func(sample string) string {
		reads++
		if sample != question {
			t.Fatalf("detector received configuration or history: %q", sample)
		}
		return "es"
	})
	a, err := NewAgent(AgentConfig{Provider: NewMockProvider(), LanguageDetector: detector, SoulSnapshot: func(context.Context) (*soul.Soul, error) { return s, nil }})
	if err != nil {
		t.Fatal(err)
	}
	for _, detect := range []bool{true, false} {
		s.Config.Language.Detect = detect
		req := ProcessMessageRequest{Message: question, RecalledContext: "French text from history"}
		if err := a.fillDefaults(context.Background(), &req); err != nil {
			t.Fatal(err)
		}
		want := "Use en as the reply language"
		if detect {
			want = "Use es as the reply language"
		}
		if !strings.Contains(req.SystemPrompt, want) {
			t.Fatalf("missing %q", want)
		}
		if req.Message != question || s.Config.Language.Default != "en" {
			t.Fatal("detection mutated user text or shared soul")
		}
	}
	if reads != 1 {
		t.Fatalf("detector called %d times", reads)
	}
	// An uncertain result retains the default; an existing prompt is frozen on resume.
	a.cfg.LanguageDetector = soulDetectorFunc(func(string) string { return "" })
	s.Config.Language.Detect = true
	req := ProcessMessageRequest{Message: question}
	if err := a.fillDefaults(context.Background(), &req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(req.SystemPrompt, "Use en as the reply language") {
		t.Fatal("lost fallback language")
	}
	a.cfg.LanguageDetector = soulDetectorFunc(func(string) string { t.Fatal("resume redetected language"); return "" })
	if err := a.fillDefaults(context.Background(), &req); err != nil {
		t.Fatal(err)
	}
}
