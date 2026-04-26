package compute

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/soul"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// stubSoulMutator implements SoulMutator for tests without touching
// disk. Tracks calls so assertions can verify the builtin dispatched
// correctly.
type stubSoulMutator struct {
	soul         soul.Soul
	setNameCalls []string
	tuneCalls    []struct {
		dim   string
		delta int
	}
	addedFragments   []string
	removedFragments []string
	rollbackSteps    int
	tuneErr          error
	addErr           error
}

func (s *stubSoulMutator) Soul() soul.Soul { return s.soul }
func (s *stubSoulMutator) SetName(name string) (string, error) {
	s.setNameCalls = append(s.setNameCalls, name)
	s.soul.Config.Name = name
	return name, nil
}
func (s *stubSoulMutator) Tune(dim string, delta int) (int, int, error) {
	s.tuneCalls = append(s.tuneCalls, struct {
		dim   string
		delta int
	}{dim, delta})
	if s.tuneErr != nil {
		return 5, 5, s.tuneErr
	}
	return 5, 5 + delta, nil
}
func (s *stubSoulMutator) SetEmojiUsage(value string) error {
	s.soul.Config.EmotiveStyle.EmojiUsage = value
	return nil
}
func (s *stubSoulMutator) AddFragment(text string) (string, int, error) {
	if s.addErr != nil {
		return "", 0, s.addErr
	}
	s.addedFragments = append(s.addedFragments, text)
	s.soul.Config.Fragments = append(s.soul.Config.Fragments, text)
	return text, len(s.soul.Config.Fragments), nil
}
func (s *stubSoulMutator) RemoveFragment(needle string) (string, error) {
	s.removedFragments = append(s.removedFragments, needle)
	return needle, nil
}
func (s *stubSoulMutator) ListFragments() []string { return s.soul.Config.Fragments }
func (s *stubSoulMutator) HistoryRollback(steps int) (string, error) {
	s.rollbackSteps = steps
	return "20260101T120000.000", nil
}

func newSoulBuiltins(t *testing.T, mut SoulMutator) *Builtins {
	t.Helper()
	b := NewBuiltins()
	if err := RegisterSoulBuiltins(b, SoulBuiltinsConfig{Mutator: mut}); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSoulGetReturnsCurrentState(t *testing.T) {
	t.Parallel()
	mut := &stubSoulMutator{
		soul: soul.Soul{
			Config: types.SoulConfig{
				Name: "bot", PersonaDescription: "test",
				EmotiveStyle: types.EmotiveStyle{Sarcasm: 7},
				Fragments:    []string{"a", "b"},
			},
		},
	}
	b := newSoulBuiltins(t, mut)
	fn, _ := b.Get("soul_get")
	out, code, err := fn(context.Background(), nil)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	var resp map[string]any
	_ = json.Unmarshal(out, &resp)
	if resp["name"] != "bot" {
		t.Errorf("name = %v", resp["name"])
	}
	frags, _ := resp["fragments"].([]any)
	if len(frags) != 2 {
		t.Errorf("fragments = %v", frags)
	}
}

func TestSoulTuneNameDispatches(t *testing.T) {
	t.Parallel()
	mut := &stubSoulMutator{}
	b := newSoulBuiltins(t, mut)
	fn, _ := b.Get("soul_tune")
	out, code, err := fn(context.Background(), map[string]string{
		"field": "name",
		"value": "Lobs",
	})
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if len(mut.setNameCalls) != 1 || mut.setNameCalls[0] != "Lobs" {
		t.Errorf("SetName not called as expected: %v", mut.setNameCalls)
	}
	var resp map[string]any
	_ = json.Unmarshal(out, &resp)
	if resp["value"] != "Lobs" {
		t.Errorf("response value = %v", resp["value"])
	}
}

func TestSoulTuneSarcasmDelta(t *testing.T) {
	t.Parallel()
	mut := &stubSoulMutator{}
	b := newSoulBuiltins(t, mut)
	fn, _ := b.Get("soul_tune")
	if _, code, err := fn(context.Background(), map[string]string{"field": "sarcasm", "delta": "1"}); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if len(mut.tuneCalls) != 1 || mut.tuneCalls[0].dim != "sarcasm" || mut.tuneCalls[0].delta != 1 {
		t.Errorf("Tune not dispatched correctly: %v", mut.tuneCalls)
	}
}

func TestSoulTuneCapReachedReturnsAppliedFalse(t *testing.T) {
	t.Parallel()
	mut := &stubSoulMutator{tuneErr: errors.New("already at cap")}
	b := newSoulBuiltins(t, mut)
	fn, _ := b.Get("soul_tune")
	out, code, err := fn(context.Background(), map[string]string{"field": "sarcasm", "delta": "1"})
	if err != nil || code != 0 {
		t.Fatalf("cap-reached should NOT be a hard error; code=%d err=%v", code, err)
	}
	var resp map[string]any
	_ = json.Unmarshal(out, &resp)
	if applied, _ := resp["applied"].(bool); applied {
		t.Errorf("applied should be false: %v", resp)
	}
	if reason, _ := resp["reason"].(string); reason == "" {
		t.Error("reason should be populated on no-op")
	}
}

func TestSoulTuneRejectsUnknownField(t *testing.T) {
	t.Parallel()
	mut := &stubSoulMutator{}
	b := newSoulBuiltins(t, mut)
	fn, _ := b.Get("soul_tune")
	if _, code, err := fn(context.Background(), map[string]string{"field": "arrogance"}); err == nil || code != 2 {
		t.Errorf("expected user-fixable error for unknown field; code=%d err=%v", code, err)
	}
}

func TestSoulFragmentAdd(t *testing.T) {
	t.Parallel()
	mut := &stubSoulMutator{}
	b := newSoulBuiltins(t, mut)
	fn, _ := b.Get("soul_fragment_add")
	if _, code, err := fn(context.Background(), map[string]string{"text": "supports Liverpool"}); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if len(mut.addedFragments) != 1 {
		t.Errorf("AddFragment not called: %v", mut.addedFragments)
	}
}

func TestSoulHistoryRollbackDefaultStep(t *testing.T) {
	t.Parallel()
	mut := &stubSoulMutator{}
	b := newSoulBuiltins(t, mut)
	fn, _ := b.Get("soul_history_rollback")
	if _, code, err := fn(context.Background(), nil); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if mut.rollbackSteps != 1 {
		t.Errorf("default steps should be 1; got %d", mut.rollbackSteps)
	}
}

func TestSoulHistoryRollbackExplicitSteps(t *testing.T) {
	t.Parallel()
	mut := &stubSoulMutator{}
	b := newSoulBuiltins(t, mut)
	fn, _ := b.Get("soul_history_rollback")
	if _, code, err := fn(context.Background(), map[string]string{"steps": "3"}); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if mut.rollbackSteps != 3 {
		t.Errorf("steps not propagated: got %d", mut.rollbackSteps)
	}
}
