package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// twoUserBrowser is one node's store holding two people's private
// threads plus a Telegram group chat that alice opened and bob also
// speaks in.
func twoUserBrowser() *fakeBrowser {
	alice := SessionBrowseInfo{
		Channel: "telegram", ChannelID: "100", UserID: "tg-@alice",
		Title: "Alice's mortgage paperwork", Messages: 12, UpdatedAt: "2026-08-12 10:00 UTC",
	}
	bob := SessionBrowseInfo{
		Channel: "telegram", ChannelID: "200", UserID: "tg-@bob",
		Title: "Bob's divorce", Messages: 9, UpdatedAt: "2026-08-13 10:00 UTC",
	}
	group := SessionBrowseInfo{
		Channel: "telegram", ChannelID: "-300", UserID: "tg-@alice",
		Title: "Weekend plans", Messages: 30, UpdatedAt: "2026-08-14 10:00 UTC",
	}
	return &fakeBrowser{
		infos: []SessionBrowseInfo{group, bob, alice},
		hits: []SessionBrowseHit{
			{Info: bob, Matches: 1, Snippets: []SessionBrowseSnippet{
				{Seq: 3, Role: "user", Text: "the settlement figure is 40000"}}},
			{Info: alice, Matches: 1, Snippets: []SessionBrowseSnippet{
				{Seq: 5, Role: "user", Text: "the deposit is 25000"}}},
			{Info: group, Matches: 1, Snippets: []SessionBrowseSnippet{
				{Seq: 8, Role: "user", Text: "the pub at 7 then"}}},
		},
		perSession: map[turn.SessionKey][]compute.Message{
			{Channel: "telegram", ChannelID: "100"}:  {{Role: "user", Content: "the deposit is 25000"}},
			{Channel: "telegram", ChannelID: "200"}:  {{Role: "user", Content: "the settlement figure is 40000"}},
			{Channel: "telegram", ChannelID: "-300"}: {{Role: "user", Content: "the pub at 7 then"}},
		},
	}
}

// A shared bot is the whole point of Telegram's UserIDScopes: bob
// asking the agent to search "our past conversations" must not be
// handed alice's.
func TestSessionSearchHidesOtherUsersConversations(t *testing.T) {
	t.Parallel()
	b := newTestSessionTools(t, twoUserBrowser(), SessionToolConfig{})

	bobs := callToolCtx(scopedCtx("tg-@bob", "telegram", "200"), t, b,
		"session_search", map[string]string{"query": "the"})
	if strings.Contains(bobs, "mortgage") || strings.Contains(bobs, "25000") {
		t.Errorf("bob was shown alice's thread:\n%s", bobs)
	}
	if !strings.Contains(bobs, "40000") {
		t.Errorf("bob lost his own thread:\n%s", bobs)
	}

	alices := callToolCtx(scopedCtx("tg-@alice", "telegram", "100"), t, b,
		"session_search", map[string]string{"query": "the"})
	if strings.Contains(alices, "divorce") || strings.Contains(alices, "40000") {
		t.Errorf("alice was shown bob's thread:\n%s", alices)
	}
	if !strings.Contains(alices, "25000") {
		t.Errorf("alice lost her own thread:\n%s", alices)
	}
}

// session_list is the other half of the leak: even without snippets,
// handing out titles and addresses is both a disclosure in itself and
// the input session_read needs.
func TestSessionListHidesOtherUsersConversations(t *testing.T) {
	t.Parallel()
	b := newTestSessionTools(t, twoUserBrowser(), SessionToolConfig{})

	out := callToolCtx(scopedCtx("tg-@bob", "telegram", "200"), t, b, "session_list", nil)
	if strings.Contains(out, "mortgage") {
		t.Errorf("alice's conversation listed to bob:\n%s", out)
	}
	if strings.Contains(out, "Weekend plans") {
		t.Errorf("a group bob isn't currently in was listed to him:\n%s", out)
	}
	if !strings.Contains(out, "Bob's divorce") {
		t.Errorf("bob's own conversation missing:\n%s", out)
	}
}

