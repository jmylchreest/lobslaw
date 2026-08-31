package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

type stubSlack struct {
	conversation, thread, search int
	gotRef, gotTS, gotQuery      string
	gotRefs                      []string
	gotLimit                     int
	msgs                         []SlackTranscriptMessage
}

func (s *stubSlack) ReadConversation(_ context.Context, ref string, limit int) ([]SlackTranscriptMessage, error) {
	s.conversation++
	s.gotRef, s.gotLimit = ref, limit
	return s.msgs, nil
}

func (s *stubSlack) ReadThread(_ context.Context, ref, ts string, limit int) ([]SlackTranscriptMessage, error) {
	s.thread++
	s.gotRef, s.gotTS, s.gotLimit = ref, ts, limit
	return s.msgs, nil
}

func (s *stubSlack) SearchConversations(_ context.Context, q string, refs []string, limit int) ([]SlackTranscriptMessage, error) {
	s.search++
	s.gotQuery, s.gotRefs, s.gotLimit = q, refs, limit
	return s.msgs, nil
}

func slackTools(t *testing.T, r SlackReader) (read, search compute.BuiltinFunc) {
	t.Helper()
	b := NewBuiltins()
	if err := RegisterSlackBuiltins(b, SlackToolConfig{Reader: r}); err != nil {
		t.Fatalf("register: %v", err)
	}
	read, _ = b.Get("slack_read_channel")
	search, _ = b.Get("slack_search")
	if read == nil || search == nil {
		t.Fatal("slack tools are not registered")
	}
	return read, search
}

// thread_ts is what separates "read the channel" from "read this
// conversation". Reading the channel when a thread was asked for
// returns the surrounding noise instead of the exchange, which looks
// like an answer and is not one.
func TestSlackReadPicksThreadOrChannelByArgument(t *testing.T) {
	t.Parallel()

	t.Run("no thread_ts reads the channel", func(t *testing.T) {
		t.Parallel()
		s := &stubSlack{}
		read, _ := slackTools(t, s)
		if _, code, err := read(context.Background(), map[string]string{"channel": "C123"}); err != nil || code != 0 {
			t.Fatalf("code=%d err=%v", code, err)
		}
		if s.conversation != 1 || s.thread != 0 {
			t.Errorf("conversation=%d thread=%d; wrong read was issued", s.conversation, s.thread)
		}
		if s.gotRef != "C123" {
			t.Errorf("ref = %q, want C123", s.gotRef)
		}
	})

	t.Run("thread_ts reads the thread", func(t *testing.T) {
		t.Parallel()
		s := &stubSlack{}
		read, _ := slackTools(t, s)
		if _, code, err := read(context.Background(), map[string]string{
			"channel": "C123", "thread_ts": "1699999999.001",
		}); err != nil || code != 0 {
			t.Fatalf("code=%d err=%v", code, err)
		}
		if s.thread != 1 || s.conversation != 0 {
			t.Errorf("thread=%d conversation=%d; wrong read was issued", s.thread, s.conversation)
		}
		if s.gotTS != "1699999999.001" {
			t.Errorf("ts = %q, want the timestamp as given", s.gotTS)
		}
	})
}

// A limit the model invents must be bounded before it reaches Slack.
// The ceiling is the point: an unbounded limit is a way to pull a whole
// channel into the prompt one call at a time.
func TestSlackLimitsAreClamped(t *testing.T) {
	t.Parallel()

	s := &stubSlack{}
	read, search := slackTools(t, s)

	read(context.Background(), map[string]string{"channel": "C1", "limit": "100000"}) //nolint:errcheck // asserting the clamp, not the call
	if s.gotLimit > 200 {
		t.Errorf("read limit = %d, above the 200 ceiling", s.gotLimit)
	}
	search(context.Background(), map[string]string{"query": "q", "limit": "100000"}) //nolint:errcheck // as above
	if s.gotLimit > 25 {
		t.Errorf("search limit = %d, above the 25 ceiling", s.gotLimit)
	}
}

// Both tools refuse their missing required argument with exit 2 — an
// argument error the model can correct, rather than a fault it retries.
func TestSlackToolsRequireTheirArgument(t *testing.T) {
	t.Parallel()

	s := &stubSlack{}
	read, search := slackTools(t, s)

	if _, code, err := read(context.Background(), map[string]string{}); err == nil || code != 2 {
		t.Errorf("read with no channel: code=%d err=%v", code, err)
	}
	if _, code, err := search(context.Background(), map[string]string{}); err == nil || code != 2 {
		t.Errorf("search with no query: code=%d err=%v", code, err)
	}
	if s.conversation+s.thread+s.search != 0 {
		t.Error("a refused call still reached Slack")
	}
}

// The channels filter narrows a search. Dropping it would search
// everywhere the token can reach, which is a wider read than asked for.
func TestSlackSearchPassesItsChannelFilter(t *testing.T) {
	t.Parallel()

	s := &stubSlack{}
	_, search := slackTools(t, s)
	if _, code, err := search(context.Background(), map[string]string{
		"query": "deploy", "channels": "C1,C2",
	}); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if s.gotQuery != "deploy" {
		t.Errorf("query = %q", s.gotQuery)
	}
	if len(s.gotRefs) != 2 || !strings.Contains(strings.Join(s.gotRefs, ","), "C2") {
		t.Errorf("channel filter = %v, want both channels", s.gotRefs)
	}
}

// No reader means no tools, rather than tools that fail on first use.
func TestSlackWithoutAReaderRegistersNothing(t *testing.T) {
	t.Parallel()

	b := NewBuiltins()
	if err := RegisterSlackBuiltins(b, SlackToolConfig{}); err == nil {
		t.Error("slack tools registered with no reader behind them")
	}
	if _, ok := b.Get("slack_search"); ok {
		t.Error("slack_search registered with no reader")
	}
}
