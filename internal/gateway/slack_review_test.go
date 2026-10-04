package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/turn"
)

// recordingPrompts captures what a confirmation was registered WITH,
// which is the only place the chained-confirmation bug is visible: the
// prompt is created correctly-shaped except for who may answer it.
type recordingPrompts struct {
	mu      sync.Mutex
	created []NewPrompt
	prompt  *Prompt
}

func (f *recordingPrompts) Create(np NewPrompt) (*Prompt, error) {
	f.mu.Lock()
	f.created = append(f.created, np)
	f.mu.Unlock()
	p := &Prompt{ID: "p-resumed", RaisedFor: np.RaisedFor, SessionID: np.SessionID}
	f.prompt = p
	return p, nil
}
func (f *recordingPrompts) Get(string) (*Prompt, error)                       { return f.prompt, nil }
func (f *recordingPrompts) Resolve(string, PromptDecision, PromptScope) error { return nil }
func (f *recordingPrompts) Wait(context.Context, string) (PromptDecision, error) {
	return PromptApproved, nil
}

// slackAPIRecorder answers every Web API method with ok:true and counts
// what was called, so a test can assert that something did NOT happen.
type slackAPIRecorder struct {
	mu    sync.Mutex
	calls []string
	srv   *httptest.Server
}

func newSlackAPIRecorder(t *testing.T) *slackAPIRecorder {
	t.Helper()
	rec := &slackAPIRecorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.calls = append(rec.calls, r.URL.Path)
		rec.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "ts": "1700000000.000100", "channel": "C1"})
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (r *slackAPIRecorder) since(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n > len(r.calls) {
		return nil
	}
	return append([]string(nil), r.calls[n:]...)
}

