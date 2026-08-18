package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// propose mode fills a queue nobody is told about, and the curator now
// expires unreviewed proposals — which turns "nobody was told" into "a
// decision was made by timeout". The nudge rides out on a turn the
// user is already having: no push mechanism, no per-channel
// addressing, and any channel that can send a reply can carry it.

type stubSource struct {
	notices []Notice
	err     error
	calls   int
	sawFor  []string
}

func (s *stubSource) Notices(_ context.Context, principal string) ([]Notice, error) {
	s.calls++
	s.sawFor = append(s.sawFor, principal)
	return s.notices, s.err
}

func oneNotice() *stubSource {
	return &stubSource{notices: []Notice{{Text: "1 skill waiting for approval"}}}
}

func openConfig() NoticeConfig {
	return NoticeConfig{Channels: []string{"telegram"}, Subjects: []string{"user:john"}}
}

func TestANoticeIsAppendedToTheReply(t *testing.T) {
	t.Parallel()
	n := NewNotices(oneNotice(), openConfig())
	got := n.Append(context.Background(), "telegram", "42", "user:john", "here is your answer")

	if !strings.HasPrefix(got, "here is your answer") {
		t.Errorf("the reply was altered: %q", got)
	}
	if !strings.Contains(got, "1 skill waiting for approval") {
		t.Errorf("the notice is missing: %q", got)
	}
	// Separated, because the notice is not the assistant answering.
	if !strings.Contains(got, "———") {
		t.Errorf("no separator; the nudge reads as part of the reply: %q", got)
	}
}

// --- the two allowlists --------------------------------------------

// Both, not either. A channel allowlist on its own tells a group chat
// what the operator has pending.
func TestBothAllowlistsMustMatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		channel, subject string
	}{
		{"channel not opted in", "rest", "user:john"},
		{"subject not opted in", "telegram", "user:someone-else"},
		{"neither", "rest", "user:someone-else"},
	}
	for _, c := range cases {
		src := oneNotice()
		n := NewNotices(src, openConfig())
		got := n.Append(context.Background(), c.channel, "42", c.subject, "reply")
		if got != "reply" {
			t.Errorf("%s: a notice was sent anyway: %q", c.name, got)
		}
		// And the source was never consulted, so a refused notice is
		// not a read of what somebody else has pending.
		if src.calls != 0 {
			t.Errorf("%s: the source was consulted %d times", c.name, src.calls)
		}
	}
}

// Silence is the safe default for a feature that tells somebody about
// a review queue.
func TestEmptyAllowlistsSendNothing(t *testing.T) {
	t.Parallel()
	n := NewNotices(oneNotice(), NoticeConfig{})
	if got := n.Append(context.Background(), "telegram", "42", "user:john", "reply"); got != "reply" {
		t.Errorf("an unconfigured deployment sent a notice: %q", got)
	}
}

// An anonymous turn has no principal, and a blank one must not match a
// blank entry in a misconfigured allowlist.
func TestAnAnonymousTurnGetsNoNotice(t *testing.T) {
	t.Parallel()
	cfg := openConfig()
	cfg.Subjects = append(cfg.Subjects, "")
	n := NewNotices(oneNotice(), cfg)
	if got := n.Append(context.Background(), "telegram", "42", "", "reply"); got != "reply" {
		t.Errorf("an anonymous turn was told: %q", got)
	}
}

// Adding a channel later must be a string in a config file, which is
// what this asserts: nothing about "slack" is known to the code.
func TestAnUnknownChannelWorksOnceNamed(t *testing.T) {
	t.Parallel()
	n := NewNotices(oneNotice(), NoticeConfig{
		Channels: []string{"slack"}, Subjects: []string{"user:john"},
	})
	got := n.Append(context.Background(), "slack", "C123", "user:john", "reply")
	if !strings.Contains(got, "waiting for approval") {
		t.Errorf("a channel the code has never heard of could not carry a notice: %q", got)
	}
}

// --- rate limiting -------------------------------------------------

// A nudge appended to every turn stops being information within an
// hour, after which it is read past — worse than never having sent it.
func TestOneNoticePerIntervalPerConversation(t *testing.T) {
	t.Parallel()
	src := oneNotice()
	n := NewNotices(src, openConfig())
	ctx := context.Background()

	first := n.Append(ctx, "telegram", "42", "user:john", "reply")
	second := n.Append(ctx, "telegram", "42", "user:john", "reply")
	if !strings.Contains(first, "waiting") {
		t.Fatalf("first = %q", first)
	}
	if second != "reply" {
		t.Errorf("second = %q; the interval was ignored", second)
	}
}

// Per conversation, so a second chat is told independently.
func TestTheIntervalIsPerConversation(t *testing.T) {
	t.Parallel()
	n := NewNotices(oneNotice(), openConfig())
	ctx := context.Background()

	n.Append(ctx, "telegram", "42", "user:john", "reply")
	other := n.Append(ctx, "telegram", "99", "user:john", "reply")
	if !strings.Contains(other, "waiting") {
		t.Errorf("a second conversation was silenced by the first: %q", other)
	}
}

func TestTheIntervalExpires(t *testing.T) {
	t.Parallel()
	cfg := openConfig()
	cfg.Interval = time.Millisecond
	n := NewNotices(oneNotice(), cfg)
	ctx := context.Background()

	n.Append(ctx, "telegram", "42", "user:john", "reply")
	time.Sleep(5 * time.Millisecond)
	again := n.Append(ctx, "telegram", "42", "user:john", "reply")
	if !strings.Contains(again, "waiting") {
		t.Errorf("the interval never expired: %q", again)
	}
}

