package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// TelegramSink adapts a TelegramHandler to the notify.Sink
// interface. Wraps Send so address-as-string (the channel-agnostic
// abstraction) becomes chat_id-as-int64 (Telegram's wire shape).
type TelegramSink struct {
	Handler *TelegramHandler
}

func (s *TelegramSink) ChannelType() string { return "telegram" }

func (s *TelegramSink) Deliver(_ context.Context, address, body string) error {
	if s.Handler == nil {
		return errors.New("telegram sink: handler not wired")
	}
	chatID, err := strconv.ParseInt(address, 10, 64)
	if err != nil {
		return fmt.Errorf("telegram sink: address %q must parse as int64 chat_id: %w", address, err)
	}
	return s.Handler.Send(chatID, body)
}

// SlackSink adapts a SlackHandler to the notify.Sink interface.
//
// The address is a Slack conversation id, and chat.postMessage accepts
// a user id there directly — it opens the DM itself — so a proactive
// notification reaches a person without the sink needing to know
// whether it is addressing a channel or a human.
type SlackSink struct {
	Handler *SlackHandler
}

func (s *SlackSink) ChannelType() string { return ChannelSlack }

func (s *SlackSink) Deliver(ctx context.Context, address, body string) error {
	if s.Handler == nil {
		return errors.New("slack sink: handler not wired")
	}
	if address == "" {
		return errors.New("slack sink: empty address")
	}
	// No thread: a notification is a new topic, not a reply to
	// whatever the last conversation happened to be about.
	return s.Handler.api.postMessage(ctx, address, "", body)
}

// RESTSink is a placeholder — REST is request/response and can't
// push asynchronously to an already-disconnected client. The
// originator-channel reply path doesn't go through here; the
// gateway's existing reply mechanism handles that. Broadcasts to
// REST users are dropped with a warning until we add a webhook
// callback (operator-supplied URL we POST to).
type RESTSink struct{}

func (s *RESTSink) ChannelType() string { return "rest" }

func (s *RESTSink) Deliver(_ context.Context, address, body string) error {
	return fmt.Errorf("rest sink: REST broadcast not supported (address=%q); add a webhook callback URL to deliver async REST messages", address)
}

// ChannelCallback is the notify channel type for an operator-supplied
// HTTP callback. Bound per person in [[user]].channels:
//
//	[[user]]
//	id = "alice"
//	channels = [{ type = "callback", address = "https://ops.example/lobslaw" }]
//
// Named for what it is rather than "webhook", which already means the
// INBOUND [[gateway.channels]] type. The two point in opposite
// directions and sharing a word would make every config question
// ambiguous.
const ChannelCallback = "callback"

// CallbackSink POSTs a notification to a URL the operator declared.
//
// This is the answer to the one thing REST cannot do. A generation
// runs for minutes; by the time it finishes the request that asked for
// it is long closed, so there is no response left to write into and
// RESTSink can only report that. A callback inverts it: lobslaw dials
// out when the work is done, which needs nothing held open in between.
//
// Chosen over a websocket deliberately. A socket has to be connected
// at delivery time, and "the caller went away" is precisely the case
// this exists to serve — plus a socket lives on one node while the
// scheduler that completes the job may be on another.
type CallbackSink struct {
	// Client routes through the egress proxy under role
	// "gateway/callback", so an operator's declared hosts are the only
	// ones reachable and smokescreen's private-range refusal still
	// applies.
	Client *http.Client

	Logger *slog.Logger
}

func (s *CallbackSink) ChannelType() string { return ChannelCallback }

// callbackPayload is JSON rather than the bare text the other sinks
// send. A callback lands in a script, not in front of a person, and a
// receiver that has to parse prose to find out what happened is a
// receiver that breaks when the wording changes.
type callbackPayload struct {
	Body string `json:"body"`
}

func (s *CallbackSink) Deliver(ctx context.Context, address, body string) error {
	if s.Client == nil {
		return errors.New("callback sink: no http client wired")
	}
	u, err := url.Parse(strings.TrimSpace(address))
	if err != nil {
		return fmt.Errorf("callback sink: address %q: %w", address, err)
	}
	// Scheme checked here as well as at boot. The address reaches this
	// point from a replicated user record, and a bare host would
	// otherwise be dialled as a relative path against nothing.
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("callback sink: address %q needs an http:// or https:// scheme", address)
	}

	payload, err := json.Marshal(callbackPayload{Body: body})
	if err != nil {
		return fmt.Errorf("callback sink: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("callback sink: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("callback sink: post to %s: %w", u.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// The body is drained but not read: a callback receiver has nothing
	// to say back, and leaving it unread wastes the connection.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	if resp.StatusCode >= 300 {
		return fmt.Errorf("callback sink: %s answered HTTP %d", u.Host, resp.StatusCode)
	}
	return nil
}
