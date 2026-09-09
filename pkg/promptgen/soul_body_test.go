package promptgen

import (
	"strings"
	"testing"
)

func TestSoulBodyIsConfigurationNotAQuestion(t *testing.T) {
	const body = "Use short sentences and dry humour."
	prompt := Generate(GenerateInput{SoulBody: body})
	contract := strings.Index(prompt, "# How To Read This Prompt")
	bodyAt := strings.Index(prompt, body)
	if bodyAt < 0 || contract < 0 || contract > bodyAt {
		t.Fatal("soul body is missing or precedes the prompt contract")
	}
	for _, rule := range []string{"not a message", "Do not acknowledge", "writing style", "not a question", "user's most recent message"} {
		if !strings.Contains(prompt, rule) {
			t.Errorf("missing framing %q", rule)
		}
	}
	if strings.Count(prompt, body) != 1 {
		t.Fatal("soul body duplicated")
	}
}
