package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// defaultSlackAPIBase is Slack's Web API root. Overridable so tests
// can point the whole client at an httptest.Server, exactly as the
// Telegram handler's APIBase does.
const defaultSlackAPIBase = "https://slack.com/api"

// slackReadLimit caps one inbound Socket Mode frame.
//
// coder/websocket defaults to 32KiB, which a real events payload
// exceeds the first time somebody posts a long message with several
// attachments and blocks. The failure mode is the connection dying
// mid-conversation with a length error, so the limit is raised rather
// than discovered in production. 1MiB is well above anything Slack
// sends and still bounds a hostile frame.
const slackReadLimit = 1 << 20

// slackAckDeadline is how long Slack gives us to acknowledge an
// envelope before it assumes we died and redelivers.
//
// This is the constraint the whole channel is shaped around: an agent
// turn takes 30-90s and this is 3. The ack therefore cannot wait for
// the turn — it goes back immediately and the answer follows as a
// separate message.
const slackAckDeadline = 3 * time.Second

// slackAPI is the Web API half of the Slack client: a bot token, an
// HTTP client, and one method that every call goes through.
//
// Hand-rolled rather than vendored, matching the Telegram handler. The
// dependency this channel genuinely cannot avoid is a WebSocket
// implementation, because Socket Mode is a WebSocket protocol and the
// standard library has no client; everything above that is JSON over
// HTTP and a vendor SDK would add a large surface to save very little.
type slackAPI struct {
	botToken string
	base     string
	client   *http.Client
}

func newSlackAPI(botToken, base string, client *http.Client) *slackAPI {
	if base == "" {
		base = defaultSlackAPIBase
	}
	if client == nil {
		client = &http.Client{Timeout: slackAPIRequestTimeout}
	}
	return &slackAPI{botToken: botToken, base: strings.TrimRight(base, "/"), client: client}
}

// slackResponse is the envelope every Web API method returns. Slack
// signals failure with HTTP 200 and ok:false, so a caller that checks
// only the status code sees every error as success.
type slackResponse struct {
	OK       bool            `json:"ok"`
	Error    string          `json:"error,omitempty"`
	Warning  string          `json:"warning,omitempty"`
	URL      string          `json:"url,omitempty"`
	TS       string          `json:"ts,omitempty"`
	Channel  string          `json:"channel,omitempty"`
	Messages []slackMessage  `json:"messages,omitempty"`
	Meta     slackResponseMD `json:"response_metadata,omitempty"`
	User     *slackUserInfo  `json:"user,omitempty"`
}

type slackResponseMD struct {
	NextCursor string `json:"next_cursor,omitempty"`
}

type slackUserInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	RealName string `json:"real_name,omitempty"`
	IsBot    bool   `json:"is_bot,omitempty"`
	TeamID   string `json:"team_id,omitempty"`
}