// An empty queue today must not silence a full one tomorrow. Marking
// on the attempt rather than on the send would do exactly that.
func TestAnEmptyQueueDoesNotBurnTheInterval(t *testing.T) {
	t.Parallel()
	src := &stubSource{}
	n := NewNotices(src, openConfig())
	ctx := context.Background()

	if got := n.Append(ctx, "telegram", "42", "user:john", "reply"); got != "reply" {
		t.Fatalf("an empty queue produced a notice: %q", got)
	}
	src.notices = []Notice{{Text: "1 skill waiting for approval"}}
	got := n.Append(ctx, "telegram", "42", "user:john", "reply")
	if !strings.Contains(got, "waiting") {
		t.Errorf("a full queue was silenced by an earlier empty one: %q", got)
	}
}

// --- failure ------------------------------------------------------

// A notice is a courtesy riding on somebody else's turn. Failing a
// reply the user is waiting for because the courtesy could not be
// assembled is the wrong trade in every case.
func TestASourceFailureDoesNotTouchTheReply(t *testing.T) {
	t.Parallel()
	n := NewNotices(&stubSource{err: errors.New("store is unreachable")}, openConfig())
	if got := n.Append(context.Background(), "telegram", "42", "user:john", "reply"); got != "reply" {
		t.Errorf("got = %q", got)
	}
}

// A nil appender is what a deployment with self-learning off gets, and
// it must behave as though the feature does not exist.
func TestANilAppenderIsInert(t *testing.T) {
	t.Parallel()
	var n *Notices
	if got := n.Append(context.Background(), "telegram", "42", "user:john", "reply"); got != "reply" {
		t.Errorf("got = %q", got)
	}
	if NewNotices(nil, openConfig()) != nil {
		t.Error("a nil source produced a live appender")
	}
}

// --- rendering ------------------------------------------------------

// A count and a command, not a list. The operator does not need to
// evaluate three proposals inside a reply about something else.
func TestTheNoticeSaysHowManyAndWhatToRun(t *testing.T) {
	t.Parallel()
	got := PendingReviewNotice(3, 2)
	if len(got) != 1 {
		t.Fatalf("got %d notices", len(got))
	}
	text := got[0].Text
	for _, want := range []string{"3 skills", "2 refinements", "lobslaw learned pending"} {
		if !strings.Contains(text, want) {
			t.Errorf("notice = %q; missing %q", text, want)
		}
	}
	// One line: notices share a turn with a real reply, so a paragraph
	// is a notice that has become the message.
	if strings.Contains(text, "\n") {
		t.Errorf("notice spans lines: %q", text)
	}
}

func TestNothingPendingIsNoNotice(t *testing.T) {
	t.Parallel()
	if got := PendingReviewNotice(0, 0); got != nil {
		t.Errorf("got %v", got)
	}
}

func TestSingularReadsProperly(t *testing.T) {
	t.Parallel()
	text := PendingReviewNotice(1, 1)[0].Text
	if strings.Contains(text, "1 skills") || strings.Contains(text, "1 refinements") {
		t.Errorf("notice = %q", text)
	}
}

// --- subject derivation ---------------------------------------------

// The same "user:<id>" form the approval subject uses. Deriving it two
// different ways would make notices silently miss the person who can
// act on them.
func TestNoticeSubjectMatchesTheApprovalSubjectForm(t *testing.T) {
	t.Parallel()
	claims := &types.Claims{UserID: "tg-@john"}
	if got, want := noticeSubject(claims), grantSubject(claims); got != want {
		t.Errorf("noticeSubject = %q, grantSubject = %q", got, want)
	}
}

func TestNoticeSubjectIsEmptyForAnonymousClaims(t *testing.T) {
	t.Parallel()
	if got := noticeSubject(nil); got != "" {
		t.Errorf("got %q", got)
	}
	if got := noticeSubject(&types.Claims{}); got != "" {
		t.Errorf("got %q", got)
	}
}

// A Telegram account with a username is attributed as "tg-@name",
// which no config file can predict — the operator writes down the
// numeric id, because that is what the console shows and what
// identity resolution is keyed on.
//
// Matching only the principal meant a subject list naming the id
// could never match a turn from a user who had set a username, so the
// nudge was configured, logged itself enabled, and could not fire.
func TestANoticeReachesAUserKnownByTheirNumericIDInstead(t *testing.T) {
	t.Parallel()
	n := NewNotices(fixedNotices{"two proposals are waiting"}, NoticeConfig{
		Channels: []string{"telegram"},
		Subjects: []string{"user:tg-6972251926"},
		Interval: time.Nanosecond,
	})
	// The principal carries the username form; the id travels beside it.
	got := n.Append(context.Background(), "telegram", "chat-1",
		"user:tg-@jmylchreest", "the reply", "user:tg-6972251926")
	if !strings.Contains(got, "two proposals are waiting") {
		t.Errorf("the notice did not reach a user matched by their numeric id:\n%s", got)
	}
}

// Tolerance is not permission. Somebody else's id must not let a
// notice through.
func TestAnUnlistedIdentityStillGetsNothing(t *testing.T) {
	t.Parallel()
	n := NewNotices(fixedNotices{"waiting"}, NoticeConfig{
		Channels: []string{"telegram"},
		Subjects: []string{"user:tg-6972251926"},
		Interval: time.Nanosecond,
	})
	got := n.Append(context.Background(), "telegram", "chat-1",
		"user:tg-@someoneelse", "the reply", "user:tg-999")
	if got != "the reply" {
		t.Errorf("a notice reached an unlisted identity:\n%s", got)
	}
}

// fixedNotices is a source with one always-pending notice.
type fixedNotices struct{ text string }

func (f fixedNotices) Notices(_ context.Context, _ string) ([]Notice, error) {
	return []Notice{{Text: f.text}}, nil
}
