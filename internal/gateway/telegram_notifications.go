package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/notify"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

const telegramNoticeChannel = "telegram-notifications"
const telegramNoticeRetention = 30 * 24 * time.Hour
const maxTelegramNoticeRecords = 1000

type telegramNoticeRef struct {
	Owner    string
	BotID    string
	TaskID   string
	ThreadID string
	URL      string
	Context  string
	At       time.Time
}

type telegramNoticeState struct {
	dirty   bool
	Owner   string
	Since   time.Time
	Sent    map[string]time.Time
	Replies map[string]telegramNoticeRef
}

func noticeHash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:16]) }

func (h *TelegramHandler) notificationStateKey(chat int64) string {
	// Numeric bot identity survives a BotFather token rotation. The token
	// itself is never a stored key, notification argument or log field.
	bot, _, _ := strings.Cut(h.cfg.BotToken, ":")
	if _, err := strconv.ParseInt(bot, 10, 64); err != nil {
		bot = noticeHash(h.cfg.BotToken)
	}
	return bot + "/" + strconv.FormatInt(chat, 10)
}

func (h *TelegramHandler) loadNoticeState(ctx context.Context, chat int64, owner string) (*telegramNoticeState, error) {
	raw, err := h.cfg.ChannelState.Get(ctx, telegramNoticeChannel, h.notificationStateKey(chat))
	if err != nil && !errors.Is(err, types.ErrNotFound) {
		return nil, err
	}
	state := &telegramNoticeState{Owner: owner, Since: time.Now(), Sent: map[string]time.Time{}, Replies: map[string]telegramNoticeRef{}, dirty: true}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, state); err != nil {
			return nil, err
		}
		state.dirty = false
		if state.Owner != owner {
			return &telegramNoticeState{Owner: owner, Since: time.Now(), Sent: map[string]time.Time{}, Replies: map[string]telegramNoticeRef{}, dirty: true}, nil
		}
		if state.Sent == nil || state.Replies == nil || state.Since.IsZero() {
			return nil, errors.New("invalid Telegram notification state")
		}
	}
	return state, nil
}

func (h *TelegramHandler) saveNoticeState(ctx context.Context, chat int64, state *telegramNoticeState) error {
	cutoff := time.Now().Add(-telegramNoticeRetention)
	for key, ref := range state.Replies {
		if ref.At.Before(cutoff) {
			delete(state.Replies, key)
		}
	}
	// Delivery receipts are retired only after a complete outbox scan proves
	// their event is no longer active. Conversation mappings have their own cap.
	if len(state.Replies) > maxTelegramNoticeRecords {
		keys := make([]string, 0, len(state.Replies))
		for key := range state.Replies {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return state.Replies[keys[i]].At.Before(state.Replies[keys[j]].At) })
		for _, key := range keys[:len(keys)-maxTelegramNoticeRecords] {
			delete(state.Replies, key)
		}
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return h.cfg.ChannelState.Put(ctx, telegramNoticeChannel, h.notificationStateKey(chat), raw)
}

func (h *TelegramHandler) NotificationsEnabled() bool { return h.cfg.NotifyTasks }

func (h *TelegramHandler) commitNoticeState(ctx context.Context, chat int64, state *telegramNoticeState) error {
	// Once Telegram accepted a message, preserve its routing receipt even if
	// shutdown cancels the dispatch context between the response and this write.
	commit, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	return h.saveNoticeState(commit, chat, state)
}

func (h *TelegramHandler) DispatchNotifications(ctx context.Context, source notify.EventSource) error {
	return h.DispatchNotificationPages(ctx, notify.SinglePage(source))
}