// call posts a JSON body to a Web API method and decodes the envelope.
//
// token is a parameter rather than always a.botToken because
// apps.connections.open authenticates with the APP token and every
// other call with the BOT token. Passing the wrong one returns
// not_allowed_token_type, which is a confusing error to debug from a
// method name alone.
func (a *slackAPI) call(ctx context.Context, method, token string, body any) (*slackResponse, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("slack: marshal %s: %w", method, err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/"+method, rdr)
	if err != nil {
		return nil, fmt.Errorf("slack: build %s: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("slack: %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Cap the read: an unbounded io.ReadAll on a response we do not
	// control is how one confused proxy becomes an OOM.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, slackReadLimit))
	if err != nil {
		return nil, fmt.Errorf("slack: read %s: %w", method, err)
	}
	var out slackResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("slack: decode %s (http %d): %w", method, resp.StatusCode, err)
	}
	if !out.OK {
		// Slack's own error string is the useful part; the HTTP status
		// is almost always 200 and says nothing.
		return &out, fmt.Errorf("slack: %s: %s", method, errOrUnknown(out.Error))
	}
	return &out, nil
}

func errOrUnknown(s string) string {
	if s == "" {
		return "unknown error"
	}
	return s
}

// authTest confirms the bot token works and returns the bot's own user
// id, which the event loop needs in order to ignore its own messages.
func (a *slackAPI) authTest(ctx context.Context) (userID, teamID string, err error) {
	var out struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error,omitempty"`
		UserID string `json:"user_id"`
		TeamID string `json:"team_id"`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/auth.test", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.botToken)
	resp, err := a.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("slack: auth.test: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(io.LimitReader(resp.Body, slackReadLimit)).Decode(&out); err != nil {
		return "", "", fmt.Errorf("slack: decode auth.test: %w", err)
	}
	if !out.OK {
		return "", "", fmt.Errorf("slack: auth.test: %s", errOrUnknown(out.Error))
	}
	return out.UserID, out.TeamID, nil
}

// postMessage sends text to a channel. threadTS empty posts to the
// channel; set, it replies inside that thread.
func (a *slackAPI) postMessage(ctx context.Context, channel, threadTS, text string) error {
	body := map[string]any{"channel": channel, "text": text}
	if threadTS != "" {
		body["thread_ts"] = threadTS
	}
	_, err := a.call(ctx, "chat.postMessage", a.botToken, body)
	return err
}

// setAssistantStatus sets the greyed "…is working" line Slack renders
// under an assistant thread.
//
// The nicest available signal by some distance: it is not a message, so
// it leaves no trace, and Slack clears it when the reply lands. It is
// also the narrowest — it needs assistant:write AND an assistant
// thread, which means a DM with an app that has the Agents & AI Apps
// feature on. In a normal channel it fails, which is why every caller
// treats failure as "fall back", never as an error.
//
// An empty status clears it.
func (a *slackAPI) setAssistantStatus(ctx context.Context, channel, threadTS, status string) error {
	if threadTS == "" {
		// Not a thread, so there is no assistant thread to decorate.
		// Reported as an error so the caller falls back without paying
		// for a round trip that cannot succeed.
		return fmt.Errorf("slack: assistant status needs a thread")
	}
	_, err := a.call(ctx, "assistant.threads.setStatus", a.botToken, map[string]any{
		"channel_id": channel,
		"thread_ts":  threadTS,
		"status":     status,
	})
	return err
}

// updateMessage rewrites a message already posted, optionally
// replacing its blocks.
//
// This is how a Slack turn reports progress. There is no typing
// indicator available to a bot — that is a real-time-messaging
// affordance for humans — and the app has no reactions:write to stand
// one in for it. Posting a fresh "still working" message each time
// would leave a trail of scaffolding around every answer, so instead
// one message is posted at the start and rewritten in place until it
// becomes the reply.
func (a *slackAPI) updateMessage(ctx context.Context, channel, ts, text string, blocks []any) error {
	body := map[string]any{"channel": channel, "ts": ts, "text": text}
	if blocks != nil {
		body["blocks"] = blocks
	}
	_, err := a.call(ctx, "chat.update", a.botToken, body)
	return err
}

// postMessageTS posts and returns the new message's ts, so it can be
// updated later.
func (a *slackAPI) postMessageTS(ctx context.Context, channel, threadTS, text string) (string, error) {
	body := map[string]any{"channel": channel, "text": text}
	if threadTS != "" {
		body["thread_ts"] = threadTS
	}
	out, err := a.call(ctx, "chat.postMessage", a.botToken, body)
	if err != nil {
		return "", err
	}
	return out.TS, nil
}

// postBlocks sends a Block Kit message. text is still supplied and is
// not optional: it is what Slack shows in notifications and in any
// client that cannot render the blocks, so a blocks-only message
// arrives on a phone as an empty push.
func (a *slackAPI) postBlocks(ctx context.Context, channel, threadTS, text string, blocks []any) error {
	body := map[string]any{"channel": channel, "text": text, "blocks": blocks}
	if threadTS != "" {
		body["thread_ts"] = threadTS
	}
	_, err := a.call(ctx, "chat.postMessage", a.botToken, body)
	return err
}

// getUploadURL reserves an upload slot and returns where to PUT the
// bytes plus the id to complete with.
//
// Form-encoded, not JSON. Most of the Web API takes either;
// files.getUploadURLExternal takes only form params and answers a JSON
// body with invalid_arguments if handed one, which reads like a bad
// filename rather than a wrong content type.
//
// length must be the exact byte count, which is why the caller buffers
// the artifact before starting: Slack rejects a mismatch at the
// complete step, after the bytes have already been sent.
func (a *slackAPI) getUploadURL(ctx context.Context, filename string, length int) (uploadURL, fileID string, err error) {
	form := url.Values{}
	form.Set("filename", filename)
	form.Set("length", strconv.Itoa(length))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.base+"/files.getUploadURLExternal", strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.botToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("slack: files.getUploadURLExternal: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out struct {
		OK        bool   `json:"ok"`
		Error     string `json:"error,omitempty"`
		UploadURL string `json:"upload_url"`
		FileID    string `json:"file_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, slackReadLimit)).Decode(&out); err != nil {
		return "", "", fmt.Errorf("slack: decode files.getUploadURLExternal: %w", err)
	}
	if !out.OK {
		return "", "", fmt.Errorf("slack: files.getUploadURLExternal: %s", errOrUnknown(out.Error))
	}
	return out.UploadURL, out.FileID, nil
}

// uploadBytes PUTs the artifact to the reserved url.
//
// This one is NOT a Web API method: it goes to whatever host
// getUploadURL named, carries no bearer, and answers with plain text
// rather than the ok/error envelope. Sending the token here would be
// leaking it to an address the API chose.
func (a *slackAPI) uploadBytes(ctx context.Context, uploadURL string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("slack: upload: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, slackAPIErrorMaxBytes))
		return fmt.Errorf("slack: upload: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// completeUpload publishes an uploaded file into a conversation.
//
// Until this runs the file exists but belongs to nowhere — invisible
// to the user and counting against the workspace's storage. A failure
// between upload and complete therefore leaks a file rather than
// merely losing one, which is why the caller logs it distinctly.
func (a *slackAPI) completeUpload(ctx context.Context, fileID, title, channel, threadTS string) error {
	file := map[string]string{"id": fileID}
	if title != "" {
		file["title"] = title
	}
	body := map[string]any{
		"files":      []map[string]string{file},
		"channel_id": channel,
	}
	if threadTS != "" {
		body["thread_ts"] = threadTS
	}
	_, err := a.call(ctx, "files.completeUploadExternal", a.botToken, body)
	return err
}

// history reads a page of a conversation, newest first. cursor empty
// starts at the top; the returned cursor is empty at the end.
func (a *slackAPI) history(ctx context.Context, channel, cursor string, limit int) ([]slackMessage, string, error) {
	body := map[string]any{"channel": channel, "limit": limit}
	if cursor != "" {
		body["cursor"] = cursor
	}
	out, err := a.call(ctx, "conversations.history", a.botToken, body)
	if err != nil {
		return nil, "", err
	}
	return out.Messages, out.Meta.NextCursor, nil
}

// replies reads one thread, given the parent message ts.
func (a *slackAPI) replies(ctx context.Context, channel, ts string, limit int) ([]slackMessage, error) {
	out, err := a.call(ctx, "conversations.replies", a.botToken, map[string]any{
		"channel": channel, "ts": ts, "limit": limit,
	})
	if err != nil {
		return nil, err
	}
	return out.Messages, nil
}

// listConversations pages the conversations the bot can see, so a
// human-written "#general" can be turned into the id everything else
// works in. Bounded by maxPages at the call site.
func (a *slackAPI) listConversations(ctx context.Context, cursor string) ([]slackConversation, string, error) {
	body := map[string]any{
		"limit":            slackConversationPageSize,
		"exclude_archived": true,
		"types":            "public_channel,private_channel",
	}
	if cursor != "" {
		body["cursor"] = cursor
	}
	var out struct {
		OK       bool                `json:"ok"`
		Error    string              `json:"error,omitempty"`
		Channels []slackConversation `json:"channels"`
		Meta     slackResponseMD     `json:"response_metadata"`
	}
	if err := a.callInto(ctx, "conversations.list", a.botToken, body, &out); err != nil {
		return nil, "", err
	}
	if !out.OK {
		return nil, "", fmt.Errorf("slack: conversations.list: %s", errOrUnknown(out.Error))
	}
	return out.Channels, out.Meta.NextCursor, nil
}

type slackConversation struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// callInto is call() for a method whose response shape is not the
// shared envelope. Same auth and same limits; the caller checks ok.
func (a *slackAPI) callInto(ctx context.Context, method, token string, body any, into any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("slack: marshal %s: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/"+method, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("slack: build %s: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("slack: %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, slackReadLimit))
	if err != nil {
		return fmt.Errorf("slack: read %s: %w", method, err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("slack: decode %s (http %d): %w", method, resp.StatusCode, err)
	}
	return nil
}

// postEphemeral sends a message only the named user can see.
//
// The right shape for a command reply: the person ran it, the room did
// not, and a channel filling up with other people's /status output is
// how a bot gets muted. Falls back to a normal post at the call site
// when the API refuses — an ephemeral post fails in a conversation the
// bot can post to but the user is not a member of.
func (a *slackAPI) postEphemeral(ctx context.Context, channel, user, threadTS, text string) error {
	body := map[string]any{"channel": channel, "user": user, "text": text}
	if threadTS != "" {
		body["thread_ts"] = threadTS
	}
	_, err := a.call(ctx, "chat.postEphemeral", a.botToken, body)
	return err
}

// slackSlashCommand is the "slash_commands" envelope's payload.
//
// Note it carries no channel_type, so a slash command cannot tell a DM
// from a channel the way a message event can. The id prefix is the only
// available signal — see slackChannelIsDM.
type slackSlashCommand struct {
	Command   string `json:"command"`
	Text      string `json:"text"`
	UserID    string `json:"user_id"`
	TeamID    string `json:"team_id"`
	ChannelID string `json:"channel_id"`
}

// slackInteraction is the "interactive" envelope's payload — a button
// tap on a Block Kit message.
type slackInteraction struct {
	Type    string        `json:"type"`
	User    slackActor    `json:"user"`
	Team    slackTeamRef  `json:"team"`
	Channel slackChanRef  `json:"channel"`
	Message slackMsgRef   `json:"message"`
	Actions []slackAction `json:"actions"`
}

type slackActor struct {
	ID     string `json:"id"`
	TeamID string `json:"team_id,omitempty"`
}

type slackTeamRef struct {
	ID string `json:"id"`
}

type slackChanRef struct {
	ID string `json:"id"`
}

type slackMsgRef struct {
	TS       string `json:"ts,omitempty"`
	ThreadTS string `json:"thread_ts,omitempty"`
}

type slackAction struct {
	ActionID string `json:"action_id"`
	Value    string `json:"value,omitempty"`
}

// openConnection asks for a Socket Mode WebSocket URL. The returned
// URL is single-use and short-lived — reconnecting means calling this
// again, never redialling the old one.
func (a *slackAPI) openConnection(ctx context.Context, appToken string) (string, error) {
	out, err := a.call(ctx, "apps.connections.open", appToken, nil)
	if err != nil {
		return "", err
	}
	if out.URL == "" {
		return "", fmt.Errorf("slack: apps.connections.open returned no url")
	}
	return out.URL, nil
}

// --- Socket Mode envelopes -------------------------------------------

// slackEnvelope is one frame from the Socket Mode socket. Slack
// multiplexes every interaction type down this one connection and
// distinguishes them by Type.
type slackEnvelope struct {
	Type       string          `json:"type"`
	EnvelopeID string          `json:"envelope_id,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	// Reason accompanies a "disconnect" — "warning" (Slack is about to
	// cycle the socket, reconnect) or "refresh_requested".
	Reason string `json:"reason,omitempty"`
}

// slackEventsPayload is the "events_api" envelope's payload.
type slackEventsPayload struct {
	TeamID string     `json:"team_id"`
	Event  slackEvent `json:"event"`
}

// slackEvent is the subset of Slack's event shape this channel reads.
type slackEvent struct {
	Type string `json:"type"`
	// Subtype marks a non-message message: joins, edits, deletions,
	// bot posts. Anything with one is skipped — answering a
	// "channel_join" is noise, and answering a "message_changed" would
	// re-run a turn the user did not resend.
	Subtype     string `json:"subtype,omitempty"`
	User        string `json:"user,omitempty"`
	BotID       string `json:"bot_id,omitempty"`
	Text        string `json:"text,omitempty"`
	Channel     string `json:"channel,omitempty"`
	ChannelType string `json:"channel_type,omitempty"`
	TS          string `json:"ts,omitempty"`
	ThreadTS    string `json:"thread_ts,omitempty"`
	EventTS     string `json:"event_ts,omitempty"`
	// Files are attachments shared with the message. A file upload
	// arrives as subtype "file_share", frequently with no text at all.
	Files []slackFile `json:"files,omitempty"`
}

// slackMessage is a stored message as the conversations.* read methods
// return it. Shares fields with slackEvent but is a distinct shape:
// history entries have no channel_type and carry their own subtypes.
type slackMessage struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
	User    string `json:"user,omitempty"`
	BotID   string `json:"bot_id,omitempty"`
	// Username is the display name a bot posted under. Alert
	// integrations set it and often set no `user` at all, so it is the
	// only way to say WHO raised an alert.
	Username string `json:"username,omitempty"`
	Text     string `json:"text,omitempty"`
	TS       string `json:"ts,omitempty"`
	ThreadTS string `json:"thread_ts,omitempty"`
	// Attachments carry the actual content of most bot alerts. A
	// monitoring integration posts an empty `text` and puts the
	// hostname, severity and message in here, so a reader that looks
	// only at Text sees an alert channel as a column of blank lines.
	Attachments []slackAttachment `json:"attachments,omitempty"`
}

// slackAttachment is the legacy attachment shape, which is still what
// most alerting webhooks emit.
type slackAttachment struct {
	Fallback string             `json:"fallback,omitempty"`
	Pretext  string             `json:"pretext,omitempty"`
	Title    string             `json:"title,omitempty"`
	Text     string             `json:"text,omitempty"`
	Fields   []slackAttachField `json:"fields,omitempty"`
}

type slackAttachField struct {
	Title string `json:"title,omitempty"`
	Value string `json:"value,omitempty"`
}

// slackSocket wraps one Socket Mode connection.
type slackSocket struct {
	conn *websocket.Conn
}

func dialSlackSocket(ctx context.Context, url string, client *http.Client) (*slackSocket, error) {
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		return nil, fmt.Errorf("slack: socket dial: %w", err)
	}
	conn.SetReadLimit(slackReadLimit)
	return &slackSocket{conn: conn}, nil
}

func (s *slackSocket) read(ctx context.Context) (*slackEnvelope, error) {
	_, data, err := s.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	var env slackEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("slack: decode envelope: %w", err)
	}
	return &env, nil
}

