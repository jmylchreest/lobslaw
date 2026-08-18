package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Approving an operator enrolment from a chat. The request arrives
// from a laptop with no credential, so nothing about it is trusted;
// what makes a tap safe to act on is that the prompt records who it
// was asked of, and that the fingerprint in the message is the thing a
// human compares.

// recordingDecider captures what the gateway asked the node to do.
type recordingDecider struct {
	calls    int
	id       string
	approved bool
	by       string
	err      error
}

func (d *recordingDecider) Decide(_ context.Context, id string, approve bool, by string) error {
	d.calls++
	d.id, d.approved, d.by = id, approve, by
	return d.err
}

func enrolHarness(t *testing.T, dec EnrolmentDecider) *tgPromptHarness {
	t.Helper()
	return newTGPromptHarness(t, newAgentFor(t), TelegramConfig{
		UnknownUserScope: "public",
		UserIDScopes:     map[int64]string{1: "owner"},
		Enrolments:       dec,
	})
}

func demoRequest() EnrolmentRequest {
	return EnrolmentRequest{
		ID: "enr-1", RequestedName: "alice",
		Fingerprint: "SHA256:aa:bb:cc",
		ExpiresAt:   time.Now().Add(time.Hour),
	}
}

// --- raising the question ----------------------------------------------

// The fingerprint has to survive being read aloud down a phone, so it
// goes in the message unadorned.
func TestTheQuestionCarriesTheFingerprint(t *testing.T) {
	t.Parallel()
	h := enrolHarness(t, &recordingDecider{})

	if _, err := h.handler.AskEnrolment(context.Background(), demoRequest()); err != nil {
		t.Fatal(err)
	}
	if !sentTextContaining(h, "SHA256:aa:bb:cc") {
		t.Error("the question does not show the fingerprint")
	}
	if !sentTextContaining(h, "alice") {
		t.Error("the question does not say who is asking")
	}
	if !sentTextContaining(h, "administer this cluster") {
		t.Error("the question does not say what is being granted")
	}
}

// An always-grant for issuing operator credentials would be a standing
// authority to admit anyone who asks.
func TestTheEnrolmentKeyboardHasNoAlwaysButton(t *testing.T) {
	t.Parallel()
	h := enrolHarness(t, &recordingDecider{})

	if _, err := h.handler.AskEnrolment(context.Background(), demoRequest()); err != nil {
		t.Fatal(err)
	}
	for _, c := range h.capturedCalls() {
		if strings.Contains(mustJSON(t, c.Body), "approve-always") {
			t.Fatal("the enrolment keyboard offers an always-grant")
		}
	}
}

// The prompt is raised FOR the owner, which is what the audience check
// then enforces. Without it anyone in the chat could tap.
func TestTheQuestionIsRaisedForTheOwner(t *testing.T) {
	t.Parallel()
	h := enrolHarness(t, &recordingDecider{})

	id, err := h.handler.AskEnrolment(context.Background(), demoRequest())
	if err != nil {
		t.Fatal(err)
	}
	p, err := h.registry.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if p.RaisedFor == "" {
		t.Fatal("the enrolment prompt has no audience; anyone could answer it")
	}
	if p.Enrolment != "enr-1" {
		t.Errorf("prompt is not linked to the request: %q", p.Enrolment)
	}
}

// With nobody mapped to the owner scope there is no one to ask, and
// saying so beats sending the question to whoever happens to be first
// in a map.
func TestWithNoOwnerNobodyIsAsked(t *testing.T) {
	t.Parallel()
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{
		UnknownUserScope: "public",
		UserIDScopes:     map[int64]string{1: "public", 2: "public"},
		Enrolments:       &recordingDecider{},
	})

	if _, err := h.handler.AskEnrolment(context.Background(), demoRequest()); err == nil {
		t.Fatal("a question was raised with no owner configured")
	}
}

// --- answering it ------------------------------------------------------

