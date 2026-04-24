package compute

import "testing"

func TestStripReasoningTags(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"plain reply":                    "plain reply",
		"<think>chain</think>\nreply":    "reply",
		"<thinking>x</thinking>\n\nhi":   "hi",
		"lead\n<think>mid</think>\nend":  "lead\n\nend",
		"<THINK>shouting</THINK>keep":    "keep",
		"multi <think>a</think> <think>b</think> reply": "multi   reply",
		"no tags at all":                 "no tags at all",
	}
	for in, want := range cases {
		got := stripReasoningTags(in)
		if got != want {
			t.Errorf("stripReasoningTags(%q) = %q; want %q", in, got, want)
		}
	}
}