// ack tells Slack the envelope arrived. Must happen inside
// slackAckDeadline or Slack redelivers, so it is sent before the turn
// runs and never after it.
func (s *slackSocket) ack(ctx context.Context, envelopeID string) error {
	if envelopeID == "" {
		return nil
	}
	b, err := json.Marshal(map[string]string{"envelope_id": envelopeID})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, slackAckDeadline)
	defer cancel()
	return s.conn.Write(ctx, websocket.MessageText, b)
}

// ping proves the connection is alive end to end.
//
// This is the liveness check a read deadline cannot be. coder/websocket
// answers Slack's pings inside Read, but answering one does not make
// Read RETURN — an idle Socket Mode connection is legitimately silent
// for minutes at a time, so a deadline on Read kills healthy sockets
// and only looks like it works on a busy workspace.
//
// Ping writes a ping frame and blocks until the matching pong arrives
// or ctx expires, which is a genuine round trip and cannot be satisfied
// by a partitioned link.
func (s *slackSocket) ping(ctx context.Context) error {
	return s.conn.Ping(ctx)
}

func (s *slackSocket) close() {
	_ = s.conn.Close(websocket.StatusNormalClosure, "")
}

// closeNow tears the connection down without the closing handshake.
//
// For a peer that has stopped answering, the graceful Close is the
// wrong tool: it writes a close frame and waits for the peer's reply,
// which is precisely what a dead peer will not send — so the call meant
// to unblock Read can itself block. CloseNow drops the TCP connection
// and Read returns immediately.
func (s *slackSocket) closeNow() {
	_ = s.conn.CloseNow()
}