func (h *TelegramHandler) DispatchNotificationPages(ctx context.Context, source notify.EventPages) error {
	h.notificationDispatchMu.Lock()
	defer h.notificationDispatchMu.Unlock()
	if !h.cfg.NotifyTasks || (h.cfg.Gate != nil && !h.cfg.Gate.Owned("telegram-notifications")) {
		return nil
	}
	var errs []error
	for chat, owner := range h.cfg.NotificationRecipients {
		if err := h.dispatchChatPages(ctx, chat, owner, source); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (h *TelegramHandler) dispatchChatPages(ctx context.Context, chat int64, owner string, source notify.EventPages) error {
	h.notificationMu.Lock()
	state, err := h.loadNoticeState(ctx, chat, owner)
	// Persist activation even when the first poll has no matching events.
	if err == nil && state.dirty {
		err = h.saveNoticeState(ctx, chat, state)
	}
	for key, at := range h.notificationRetry {
		if time.Since(at) > 24*time.Hour {
			delete(h.notificationRetry, key)
		}
	}
	h.notificationMu.Unlock()
	if err != nil {
		return err
	}
	live := map[string]bool{}
	complete := true
	for cursor := ""; ; {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := source(ctx, owner, cursor)
		if err != nil {
			return err
		}
		complete = complete && !page.Incomplete
		for _, event := range page.Events {
			live[noticeHash(event.ID)] = true
			if err := h.deliverChatNotice(ctx, chat, owner, event, state); err != nil {
				return err
			}
		}
		if page.Next == "" {
			break
		}
		if page.Next == cursor {
			return errors.New("notification cursor did not advance")
		}
		cursor = page.Next
	}
	if complete {
		return h.retireNoticeReceipts(ctx, chat, owner, live)
	}
	return nil
}

func (h *TelegramHandler) deliverChatNotice(ctx context.Context, chat int64, owner string, event notify.Event, state *telegramNoticeState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if h.cfg.Gate != nil && !h.cfg.Gate.Owned("telegram-notifications") {
		return nil
	}
	if event.BotID == "" || event.ID == "" || (!event.Attention && event.At.Before(state.Since)) || event.At.Before(time.Now().Add(-24*time.Hour)) || (!event.Expires.IsZero() && time.Now().After(event.Expires)) {
		return nil
	}
	key := noticeHash(event.ID)
	if _, sent := state.Sent[key]; sent {
		return nil
	}
	retryKey := h.notificationStateKey(chat) + "/" + key
	h.notificationMu.Lock()
	retry := h.notificationRetry[retryKey]
	h.notificationMu.Unlock()
	if time.Now().Before(retry) {
		return nil
	}
	if h.cfg.NotificationBotCheck == nil {
		return errors.New("telegram notification owner check is not wired")
	}
	if err := h.cfg.NotificationBotCheck(ctx, owner, event.BotID); err != nil {
		return err
	}
	thread := key
	if event.TaskID != "" {
		thread = noticeHash(owner + "/" + event.BotID + "/" + event.TaskID)
	}
	ref := telegramNoticeRef{Owner: owner, BotID: event.BotID, TaskID: event.TaskID, ThreadID: thread, URL: event.URL, Context: event.Title + "\n" + event.Body + "\nTask: " + event.TaskID + "\nOwn inbox item (inbox_read): " + event.InboxID, At: time.Now()}
	text := event.Title + "\n\n" + event.Body + "\n\nReply to this message to discuss it with this agent. Open the console for details and task decisions."
	id, err := h.sendNoticeMessage(ctx, chat, 0, text, event.URL)
	h.notificationMu.Lock()
	defer h.notificationMu.Unlock()
	if err != nil {
		h.notificationRetry[retryKey] = time.Now().Add(2 * time.Minute)
		return err
	}
	current, err := h.loadNoticeState(ctx, chat, owner)
	if err != nil {
		return err
	}
	current.Sent[key] = time.Now()
	current.Replies[strconv.FormatInt(id, 10)] = ref
	if err := h.commitNoticeState(ctx, chat, current); err != nil {
		return fmt.Errorf("telegram notice sent but receipt could not be saved: %w", err)
	}
	state.Sent[key] = current.Sent[key]
	delete(h.notificationRetry, retryKey)
	h.log.Info("telegram: task notification delivered", "bot", event.BotID, "chat_id", chat, "message_id", id, "event_id", event.ID, "console_path", event.URL)
	return nil
}

func (h *TelegramHandler) retireNoticeReceipts(ctx context.Context, chat int64, owner string, live map[string]bool) error {
	h.notificationMu.Lock()
	defer h.notificationMu.Unlock()
	state, err := h.loadNoticeState(ctx, chat, owner)
	if err != nil {
		return err
	}
	type receipt struct {
		key string
		at  time.Time
	}
	var inactive []receipt
	changed := false
	for key, at := range state.Sent {
		if live[key] {
			continue
		}
		if time.Since(at) > telegramNoticeRetention {
			delete(state.Sent, key)
			changed = true
		} else {
			inactive = append(inactive, receipt{key, at})
		}
	}
	sort.Slice(inactive, func(i, j int) bool { return inactive[i].at.Before(inactive[j].at) })
	for i := 0; i < len(inactive)-maxTelegramNoticeRecords; i++ {
		delete(state.Sent, inactive[i].key)
		changed = true
	}
	if changed {
		return h.saveNoticeState(ctx, chat, state)
	}
	return nil
}

func (h *TelegramHandler) notificationLink(target string) (string, error) {
	u, err := url.Parse(target)
	if err != nil || u.IsAbs() || u.Host != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || path.Clean(u.Path) != u.Path || (!strings.HasPrefix(u.Path, "/bots/") && !strings.HasPrefix(u.Path, "/approvals/")) {
		return "", errors.New("invalid console notification path")
	}
	return strings.TrimSuffix(h.cfg.ConsoleURL, "/") + u.EscapedPath(), nil
}

func (h *TelegramHandler) sendNoticeMessage(ctx context.Context, chat, replyTo int64, text, path string) (int64, error) {
	link, err := h.notificationLink(path)
	if err != nil {
		return 0, err
	}
	if len([]rune(text)) > 1500 {
		text = string([]rune(text)[:1500]) + "…"
	}
	body := map[string]any{"chat_id": chat, "text": text + "\n\n" + link, "link_preview_options": map[string]bool{"is_disabled": true}, "reply_markup": map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "Open in Lobslaw", "url": link}}}}}
	if replyTo != 0 {
		body["reply_parameters"] = map[string]any{"message_id": replyTo, "allow_sending_without_reply": true}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.base+"/bot"+h.cfg.BotToken+"/sendMessage", bytes.NewReader(data))
	if err != nil {
		return 0, errors.New("invalid Telegram API request")
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := h.client.Do(req)
	if err != nil {
		return 0, errors.New("telegram notification delivery failed; retry pending")
	}
	defer func() { _ = response.Body.Close() }()
	var ack struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&ack); err != nil {
		return 0, errors.New("invalid Telegram notification response")
	}
	if response.StatusCode != http.StatusOK || !ack.OK || ack.Result.MessageID <= 0 {
		return 0, fmt.Errorf("telegram rejected notification (HTTP %d)", response.StatusCode)
	}
	return ack.Result.MessageID, nil
}