func TestApprovingIssuesTheCredential(t *testing.T) {
	t.Parallel()
	dec := &recordingDecider{}
	h := enrolHarness(t, dec)

	id, err := h.handler.AskEnrolment(context.Background(), demoRequest())
	if err != nil {
		t.Fatal(err)
	}
	tapPrompt(t, h, id, `{"id": 1, "username": "owner"}`)

	if dec.calls != 1 {
		t.Fatalf("decider called %d times", dec.calls)
	}
	if dec.id != "enr-1" || !dec.approved {
		t.Errorf("decided %q approve=%v", dec.id, dec.approved)
	}
	if dec.by == "" {
		t.Error("the decision names nobody; the credential is unattributable")
	}
}

// A refusal has to actually close the request, or the laptop keeps
// polling a question somebody already said no to.
func TestDenyingClosesTheRequest(t *testing.T) {
	t.Parallel()
	dec := &recordingDecider{}
	h := enrolHarness(t, dec)

	id, err := h.handler.AskEnrolment(context.Background(), demoRequest())
	if err != nil {
		t.Fatal(err)
	}
	denyPrompt(t, h, id, `{"id": 1, "username": "owner"}`)

	if dec.calls != 1 {
		t.Fatalf("a denial did not reach the node (%d calls)", dec.calls)
	}
	if dec.approved {
		t.Error("a denial was passed on as an approval")
	}
}

// THE GUARANTEE THIS INHERITS. Somebody else in the chat cannot admit
// an operator.
func TestABystanderCannotApproveAnEnrolment(t *testing.T) {
	t.Parallel()
	dec := &recordingDecider{}
	h := enrolHarness(t, dec)

	id, err := h.handler.AskEnrolment(context.Background(), demoRequest())
	if err != nil {
		t.Fatal(err)
	}
	tapPrompt(t, h, id, `{"id": 99, "username": "mallory"}`)

	if dec.calls != 0 {
		t.Fatal("a bystander's tap issued an operator credential")
	}
}

// They tapped Approve and are entitled to know the certificate was not
// issued — otherwise they tell somebody to collect one that does not
// exist.
func TestAFailedIssueIsReportedToTheApprover(t *testing.T) {
	t.Parallel()
	dec := &recordingDecider{err: errors.New("CA unavailable")}
	h := enrolHarness(t, dec)

	id, err := h.handler.AskEnrolment(context.Background(), demoRequest())
	if err != nil {
		t.Fatal(err)
	}
	tapPrompt(t, h, id, `{"id": 1, "username": "owner"}`)

	if !sentTextContaining(h, "could not be applied") {
		t.Error("a failed issue was not reported to the person who approved it")
	}
}

// With no decider wired the channel must not pretend it did something.
func TestWithNoDeciderNothingIsClaimed(t *testing.T) {
	t.Parallel()
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{
		UnknownUserScope: "public",
		UserIDScopes:     map[int64]string{1: "owner"},
	})

	id, err := h.handler.AskEnrolment(context.Background(), demoRequest())
	if err != nil {
		t.Fatal(err)
	}
	tapPrompt(t, h, id, `{"id": 1, "username": "owner"}`)

	if sentTextContaining(h, "credential issued") {
		t.Error("the channel claimed to issue a credential with no decider wired")
	}
}

// denyPrompt fires a deny callback from a given sender.
func denyPrompt(t *testing.T, h *tgPromptHarness, promptID, from string) {
	t.Helper()
	update := `{
		"update_id": 901,
		"callback_query": {
			"id": "cb-deny",
			"from": ` + from + `,
			"message": {"message_id": 2, "chat": {"id": 1, "type": "private"}, "date": 0},
			"data": "prompt:deny:` + promptID + `"
		}
	}`
	postUpdate(t, h.handler, "test-webhook-secret", update)
}

// mustJSON renders a captured call body for substring checks.
func mustJSON(t *testing.T, body map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
