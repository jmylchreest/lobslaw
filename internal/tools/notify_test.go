package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/turn"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/notify"
)

type stubNotifier struct {
	sent []notify.Notification
	err  error
}

func (s *stubNotifier) Send(_ context.Context, n notify.Notification) error {
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, n)
	return nil
}

func notifyTool(t *testing.T, svc Notifier) compute.BuiltinFunc {
	t.Helper()
	b := NewBuiltins()
	if err := RegisterNotifyBuiltins(b, NotifyConfig{Service: svc}); err != nil {
		t.Fatalf("register: %v", err)
	}
	fn, ok := b.Get("notify")
	if !ok {
		t.Fatal("notify is not registered")
	}
	return fn
}

// The text and the recipient both have to survive to the notifier. A
// notification delivered to the right person with the wrong words, or
// the right words to the wrong person, is worse than one that failed.
func TestNotifyPassesRecipientAndTextThrough(t *testing.T) {
	t.Parallel()

	svc := &stubNotifier{}
	fn := notifyTool(t, svc)
	if _, code, err := fn(context.Background(), map[string]string{
		"user_id": "user:alice", "text": "the build finished",
	}); err != nil || code != 0 {
		t.Fatalf("notify: code=%d err=%v", code, err)
	}
	if len(svc.sent) != 1 {
		t.Fatalf("sent %d notifications, want 1", len(svc.sent))
	}
	if svc.sent[0].UserID != "user:alice" {
		t.Errorf("UserID = %q, want user:alice", svc.sent[0].UserID)
	}
	if svc.sent[0].Body != "the build finished" {
		t.Errorf("Body = %q, want the message as written", svc.sent[0].Body)
	}
}

// Both arguments are required, and refusing is exit 2 — the model can
// fix an argument error, where exit 1 invites a retry of something
// that will fail identically.
func TestNotifyRequiresRecipientAndText(t *testing.T) {
	t.Parallel()

	svc := &stubNotifier{}
	fn := notifyTool(t, svc)
	for _, args := range []map[string]string{
		{"text": "no recipient"},
		{"user_id": "user:alice"},
		{"user_id": "", "text": ""},
	} {
		_, code, err := fn(context.Background(), args)
		if err == nil || code != 2 {
			t.Errorf("args %v were accepted (code=%d err=%v)", args, code, err)
		}
	}
	if len(svc.sent) != 0 {
		t.Errorf("%d notifications were sent despite refusal", len(svc.sent))
	}
}

// A delivery failure reaches the model rather than being swallowed.
// The agent often tells the user it has notified someone; doing that
// after a silent failure is a lie it had the information to avoid.
func TestNotifySurfacesADeliveryFailure(t *testing.T) {
	t.Parallel()

	fn := notifyTool(t, &stubNotifier{err: errors.New("no channel bound")})
	out, code, err := fn(context.Background(), map[string]string{
		"user_id": "user:alice", "text": "hello",
	})
	if err == nil && code == 0 {
		t.Errorf("a failed delivery reported success: %s", out)
	}
}

// No notifier means no tool, rather than a tool that fails on use.
func TestNotifyWithoutAServiceRegistersNothing(t *testing.T) {
	t.Parallel()

	b := NewBuiltins()
	_ = RegisterNotifyBuiltins(b, NotifyConfig{})
	if _, ok := b.Get("notify"); ok {
		t.Error("notify registered with no service behind it")
	}
}

// A bot cannot name its own sender.
//
// Attribution is stamped from the turn identity, and the schema is
// closed so the model has no field to put one in. Both halves matter:
// if a sender parameter ever appears, marketing could send you
// something that reads as though engineering said it — and the whole
// value of attributing a ping is that the attribution is true.
func TestNotifyOffersNoWayToClaimADifferentSender(t *testing.T) {
	t.Parallel()

	var schema struct {
		Properties           map[string]any `json:"properties"`
		AdditionalProperties *bool          `json:"additionalProperties"`
	}
	var found bool
	for _, td := range NotifyToolDefs() {
		if td.Name != "notify" {
			continue
		}
		found = true
		if err := json.Unmarshal(td.ParametersSchema, &schema); err != nil {
			t.Fatalf("notify schema does not parse: %v", err)
		}
	}
	if !found {
		t.Fatal("no notify tool definition")
	}

	for _, banned := range []string{"sender", "sender_bot", "from", "as_bot", "bot_id"} {
		if _, ok := schema.Properties[banned]; ok {
			t.Errorf("notify accepts %q — a bot can attribute a message to somebody else", banned)
		}
	}
	// Closed schema, so an unlisted field cannot slip through either.
	if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
		t.Error("notify's schema is open; an arbitrary field can be smuggled in")
	}
}

// A bot working its inbox must still be able to reach you.
//
// Persisting headless transcripts gave those turns a channel name
// ("bot") where they previously had none. notify routes an originating
// channel back to itself, so what had been "tell the user however they
// asked to be told" silently became "reply into a transcript" — and
// failed with `no sink registered for channel "bot"`. Nothing else
// changed; a feature that had worked stopped, because a field that had
// been empty stopped being empty.
func TestNotifyBroadcastsWhenTheChannelIsSynthetic(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		identity turn.Identity
		want     string // expected OriginatorChannel
	}{
		{
			name:     "a real channel is replied to in place",
			identity: turn.Identity{Channel: "telegram", ChannelID: "-100123"},
			want:     "telegram",
		},
		{
			name:     "the synthetic bot channel broadcasts instead",
			identity: turn.Identity{Channel: turn.ChannelBot, ChannelID: "devops.inbox.01ABC"},
			want:     "",
		},
		{
			name:     "no channel at all broadcasts, as it always did",
			identity: turn.Identity{},
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := humanChannel(tc.identity); got != tc.want {
				t.Errorf("OriginatorChannel = %q, want %q", got, tc.want)
			}
			// The id has to travel with it. Left behind, notify would
			// fall back to a channel id for a channel it is no longer
			// delivering on.
			if tc.want == "" && humanChannelID(tc.identity) != "" {
				t.Error("channel id survived a channel that did not")
			}
		})
	}
}

// Who asked is decided by the principal, not by whether a bot is
// running the turn.
//
// Identity.IsBot() is `BotID != ""`, and since channel turns resolve
// to the default team's coordinator, EVERY turn has a BotID now —
// including one a person is driving. Testing that here discarded the
// requester in exactly the case where there was one.
func TestTheRequesterIsTheHumanEvenWhenABotRunsTheTurn(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		id   turn.Identity
		want string
	}{
		{
			name: "a person talking to the coordinator",
			id: turn.Identity{
				BotID:     "coordinator",
				Principal: identity.Principal("user:sam"),
			},
			want: "user:sam",
		},
		{
			name: "a bot working on its own asked nobody",
			id: turn.Identity{
				BotID:     "devops",
				Principal: identity.Bot("devops"),
			},
			want: "",
		},
		{
			name: "a carried requester wins over everything",
			id: turn.Identity{
				BotID:       "research",
				Principal:   identity.Bot("research"),
				RequestedBy: "user:sam",
			},
			want: "user:sam",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requesterLabel(tc.id); got != tc.want {
				t.Errorf("requesterLabel = %q, want %q", got, tc.want)
			}
		})
	}
}