func (h *TelegramHandler) notificationReply(ctx context.Context, msg *tgMessage, owner string) (*telegramNoticeRef, error) {
	if !h.cfg.NotifyTasks || msg.ReplyToMessage == nil {
		return nil, nil
	}
	bound := h.cfg.NotificationRecipients[msg.Chat.ID]
	if bound == "" {
		return nil, nil
	}
	if isSharedChat(msg.Chat) || msg.From == nil || msg.From.ID != msg.Chat.ID || owner != bound {
		return nil, errors.New("notification reply owner mismatch")
	}
	h.notificationMu.Lock()
	state, err := h.loadNoticeState(ctx, msg.Chat.ID, owner)
	h.notificationMu.Unlock()
	if err != nil {
		return nil, err
	}
	ref, ok := state.Replies[strconv.FormatInt(msg.ReplyToMessage.MessageID, 10)]
	if !ok {
		return nil, nil
	}
	if ref.Owner != owner || time.Since(ref.At) > telegramNoticeRetention {
		return nil, errors.New("notification context expired")
	}
	if h.cfg.NotificationBotCheck == nil {
		return nil, errors.New("notification owner check unavailable")
	}
	if err := h.cfg.NotificationBotCheck(ctx, owner, ref.BotID); err != nil {
		return nil, err
	}
	h.log.Info("telegram: notification reply routed", "bot", ref.BotID, "task_id", ref.TaskID, "chat_id", msg.Chat.ID, "thread_id", ref.ThreadID)
	return &ref, nil
}

func (h *TelegramHandler) sendNotificationReply(ctx context.Context, chat, replyTo int64, text string, ref telegramNoticeRef) error {
	id, err := h.sendNoticeMessage(ctx, chat, replyTo, text, ref.URL)
	if err != nil {
		return err
	}
	h.notificationMu.Lock()
	defer h.notificationMu.Unlock()
	state, err := h.loadNoticeState(ctx, chat, ref.Owner)
	if err != nil {
		return err
	}
	ref.At = time.Now()
	state.Replies[strconv.FormatInt(id, 10)] = ref
	if err := h.commitNoticeState(ctx, chat, state); err != nil {
		return err
	}
	h.log.Info("telegram: notification reply delivered", "bot", ref.BotID, "chat_id", chat, "message_id", id, "thread_id", ref.ThreadID)
	return nil
}
