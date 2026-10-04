package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

const (
	learnedReviewTTL   = 30 * time.Minute
	learnedPageSize    = 8
	learnedInlineBytes = 3000
)

type learnedTarget struct {
	ID       string `json:"id"`
	Revision uint64 `json:"revision"`
	Digest   string `json:"digest"`
	UserID   string `json:"user_id"`
}

func (h *TelegramHandler) registerLearnedCommand() {
	if h.cfg.Learned == nil || h.cfg.Prompts == nil {
		return
	}
	h.commands.Register(&Command{Name: "learned", Summary: "review proposed skills and amendments in private", Handler: func(ctx context.Context, req CommandRequest) (string, error) {
		chatID, err := strconv.ParseInt(req.Session.ChannelID, 10, 64)
		if err != nil {
			return "", err
		}
		parts := strings.Fields(req.Args)
		switch {
		case len(parts) == 2 && parts[0] == "show":
			err = h.showLearned(ctx, chatID, req.Claims, parts[1])
		case len(parts) == 0 || (len(parts) == 1 && parts[0] == "pending"):
			err = h.listLearned(ctx, chatID, req.Claims, 1)
		default:
			return "Use /learned to open the queue, or /learned show <id> to review one proposal.", nil
		}
		if err != nil {
			return "", err
		}
		return "Use /learned to reopen the review queue at any time.", nil
	}})
}