func (r *slackAPIRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// Review #1. Approve tool A, the agent proceeds, tool B needs approval
// too — and that second prompt used to be raised for nobody, so
// mayResolve refused every tap including from the person it was asked
// of. The turn then had no ending but its TTL.
//
// Driven at the seam the bug lives in rather than through a real agent:
// the resumed leg builds a SessionRef from the prompt, and
// sendConfirmationBlocks stamps RaisedFor from it. Both halves are here,
// and the tap is checked against the real mayResolve.
func TestSlackChainedConfirmationIsAnswerableByTheOriginalApprover(t *testing.T) {
	t.Parallel()

	rec := newSlackAPIRecorder(t)
	prompts := &recordingPrompts{}
	h := &SlackHandler{
		cfg: SlackConfig{AllowedChannels: []string{"*"}, Prompts: prompts},
		log: discardLogger(),
		api: newSlackAPI("xoxb-test", rec.srv.URL, rec.srv.Client()),
	}

	// The first approval was given by Alice, so the resumed leg is hers.
	first := &Prompt{
		ID:        "p-first",
		SessionID: "C1/1700000000.000100",
		ChannelID: "C1",
		TurnID:    "t1",
		RaisedFor: "slack-T0-U0ALICE",
	}
	session := resumeSessionFor(first)
	if session.UserID != first.RaisedFor {
		t.Fatalf("resumed session UserID = %q; the second confirmation is raised for whoever this names", session.UserID)
	}

	// The resumed leg needs a second confirmation.
	r := &slackResponder{h: h, channel: "C1", thread: "1700000000.000100", status: "1700000000.000100"}
	h.sendConfirmationBlocks(context.Background(), r,
		turn.Request{TurnID: "t1"},
		&turn.Response{ConfirmationReason: "run the second tool"},
		session)

	prompts.mu.Lock()
	created := append([]NewPrompt(nil), prompts.created...)
	prompts.mu.Unlock()
	if len(created) != 1 {
		t.Fatalf("prompts created = %d; want 1", len(created))
	}
	if created[0].RaisedFor != "slack-T0-U0ALICE" {
		t.Fatalf("RaisedFor on the resumed leg = %q; an empty value is answerable by nobody", created[0].RaisedFor)
	}

	// And the tap actually resolves, which is the user-visible claim.
	if !h.mayResolve(context.Background(), "p-resumed", "T0", "U0ALICE", "C1", "1700000000.000100") {
		t.Error("the person who gave the first approval could not answer the second question")
	}
}

// Review #3. The interim timer runs on its own goroutine and is stopped
// by a cleanup that fires AFTER the reply is written, so a turn landing
// near the threshold could have its answer rewritten — in place, by
// Slack — with "still working on this".
func TestSlackInterimCannotOverwriteTheAnswer(t *testing.T) {
	t.Parallel()

	rec := newSlackAPIRecorder(t)
	h := &SlackHandler{
		cfg: SlackConfig{AllowedChannels: []string{"*"}},
		log: discardLogger(),
		api: newSlackAPI("xoxb-test", rec.srv.URL, rec.srv.Client()),
	}
	r := &slackResponder{h: h, channel: "C1", thread: "1.1", status: "1.1"}

	r.writeFinal(context.Background(), "the answer", nil)
	before := rec.count()

	if err := r.Interim(context.Background(), "Still working on this…"); err != nil {
		t.Fatalf("Interim: %v", err)
	}
	if got := rec.since(before); len(got) != 0 {
		t.Errorf("interim called Slack after the answer landed: %v", got)
	}
}

// The other direction: an interim BEFORE the answer must still work, or
// the latch has simply broken the progress signal it was protecting.
func TestSlackInterimStillWorksBeforeTheAnswer(t *testing.T) {
	t.Parallel()

	rec := newSlackAPIRecorder(t)
	h := &SlackHandler{
		cfg: SlackConfig{AllowedChannels: []string{"*"}},
		log: discardLogger(),
		api: newSlackAPI("xoxb-test", rec.srv.URL, rec.srv.Client()),
	}
	r := &slackResponder{h: h, channel: "C1", thread: "1.1", status: "1.1"}

	if err := r.Interim(context.Background(), "Still working on this…"); err != nil {
		t.Fatalf("Interim: %v", err)
	}
	if rec.count() == 0 {
		t.Error("an interim before the answer should reach Slack")
	}
}

// Review #2. A slash payload carries no thread_ts, so in a channel this
// handler cannot name the conversation the user is looking at. /new
// against the bare channel id deletes an empty session, reports success,
// and revokes grants under a session id the user never used.
func TestSlackRefusesSessionScopedCommandsInAChannel(t *testing.T) {
	t.Parallel()

	rec := newSlackAPIRecorder(t)
	commands := NewCommandSet(fakeAuthz{allow: true}, discardLogger())
	ran := false
	commands.Register(&Command{
		Name:          "new",
		Summary:       "forget this conversation",
		SharedSafe:    true,
		SessionScoped: true,
		Handler: func(context.Context, CommandRequest) (string, error) {
			ran = true
			return "Started a fresh conversation.", nil
		},
	})
	h := &SlackHandler{
		// A scope, because an unknown user is dropped before
		// addressability is ever considered — authorisation first is the
		// right order, and it means the refusal below is reached only by
		// somebody entitled to run the command.
		cfg: SlackConfig{
			AllowedChannels: []string{"*"},
			UserScopes:      map[string]string{"U0ALICE": "user:alice"},
		},
		log:      discardLogger(),
		api:      newSlackAPI("xoxb-test", rec.srv.URL, rec.srv.Client()),
		commands: commands,
	}

	h.handleSlashCommand(context.Background(), slackSlashCommand{
		Command: "/lobslaw", Text: "new", ChannelID: "C1", UserID: "U0ALICE", TeamID: "T0",
	})
	if ran {
		t.Error("a session-scoped command ran against a channel id it could not have meant")
	}

	// The refusal is worth checking as a refusal, not silence: the user
	// typed something and has to learn it did not happen.
	var said bool
	for _, path := range rec.since(0) {
		if path == "/chat.postEphemeral" || path == "/chat.postMessage" {
			said = true
		}
	}
	if !said {
		t.Error("the refusal was never sent to the user")
	}
}

// A DM can address its conversation, so nothing is refused there.
func TestSlackAllowsSessionScopedCommandsInADM(t *testing.T) {
	t.Parallel()

	rec := newSlackAPIRecorder(t)
	commands := NewCommandSet(fakeAuthz{allow: true}, discardLogger())
	ran := false
	commands.Register(&Command{
		Name:          "new",
		SharedSafe:    true,
		SessionScoped: true,
		Handler: func(context.Context, CommandRequest) (string, error) {
			ran = true
			return "done", nil
		},
	})
	h := &SlackHandler{
		cfg: SlackConfig{
			AllowedChannels: []string{"*"},
			UserScopes:      map[string]string{"U0ALICE": "user:alice"},
		},
		log:      discardLogger(),
		api:      newSlackAPI("xoxb-test", rec.srv.URL, rec.srv.Client()),
		commands: commands,
	}
	h.handleSlashCommand(context.Background(), slackSlashCommand{
		Command: "/lobslaw", Text: "new", ChannelID: "D0ALICE", UserID: "U0ALICE", TeamID: "T0",
	})
	if !ran {
		t.Error("a DM can name its own conversation; the command should have run")
	}
}

var _ = json.Marshal

// Review #9. A DM id is minted per user on first contact, so an
// operator cannot enumerate them — which left ["*"] as the only config
// that allowed DMs at all, and it also opens every channel the bot is
// in.
func TestSlackDMSentinelAllowsDMsWithoutOpeningChannels(t *testing.T) {
	t.Parallel()

	h := &SlackHandler{cfg: SlackConfig{AllowedChannels: []string{"dm", "C0ALLOWED"}}}

	if !h.isAllowedChannel("D0ALICE") {
		t.Error(`"dm" should match a DM from anyone, including one never seen before`)
	}
	if !h.isAllowedChannel("D0BOB") {
		t.Error(`"dm" should match every DM, not one of them`)
	}
	if !h.isAllowedChannel("C0ALLOWED") {
		t.Error("an explicitly listed channel should still match")
	}
	if h.isAllowedChannel("C0OTHER") {
		t.Error(`"dm" must not open channels — that is the whole point of not writing ["*"]`)
	}
	if h.isAllowedChannel("G0PRIVATE") {
		t.Error(`"dm" must not open private groups`)
	}
}

func TestSlackAllowedChannelsStillClosedByDefault(t *testing.T) {
	t.Parallel()
	h := &SlackHandler{cfg: SlackConfig{}}
	if h.isAllowedChannel("D0ALICE") || h.isAllowedChannel("C1") {
		t.Error("empty allowed_channels is closed, including to DMs")
	}
}

// Review #7's second half. Two files sharing a name in one turn is
// ordinary — "screenshot.png" twice — and the second used to overwrite
// the first, so the agent read one image while being told about two.
func TestSlackFilePrefixDisambiguatesSameNamedFiles(t *testing.T) {
	t.Parallel()

	a := slackFilePrefix("https://files.slack.com/files-pri/T024BE7LD-F024BERPE/screenshot.png")
	b := slackFilePrefix("https://files.slack.com/files-pri/T024BE7LD-F07XYZ123/screenshot.png")
	if a == "" || b == "" {
		t.Fatalf("no prefix derived: %q %q", a, b)
	}
	if a == b {
		t.Errorf("both files got the same prefix %q; they would still collide", a)
	}

	// A filename that happens to look like an id must not donate one:
	// the prefix has to identify the FILE, not the last path segment.
	if got := slackFilePrefix("https://example.com/FOO-bar.png"); got != "" {
		t.Errorf("filename contributed a prefix: %q", got)
	}
	if got := slackFilePrefix("not-a-url"); got != "" {
		t.Errorf("prefix from a reference with no file id: %q", got)
	}
}
