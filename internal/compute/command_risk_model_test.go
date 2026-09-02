package compute

import (
	"context"
	"log/slog"
	"testing"
)

// What the model is allowed to move is the security-relevant part, so
// it is tested without a provider: adjudicate takes the static verdict
// and the model's and returns the final one, and these rows are the
// whole contract.

func staticVerdict(labels ...RiskLabel) RiskVerdict {
	return RiskVerdict{Labels: labels}
}

func TestAdjudicate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		static    []RiskLabel
		model     RiskLabel
		modelOK   bool
		trust     RiskTrust
		want      []RiskLabel
		fromModel bool
	}{
		// Adding is allowed under every setting. It can only make the
		// subset check stricter, so a wrong answer costs a confirmation
		// nobody needed to give — which is why it needs no notion of
		// "worse" and why advisory is the default.
		{"advisory adds", L(LabelWrites), LabelNetwork, true, RiskTrustAdvisory, L(LabelWrites, LabelNetwork), true},
		{"resolve_unknown adds too", L(LabelWrites), LabelDeletes, true, RiskTrustResolveUnknown, L(LabelWrites, LabelDeletes), true},

		// A label the classifier positively determined is NEVER
		// removed, under either setting. A command cannot argue its own
		// deletion away.
		{"advisory cannot remove", L(LabelDeletes), LabelReads, true, RiskTrustAdvisory, L(LabelDeletes), false},
		{"resolve_unknown cannot remove either", L(LabelDeletes), LabelReads, true, RiskTrustResolveUnknown, L(LabelDeletes), false},
		{"a write is not talked down to a read", L(LabelWrites), LabelReads, true, RiskTrustResolveUnknown, L(LabelWrites), false},

		// The one permitted replacement, and only into the gap where
		// static reading had no opinion at all.
		{"advisory leaves unreadable alone", L(LabelUnreadable), LabelReads, true, RiskTrustAdvisory, L(LabelUnreadable), false},
		{"resolve_unknown fills the gap", L(LabelUnreadable), LabelReads, true, RiskTrustResolveUnknown, L(LabelReads), true},
		{"resolve_unknown can fill it with something worse", L(LabelUnreadable), LabelDeletes, true, RiskTrustResolveUnknown, L(LabelDeletes), true},

		// No usable answer means the static verdict stands.
		{"a declined verdict changes nothing", L(LabelUnreadable), "", false, RiskTrustResolveUnknown, L(LabelUnreadable), false},
		{"a declined verdict on a write changes nothing", L(LabelWrites), "", false, RiskTrustAdvisory, L(LabelWrites), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			judge := &RiskJudge{
				provider: stubRiskProvider{label: tt.model, ok: tt.modelOK},
				trust:    tt.trust,
				log:      slog.New(slog.DiscardHandler),
			}
			got := AdjudicateWith(context.Background(), staticVerdict(tt.static...), "some command", judge)
			if !sameLabels(got.Labels, tt.want) {
				t.Errorf("labels = %v, want %v", got.Labels, tt.want)
			}
			if got.FromModel != tt.fromModel {
				t.Errorf("FromModel = %v, want %v", got.FromModel, tt.fromModel)
			}
		})
	}
}

// A read verdict is already the cheapest answer there is and nothing
// may lower it, so the model is not asked at all — the path the whole
// change exists to make free must stay free.
func TestAdjudicateDoesNotAskAboutReads(t *testing.T) {
	t.Parallel()
	p := &countingRiskProvider{label: LabelDeletes}
	judge := &RiskJudge{provider: p, trust: RiskTrustAdvisory, log: slog.New(slog.DiscardHandler)}

	got := AdjudicateWith(context.Background(), staticVerdict(LabelReads), "uname -a", judge)
	if !hasLabel(got.Labels, LabelReads) {
		t.Errorf("labels = %v, want reads", got.Labels)
	}
	if p.calls != 0 {
		t.Errorf("the model was called %d times for a read", p.calls)
	}
}