func (h *TelegramHandler) listLearned(ctx context.Context, chatID int64, claims *types.Claims, page int) error {
	rows, err := h.cfg.Learned.List(ctx, claims)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return h.learnedPost(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": "No proposed skills or amendments are waiting for your review."})
	}
	pages := (len(rows) + learnedPageSize - 1) / learnedPageSize
	if page < 1 || page > pages {
		return fmt.Errorf("that page no longer exists; reopen /learned")
	}
	end := min(page*learnedPageSize, len(rows))
	var text strings.Builder
	fmt.Fprintf(&text, "Pending review — %d items (page %d/%d)\n", len(rows), page, pages)
	var buttons [][]map[string]string
	for _, r := range rows[(page-1)*learnedPageSize : end] {
		kind := "proposal"
		if r.Pending != nil {
			kind = "amendment"
		}
		fmt.Fprintf(&text, "\n%s (%s)\n", r.Name, kind)
		target, _ := json.Marshal(learnedTarget{ID: r.ID, Revision: r.Revision, Digest: r.Digest, UserID: claims.UserID})
		p, err := h.cfg.Prompts.Create(NewPrompt{Action: "learned:open", Resource: string(target), Channel: "telegram", ChannelID: strconv.FormatInt(chatID, 10), RaisedFor: claims.UserID, TTL: learnedReviewTTL})
		if err != nil {
			return err
		}
		buttons = append(buttons, []map[string]string{{"text": "Review " + r.Name, "callback_data": "learned:show:" + p.ID}})
	}
	if page > 1 {
		buttons = append(buttons, []map[string]string{{"text": "Previous", "callback_data": "learned:list:" + strconv.Itoa(page-1)}})
	}
	if page < pages {
		buttons = append(buttons, []map[string]string{{"text": "Next", "callback_data": "learned:list:" + strconv.Itoa(page+1)}})
	}
	return h.learnedPost(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": text.String(), "reply_markup": map[string]any{"inline_keyboard": buttons}})
}

func (h *TelegramHandler) showLearned(ctx context.Context, chatID int64, claims *types.Claims, id string) error {
	r, err := h.cfg.Learned.Get(ctx, claims, id)
	if err != nil {
		return err
	}
	text := renderLearnedReview(r)
	// Do not offer approval if delivery of the complete content failed.
	if len(text) <= learnedInlineBytes {
		err = h.learnedPost(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": text})
	} else {
		err = h.sendLearnedDocument(ctx, chatID, text)
	}
	if err != nil {
		return err
	}
	target, _ := json.Marshal(learnedTarget{ID: r.ID, Revision: r.Revision, Digest: r.Digest, UserID: claims.UserID})
	p, err := h.cfg.Prompts.Create(NewPrompt{Action: learnedReviewAction, Resource: string(target), Reason: r.Name, Channel: "telegram", ChannelID: strconv.FormatInt(chatID, 10), RaisedFor: claims.UserID, TTL: learnedReviewTTL})
	if err != nil {
		return err
	}
	return h.learnedPost(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": fmt.Sprintf("Decide %s, revision %d. Read the complete review above before approving. Later leaves it pending.", r.Name, r.Revision), "reply_markup": map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "Approve", "callback_data": "learned:approve:" + p.ID}, {"text": "Deny", "callback_data": "learned:deny:" + p.ID}, {"text": "Later", "callback_data": "learned:later:" + p.ID}}}}})
}

// These callbacks are distinct from tool confirmations: no session or always
// grants, no agent continuation, and current authority is checked on every tap.
func (h *TelegramHandler) handleLearnedCallback(ctx context.Context, q *tgCallbackQuery) {
	if h.cfg.Learned == nil || h.cfg.Prompts == nil || q.From == nil || q.Message == nil {
		return
	}
	if isSharedChat(q.Message.Chat) || q.Message.Chat.ID != q.From.ID {
		h.answerCallback(q, "Review skills in a private chat with the bot.")
		return
	}
	scope, ok := h.resolveScope(q.From)
	if !ok {
		return
	}
	claims := &types.Claims{UserID: h.principalFor(ctx, q.From), Scope: scope}
	claims.Roles = h.rolesFor(claims.UserID)
	if h.cfg.CommandAuthorizer == nil || !h.cfg.CommandAuthorizer.AllowsCommand(ctx, claims, "learned") {
		h.answerCallback(q, "You are not authorised to review skills.")
		return
	}
	parts := strings.SplitN(q.Data, ":", 3)
	if len(parts) != 3 {
		return
	}
	verb, id := parts[1], parts[2]
	var err error
	if verb == "list" {
		page, e := strconv.Atoi(id)
		if e != nil {
			return
		}
		err = h.listLearned(ctx, q.Message.Chat.ID, claims, page)
	} else {
		err = h.decideLearnedCallback(ctx, q, claims, verb, id)
	}
	if err != nil {
		h.sendText(q.Message.Chat.ID, "Review could not be completed: "+err.Error()+". Reopen /learned to see the current state.")
	}
}

func (h *TelegramHandler) decideLearnedCallback(ctx context.Context, q *tgCallbackQuery, claims *types.Claims, verb, id string) error {
	p, err := h.cfg.Prompts.Get(id)
	if err != nil {
		return err
	}
	if p.Channel != "telegram" || p.ChannelID != strconv.FormatInt(q.Message.Chat.ID, 10) || p.RaisedFor != claims.UserID {
		return fmt.Errorf("this review was not addressed to you")
	}
	if p.Decision != PromptPending {
		return fmt.Errorf("this review has already been closed")
	}
	if !time.Now().Before(p.ExpiresAt) {
		return ErrPromptExpired
	}
	var target learnedTarget
	if err := json.Unmarshal([]byte(p.Resource), &target); err != nil {
		return err
	}
	if target.UserID != claims.UserID || target.ID == "" || target.Revision == 0 {
		return fmt.Errorf("invalid review target")
	}
	if verb == "show" && p.Action == "learned:open" {
		return h.showLearned(ctx, q.Message.Chat.ID, claims, target.ID)
	}
	if p.Action != learnedReviewAction {
		return fmt.Errorf("not a decision prompt")
	}
	if verb != "approve" && verb != "deny" && verb != "later" {
		return fmt.Errorf("unknown review action")
	}
	// Revalidate ownership and authority even for Later; closing a review is an
	// action by its owner, not a consequence of possessing the callback token.
	if _, err := h.cfg.Learned.Get(ctx, claims, target.ID); err != nil {
		return err
	}
	decision := PromptDenied
	if verb == "approve" {
		decision = PromptApproved
	}
	if err := h.cfg.Prompts.Resolve(id, decision, PromptScopeOnce); err != nil {
		return err
	}
	if verb == "later" {
		return h.learnedPost(ctx, "sendMessage", map[string]any{"chat_id": q.Message.Chat.ID, "text": "Left pending. Reopen /learned when you are ready."})
	}
	result, err := h.cfg.Learned.Decide(ctx, claims, target.ID, target.Revision, target.Digest, verb == "approve")
	if err != nil {
		return err
	}
	return h.learnedPost(ctx, "sendMessage", map[string]any{"chat_id": q.Message.Chat.ID, "text": result})
}

func (h *TelegramHandler) learnedPost(ctx context.Context, method string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return h.learnedRequest(ctx, method, "application/json", bytes.NewReader(raw))
}

func (h *TelegramHandler) learnedRequest(ctx context.Context, method, contentType string, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/bot%s/%s", h.base, h.cfg.BotToken, method), body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("review delivery failed")
	}
	defer func() { _ = resp.Body.Close() }()
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, telegramAPIResponseMaxBytes)).Decode(&result); err != nil {
		return fmt.Errorf("invalid Telegram response")
	}
	if resp.StatusCode != http.StatusOK || !result.OK {
		return fmt.Errorf("telegram did not accept the review message")
	}
	return nil
}

func (h *TelegramHandler) sendLearnedDocument(ctx context.Context, chatID int64, text string) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return err
	}
	if err := writer.WriteField("caption", "Complete skill review, including instructions and bundled files."); err != nil {
		return err
	}
	part, err := writer.CreateFormFile("document", "skill-review.txt")
	if err != nil {
		return err
	}
	if _, err := io.WriteString(part, text); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return h.learnedRequest(ctx, "sendDocument", writer.FormDataContentType(), &body)
}