func TestSessionReadRefusesAnotherUsersConversation(t *testing.T) {
	t.Parallel()
	browser := twoUserBrowser()
	b := newTestSessionTools(t, browser, SessionToolConfig{})

	out, code, err := invokeToolCtx(scopedCtx("tg-@bob", "telegram", "200"), t, b,
		"session_read", map[string]string{"channel": "telegram", "channel_id": "100"})
	if err == nil {
		t.Fatalf("bob read alice's transcript: %s", out)
	}
	if code == 0 {
		t.Error("refusal reported exit 0")
	}
	// The check has to happen before the load, not filter after it —
	// alice's transcript should never have left the store.
	for _, k := range browser.reads {
		if k.ChannelID == "100" {
			t.Errorf("alice's transcript was loaded before the check: %+v", browser.reads)
		}
	}
	// A refused read and a conversation that doesn't exist say the
	// same thing, so the tool can't be used to probe for chat ids.
	_, _, missing := invokeToolCtx(scopedCtx("tg-@bob", "telegram", "200"), t, b,
		"session_read", map[string]string{"channel": "telegram", "channel_id": "999"})
	if missing == nil || missing.Error() != err.Error() {
		t.Errorf("absent conversation said %v, refused one said %v — that's an oracle", missing, err)
	}
}

// Telegram shares one session across a group: the record's owner is
// whoever spoke first, so a second member speaking must still reach
// the conversation they can already scroll up through.
func TestSessionReadAllowsAnotherMemberOfTheCurrentChat(t *testing.T) {
	t.Parallel()
	b := newTestSessionTools(t, twoUserBrowser(), SessionToolConfig{})

	out := callToolCtx(scopedCtx("tg-@bob", "telegram", "-300"), t, b,
		"session_read", map[string]string{"channel": "telegram", "channel_id": "-300"})
	if !strings.Contains(out, "the pub at 7 then") {
		t.Errorf("a group member was refused the conversation he's in:\n%s", out)
	}

	found := callToolCtx(scopedCtx("tg-@bob", "telegram", "-300"), t, b,
		"session_search", map[string]string{"query": "pub"})
	if !strings.Contains(found, "Weekend plans") {
		t.Errorf("search hid the group conversation from a member:\n%s", found)
	}
	// Being in alice's group doesn't reach alice's DMs.
	if strings.Contains(found, "mortgage") {
		t.Errorf("group membership widened bob's scope to alice's DMs:\n%s", found)
	}
}

// The operator path: `lobslaw session` and the compactor drive the
// store with no turn identity and must keep seeing everything.
func TestSessionToolsUnscopedContextSeesEverything(t *testing.T) {
	t.Parallel()
	b := newTestSessionTools(t, twoUserBrowser(), SessionToolConfig{})

	list := callTool(t, b, "session_list", nil)
	for _, want := range []string{"mortgage", "divorce", "Weekend plans"} {
		if !strings.Contains(list, want) {
			t.Errorf("unscoped list dropped %q:\n%s", want, list)
		}
	}
	read := callTool(t, b, "session_read",
		map[string]string{"channel": "telegram", "channel_id": "200"})
	if !strings.Contains(read, "40000") {
		t.Errorf("unscoped read refused:\n%s", read)
	}
}

// The tools ask the browser to filter because the result limit depends
// on it, but they don't trust it to have done so — a browser that
// ignores the predicate must degrade to empty results, not back to the
// original leak.
func TestSessionToolsRecheckBrowserThatIgnoresVisibility(t *testing.T) {
	t.Parallel()
	browser := twoUserBrowser()
	browser.ignoreVisibility = true
	b := newTestSessionTools(t, browser, SessionToolConfig{})

	ctx := scopedCtx("tg-@bob", "telegram", "200")
	if out := callToolCtx(ctx, t, b, "session_list", nil); strings.Contains(out, "mortgage") {
		t.Errorf("a sloppy browser leaked through session_list:\n%s", out)
	}
	if out := callToolCtx(ctx, t, b, "session_search", map[string]string{"query": "the"}); strings.Contains(out, "25000") {
		t.Errorf("a sloppy browser leaked through session_search:\n%s", out)
	}
}

// An unauthenticated turn is scoped, not unscoped: it owns the
// conversation it is in and nothing attributed to a named user.
func TestTurnIdentityAnonymousTurnIsStillScoped(t *testing.T) {
	t.Parallel()
	b := newTestSessionTools(t, twoUserBrowser(), SessionToolConfig{})
	ctx := turn.WithIdentity(context.Background(), (&compute.Agent{}).TurnIdentityFor(compute.ProcessMessageRequest{
		Channel:   "telegram",
		ChannelID: "-300",
	}))
	out := callToolCtx(ctx, t, b, "session_list", nil)
	if strings.Contains(out, "divorce") || strings.Contains(out, "mortgage") {
		t.Errorf("an unauthenticated turn saw named users' threads:\n%s", out)
	}
	if !strings.Contains(out, "Weekend plans") {
		t.Errorf("the current conversation was hidden from its own turn:\n%s", out)
	}
}

