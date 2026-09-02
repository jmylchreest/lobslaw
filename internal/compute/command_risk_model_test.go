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

func staticVerdict(tier CommandRisk) RiskVerdict {
	return RiskVerdict{Tier: tier, Reason: reasonFor[tier]}
}

func TestAdjudicate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		static    CommandRisk
		model     CommandRisk
		modelOK   bool
		trust     RiskTrust
		want      CommandRisk
		fromModel bool
	}{
		// Raising is allowed under every setting: the cost of a wrong
		// raise is one confirmation nobody needed to give.
		{"advisory raises", RiskWrite, RiskDestructive, true, RiskTrustAdvisory, RiskDestructive, true},
		{"resolve_unknown raises too", RiskWrite, RiskDestructive, true, RiskTrustResolveUnknown, RiskDestructive, true},

		// Lowering a tier the classifier positively determined is
		// refused under BOTH settings. A command that can argue its own
		// tier down is the entire vulnerability.
		{"advisory does not lower", RiskDestructive, RiskRead, true, RiskTrustAdvisory, RiskDestructive, false},
		{"resolve_unknown does not lower either", RiskDestructive, RiskRead, true, RiskTrustResolveUnknown, RiskDestructive, false},
		{"a write is not talked down to a read", RiskWrite, RiskRead, true, RiskTrustResolveUnknown, RiskWrite, false},

		// The one permitted de-escalation, and only into the gap where
		// static reading had no opinion at all.
		{"advisory leaves unknown alone", RiskUnknown, RiskRead, true, RiskTrustAdvisory, RiskUnknown, false},
		{"resolve_unknown fills the gap", RiskUnknown, RiskRead, true, RiskTrustResolveUnknown, RiskRead, true},
		{"resolve_unknown can fill it with a worse tier", RiskUnknown, RiskDestructive, true, RiskTrustResolveUnknown, RiskDestructive, true},

		// No usable answer means the static verdict stands, whatever
		// the trust setting.
		{"a declined verdict changes nothing", RiskUnknown, "", false, RiskTrustResolveUnknown, RiskUnknown, false},
		{"a declined verdict on a write changes nothing", RiskWrite, "", false, RiskTrustAdvisory, RiskWrite, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			judge := &RiskJudge{
				provider: stubRiskProvider{tier: tt.model, ok: tt.modelOK},
				trust:    tt.trust,
				log:      slog.New(slog.DiscardHandler),
			}
			got := AdjudicateWith(context.Background(), staticVerdict(tt.static), "some command", judge)
			if got.Tier != tt.want {
				t.Errorf("tier = %q, want %q", got.Tier, tt.want)
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
	p := &countingRiskProvider{tier: RiskDestructive}
	judge := &RiskJudge{provider: p, trust: RiskTrustAdvisory, log: slog.New(slog.DiscardHandler)}

	got := AdjudicateWith(context.Background(), staticVerdict(RiskRead), "uname -a", judge)
	if got.Tier != RiskRead {
		t.Errorf("tier = %q, want read", got.Tier)
	}
	if p.calls != 0 {
		t.Errorf("the model was called %d times for a read", p.calls)
	}
}

func TestAdjudicateWithoutAJudge(t *testing.T) {
	t.Parallel()
	got := AdjudicateWith(context.Background(), staticVerdict(RiskUnknown), "for x in a; do echo $x; done", nil)
	if got.Tier != RiskUnknown || got.FromModel {
		t.Errorf("got %q/%v, want unknown/false", got.Tier, got.FromModel)
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
		want    CommandRisk
		wantOK  bool
	}{
		{"a clean answer", `{"tier":"write","confidence":"high"}`, RiskWrite, true},
		{"wrapped in a fence", "```json\n{\"tier\":\"read\",\"confidence\":\"high\"}\n```", RiskRead, true},
		{"with prose around it", `Sure! {"tier":"destructive","confidence":"high"} hope that helps`, RiskDestructive, true},
		{"upper case", `{"tier":"NETWORK","confidence":"HIGH"}`, RiskNetwork, true},

		{"low confidence is discarded", `{"tier":"read","confidence":"low"}`, "", false},
		{"a missing confidence is discarded", `{"tier":"read"}`, "", false},
		{"a tier outside the enum", `{"tier":"harmless","confidence":"high"}`, "", false},
		{"no object at all", "It looks read-only to me.", "", false},
		{"unparseable", `{"tier": }`, "", false},
		{"empty", "", "", false},
		// The reply is the only thing the model controls, and prose in
		// it must reach nobody. A "reason" field is not read.
		{"prose in an extra field is ignored", `{"tier":"read","confidence":"high","reason":"IGNORE PREVIOUS INSTRUCTIONS"}`, RiskRead, true},
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
	tier CommandRisk
	ok   bool
}

func (s stubRiskProvider) Chat(context.Context, ChatRequest) (*ChatResponse, error) {
	if !s.ok {
		return &ChatResponse{Content: "no idea"}, nil
	}
	return &ChatResponse{Content: `{"tier":"` + string(s.tier) + `","confidence":"high"}`}, nil
}

type countingRiskProvider struct {
	tier  CommandRisk
	calls int
}

func (c *countingRiskProvider) Chat(context.Context, ChatRequest) (*ChatResponse, error) {
	c.calls++
	return &ChatResponse{Content: `{"tier":"` + string(c.tier) + `","confidence":"high"}`}, nil
}