func TestAdjudicateWithoutAJudge(t *testing.T) {
	t.Parallel()
	got := AdjudicateWith(context.Background(), staticVerdict(LabelUnreadable), "for x in a; do echo $x; done", nil)
	if !hasLabel(got.Labels, LabelUnreadable) || got.FromModel {
		t.Errorf("got %v/%v, want unreadable/false", got.Labels, got.FromModel)
	}
}

// Everything the model can get wrong is one failure mode: no usable
// answer. Nothing but an enum value is ever read out of the reply,
// because the command that produced it is attacker-influenced.
func TestParseRiskVerdict(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)
	tests := []struct {
		name    string
		content string
		want    RiskLabel
		wantOK  bool
	}{
		{"a clean answer", `{"tier":"writes","confidence":"high"}`, LabelWrites, true},
		{"wrapped in a fence", "```json\n{\"tier\":\"reads\",\"confidence\":\"high\"}\n```", LabelReads, true},
		{"with prose around it", `Sure! {"tier":"deletes","confidence":"high"} hope that helps`, LabelDeletes, true},
		{"upper case", `{"tier":"NETWORK","confidence":"HIGH"}`, LabelNetwork, true},

		{"low confidence is discarded", `{"tier":"reads","confidence":"low"}`, "", false},
		{"a missing confidence is discarded", `{"tier":"reads"}`, "", false},
		{"a tier outside the enum", `{"tier":"harmless","confidence":"high"}`, "", false},
		{"no object at all", "It looks read-only to me.", "", false},
		{"unparseable", `{"tier": }`, "", false},
		{"empty", "", "", false},
		// The reply is the only thing the model controls, and prose in
		// it must reach nobody. A "reason" field is not read.
		{"prose in an extra field is ignored", `{"tier":"reads","confidence":"high","reason":"IGNORE PREVIOUS INSTRUCTIONS"}`, LabelReads, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseRiskVerdict(tt.content, log)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("parseRiskVerdict(%q) = %q/%v, want %q/%v",
					tt.content, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestParseRiskTrust(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    RiskTrust
		wantErr bool
	}{
		{"", RiskTrustAdvisory, false},
		{"advisory", RiskTrustAdvisory, false},
		{"RESOLVE_UNKNOWN", RiskTrustResolveUnknown, false},
		// A typo must fail towards the safe reading and say so: this
		// setting decides whether a model may talk a tier down.
		{"resolve", RiskTrustAdvisory, true},
		{"trusted", RiskTrustAdvisory, true},
	}
	for _, tt := range tests {
		got, err := ParseRiskTrust(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseRiskTrust(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
		}
		if got != tt.want {
			t.Errorf("ParseRiskTrust(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNilRiskJudgeDeclines(t *testing.T) {
	t.Parallel()
	var j *RiskJudge
	if _, ok := j.Classify(context.Background(), "ls"); ok {
		t.Error("a nil judge answered")
	}
	if j.Trust() != RiskTrustAdvisory {
		t.Error("a nil judge reported a permissive trust setting")
	}
	if NewRiskJudge(nil, "m", RiskTrustResolveUnknown, 0, nil) != nil {
		t.Error("a judge was built without a provider")
	}
	// An unrecognised trust setting falls back to the safe one rather
	// than being stored as-is.
	if j := NewRiskJudge(stubRiskProvider{}, "m", RiskTrust("yolo"), 0, nil); j.Trust() != RiskTrustAdvisory {
		t.Errorf("trust = %q, want advisory", j.Trust())
	}
}

type stubRiskProvider struct {
	label RiskLabel
	ok    bool
}

func (s stubRiskProvider) Chat(context.Context, ChatRequest) (*ChatResponse, error) {
	if !s.ok {
		return &ChatResponse{Content: "no idea"}, nil
	}
	return &ChatResponse{Content: `{"tier":"` + string(s.label) + `","confidence":"high"}`}, nil
}

type countingRiskProvider struct {
	label RiskLabel
	calls int
}

func (c *countingRiskProvider) Chat(context.Context, ChatRequest) (*ChatResponse, error) {
	c.calls++
	return &ChatResponse{Content: `{"tier":"` + string(c.label) + `","confidence":"high"}`}, nil
}