// wireSessionToolsIntoAgent puts the session builtins behind the real
// executor so a test can drive them the way a turn does — through the
// agent loop, which is the thing that has to attach the scope.
func wireSessionToolsIntoAgent(t *testing.T, env *agentEnv, browser *fakeBrowser) {
	t.Helper()
	b := NewBuiltins()
	if err := RegisterSessionBuiltins(b, SessionToolConfig{Browser: browser}); err != nil {
		t.Fatal(err)
	}
	env.executor.SetBuiltins(b)
	for _, td := range SessionToolDefs() {
		if err := env.reg.Register(td); err != nil {
			t.Fatal(err)
		}
	}
}

// runSessionToolTurn drives one turn that calls exactly one session
// tool, and returns what the tool handed back to the model.
func runSessionToolTurn(t *testing.T, browser *fakeBrowser, claims *types.Claims, channel, chatID, tool, args string) compute.ToolInvocation {
	t.Helper()
	env := newAgentEnv(t,
		compute.MockResponse{ToolCalls: []compute.ToolCall{{ID: "call-1", Name: tool, Arguments: args}}},
		compute.MockResponse{Content: "ok"},
	)
	wireSessionToolsIntoAgent(t, env, browser)

	resp, err := env.agent.RunToolCallLoop(context.Background(), compute.ProcessMessageRequest{
		Message:      "what did we talk about?",
		Claims:       claims,
		TurnID:       "turn-" + claims.UserID,
		SystemPrompt: "test",
		Channel:      channel,
		ChannelID:    chatID,
		Budget:       mkBudget(t, compute.BudgetCaps{}),
	})
	if err != nil {
		t.Fatalf("RunToolCallLoop: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(resp.ToolCalls))
	}
	return resp.ToolCalls[0]
}

// The end-to-end case this whole change exists for: two people using
// one bot, driving real turns through the agent loop, neither able to
// reach the other's transcripts through any of the three tools.
func TestTwoUsersDrivingTurnsCannotReachEachOther(t *testing.T) {
	t.Parallel()
	browser := twoUserBrowser()
	alice := &types.Claims{UserID: "tg-@alice", Scope: "default"}
	bob := &types.Claims{UserID: "tg-@bob", Scope: "default"}

	bobSearch := runSessionToolTurn(t, browser, bob, "telegram", "200",
		"session_search", `{"query":"the"}`).Output
	if strings.Contains(bobSearch, "mortgage") || strings.Contains(bobSearch, "25000") {
		t.Errorf("session_search leaked alice's thread to bob:\n%s", bobSearch)
	}
	if !strings.Contains(bobSearch, "40000") {
		t.Errorf("bob's own thread went missing:\n%s", bobSearch)
	}

	bobList := runSessionToolTurn(t, browser, bob, "telegram", "200",
		"session_list", `{}`).Output
	if strings.Contains(bobList, "mortgage") {
		t.Errorf("session_list leaked alice's thread to bob:\n%s", bobList)
	}

	// Addresses are guessable — Telegram chat ids are small integers —
	// so the read gate has to hold even when the model names one
	// directly rather than getting it from list or search.
	bobRead := runSessionToolTurn(t, browser, bob, "telegram", "200",
		"session_read", `{"channel":"telegram","channel_id":"100"}`)
	if strings.Contains(bobRead.Output, "25000") {
		t.Errorf("session_read handed bob alice's transcript:\n%s", bobRead.Output)
	}
	if bobRead.Error == "" && bobRead.ExitCode == 0 {
		t.Errorf("session_read on another user's thread succeeded: %+v", bobRead)
	}

	// Symmetric: alice can't reach bob either.
	aliceSearch := runSessionToolTurn(t, browser, alice, "telegram", "100",
		"session_search", `{"query":"the"}`).Output
	if strings.Contains(aliceSearch, "divorce") || strings.Contains(aliceSearch, "40000") {
		t.Errorf("session_search leaked bob's thread to alice:\n%s", aliceSearch)
	}
}

// A scheduler or research turn has claims but no chat. Ownership is
// all it gets — and all it needs, since those claims are the person
// the work is being done for.
func TestTurnIdentityChannellessTurnFallsBackToOwnership(t *testing.T) {
	t.Parallel()
	b := newTestSessionTools(t, twoUserBrowser(), SessionToolConfig{})
	ctx := turn.WithIdentity(context.Background(), turn.Identity{UserID: "tg-@bob"})

	out := callToolCtx(ctx, t, b, "session_list", nil)
	if !strings.Contains(out, "Bob's divorce") {
		t.Errorf("a scheduled task lost its owner's conversation:\n%s", out)
	}
	if strings.Contains(out, "mortgage") || strings.Contains(out, "Weekend plans") {
		t.Errorf("a channelless turn matched sessions it doesn't own:\n%s", out)
	}
}
