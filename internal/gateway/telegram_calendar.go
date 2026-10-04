package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/google/calendar"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type calendarUI struct {
	Action   string          `json:"action"`
	Request  CalendarRequest `json:"request"`
	Page     int             `json:"page"`
	Selector string          `json:"selector"`
}
type calendarButton struct {
	Label  string
	Target calendarUI
}

func calendarDecode(value any, dst any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}
func (h *TelegramHandler) calendarPost(ctx context.Context, chat int64, claims *types.Claims, text string, buttons []calendarButton) error {
	if h.cfg.Prompts == nil {
		return errors.New("calendar: private controls unavailable")
	}
	if len(text) > 3500 {
		return errors.New("calendar: details exceed safe review size")
	}
	rows := [][]map[string]string{}
	for _, button := range buttons {
		raw, err := json.Marshal(button.Target)
		if err != nil {
			return err
		}
		p, err := h.cfg.Prompts.Create(NewPrompt{Action: "calendar:ui", Resource: string(raw), Channel: "telegram", ChannelID: strconv.FormatInt(chat, 10), RaisedFor: claims.UserID, TTL: 15 * time.Minute})
		if err != nil {
			return err
		}
		rows = append(rows, []map[string]string{{"text": button.Label, "callback_data": "cal:do:" + p.ID}})
	}
	return h.learnedPost(ctx, "sendMessage", map[string]any{"chat_id": chat, "text": text, "reply_markup": map[string]any{"inline_keyboard": rows}})
}
func (h *TelegramHandler) registerCalendarsCommand() {
	h.commands.Register(&Command{Name: "calendars", Summary: "manage calendar accounts, nicknames and preferences privately", Handler: func(ctx context.Context, req CommandRequest) (string, error) {
		if req.Shared {
			return "", errors.New("calendar: use a private conversation")
		}
		chat, err := strconv.ParseInt(req.Session.ChannelID, 10, 64)
		if err != nil {
			return "", err
		}
		err = h.calendarUI(ctx, chat, req.Claims, calendarUI{Action: "list"})
		return "", err
	}})
}
func (h *TelegramHandler) handleCalendarCallback(ctx context.Context, q *tgCallbackQuery) {
	if h.cfg.Calendar == nil || h.cfg.Prompts == nil || q.From == nil || q.Message == nil {
		return
	}
	if isSharedChat(q.Message.Chat) || q.From.ID != q.Message.Chat.ID {
		return
	}
	scope, ok := h.resolveScope(q.From)
	if !ok {
		return
	}
	claims := &types.Claims{UserID: h.principalFor(ctx, q.From), Scope: scope}
	claims.Roles = h.rolesFor(claims.UserID)
	if h.cfg.CommandAuthorizer == nil || !h.cfg.CommandAuthorizer.AllowsCommand(ctx, claims, "calendars") {
		return
	}
	parts := strings.SplitN(q.Data, ":", 3)
	if len(parts) != 3 || parts[1] != "do" {
		return
	}
	p, err := h.cfg.Prompts.Get(parts[2])
	if err != nil {
		return
	}
	if p.Action != "calendar:ui" || p.Channel != "telegram" || p.ChannelID != strconv.FormatInt(q.Message.Chat.ID, 10) || p.RaisedFor != claims.UserID || p.Decision != PromptPending || !p.ExpiresAt.After(time.Now()) {
		return
	}
	var target calendarUI
	if err = json.Unmarshal([]byte(p.Resource), &target); err != nil {
		return
	}
	if err = h.cfg.Prompts.Resolve(p.ID, PromptApproved, PromptScopeOnce); err != nil {
		return
	}
	if err = h.calendarUI(ctx, q.Message.Chat.ID, claims, target); err != nil {
		h.sendText(q.Message.Chat.ID, "Calendar action could not complete: "+err.Error()+". Reopen /calendars to review the current state.")
	}
}
func (h *TelegramHandler) calendarUI(ctx context.Context, chat int64, claims *types.Claims, target calendarUI) error {
	call := func(req CalendarRequest, dst any) error {
		out, err := h.cfg.Calendar(ctx, claims, req)
		if err != nil {
			return err
		}
		return calendarDecode(out, dst)
	}
	back := calendarButton{"Your calendars", calendarUI{Action: "list"}}
	switch target.Action {
	case "list", "show":
		return h.calendarInventoryUI(ctx, chat, claims, target)
	case "change":
		var p calendar.SettingsPreview
		req := target.Request
		req.Operation = "settings_prepare"
		if err := call(req, &p); err != nil {
			return err
		}
		if p.Confirm {
			return h.calendarPost(ctx, chat, claims, p.Summary, []calendarButton{{"Confirm this change", calendarUI{Action: "apply", Request: CalendarRequest{Operation: "settings_apply", ID: p.ID}}}, back})
		}
		// The trusted human route applies only the exact prepared operation.
		target = calendarUI{Action: "apply", Request: CalendarRequest{Operation: "settings_apply", ID: p.ID}}
		return h.calendarUI(ctx, chat, claims, target)
	case "apply":
		var result calendar.SettingsResult
		if err := call(target.Request, &result); err != nil {
			return err
		}
		buttons := []calendarButton{back}
		if result.Undo != "" {
			buttons = append(buttons, calendarButton{"Undo", calendarUI{Action: "change", Request: CalendarRequest{Change: calendar.SettingsChange{Operation: "undo", Value: result.Undo}}}})
		}
		return h.calendarPost(ctx, chat, claims, "Calendar settings updated.", buttons)
	case "connect":
		var result struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		}
		if err := call(CalendarRequest{Operation: "connect", Write: target.Request.Write}, &result); err != nil {
			return err
		}
		return h.calendarPost(ctx, chat, claims, "Open this link to authorize your Google account, then choose calendars here:\n"+result.URL, []calendarButton{{"Choose calendars", calendarUI{Action: "select", Request: CalendarRequest{ID: result.ID, Calendars: map[string]calendar.Permission{}}}}, back})
	case "select", "review":
		return h.calendarSelectionUI(ctx, chat, claims, target)
	case "activate":
		var result calendar.ConnectionView
		if err := call(target.Request, &result); err != nil {
			return err
		}
		return h.calendarPost(ctx, chat, claims, "Connected "+result.Email+". You can now give calendars nicknames and choose their purposes in chat.", []calendarButton{back})
	default:
		return errors.New("unknown calendar action")
	}
}

func (h *TelegramHandler) calendarInventoryUI(ctx context.Context, chat int64, claims *types.Claims, target calendarUI) error {
	call := func(req CalendarRequest, dst any) error {
		out, err := h.cfg.Calendar(ctx, claims, req)
		if err != nil {
			return err
		}
		return calendarDecode(out, dst)
	}
	back := calendarButton{"Your calendars", calendarUI{Action: "list"}}
	var in calendar.Inventory
	if err := call(CalendarRequest{Operation: "inventory"}, &in); err != nil {
		return err
	}
	if target.Action == "show" {
		for _, c := range in.Calendars {
			if c.ID != target.Selector {
				continue
			}
			text := fmt.Sprintf("%s\nAccount: %s\nRead: %t; write: %t\nAgenda: %t; availability: %t\nDefault for: %s\n\nTo rename or choose a purpose, say ‘Call this calendar Family’ or ‘Use Family for school events’. You can also set your time zone in chat.", c.Nickname, c.Account, c.Permission.Read, c.Permission.Write, c.Agenda, c.Availability, strings.Join(c.Defaults, ", "))
			buttons := []calendarButton{}
			for _, op := range []string{"agenda", "availability"} {
				enabled := !c.Agenda
				if op == "availability" {
					enabled = !c.Availability
				}
				change := calendar.SettingsChange{Operation: op, Calendar: c.ID, Enabled: &enabled, Revision: &in.Revision}
				buttons = append(buttons, calendarButton{fmt.Sprintf("%s: %t", op, enabled), calendarUI{Action: "change", Request: CalendarRequest{Change: change}}})
			}
			for _, grant := range []struct {
				label string
				p     calendar.Permission
			}{{"Read only", calendar.Permission{Read: true}}, {"Read and write", calendar.Permission{Read: true, Write: true}}, {"Disable access", calendar.Permission{}}} {
				buttons = append(buttons, calendarButton{grant.label, calendarUI{Action: "change", Request: CalendarRequest{Change: calendar.SettingsChange{Operation: "access", Calendar: c.ID, Permission: &grant.p, Revision: &in.Revision}}}})
			}
			buttons = append(buttons, calendarButton{"Use as general default", calendarUI{Action: "change", Request: CalendarRequest{Change: calendar.SettingsChange{Operation: "default", Calendar: c.ID, Value: "general", Revision: &in.Revision}}}}, calendarButton{"Disconnect account", calendarUI{Action: "change", Request: CalendarRequest{Change: calendar.SettingsChange{Operation: "disconnect", Calendar: c.ID, Revision: &in.Revision}}}}, back)
			return h.calendarPost(ctx, chat, claims, text, buttons)
		}
		return errors.New("calendar no longer available")
	}
	page := target.Page
	if page < 0 || page*5 > len(in.Calendars) {
		return errors.New("invalid page")
	}
	text := "Your calendars\nTime zone: " + in.TimeZone + "\nManage preferences by chatting, or choose a calendar below.\n"
	buttons := []calendarButton{}
	for i := page * 5; i < len(in.Calendars) && i < (page+1)*5; i++ {
		c := in.Calendars[i]
		text += fmt.Sprintf("\n%s — %s (read=%t, write=%t)", c.Nickname, c.Account, c.Permission.Read, c.Permission.Write)
		buttons = append(buttons, calendarButton{c.Nickname, calendarUI{Action: "show", Selector: c.ID}})
	}
	if page > 0 {
		buttons = append(buttons, calendarButton{"Previous", calendarUI{Action: "list", Page: page - 1}})
	}
	if (page+1)*5 < len(in.Calendars) {
		buttons = append(buttons, calendarButton{"Next", calendarUI{Action: "list", Page: page + 1}})
	}
	buttons = append(buttons, calendarButton{"Connect Google (read)", calendarUI{Action: "connect"}}, calendarButton{"Connect Google (read/write)", calendarUI{Action: "connect", Request: CalendarRequest{Write: true}}})
	return h.calendarPost(ctx, chat, claims, text, buttons)
}

func (h *TelegramHandler) calendarSelectionUI(ctx context.Context, chat int64, claims *types.Claims, target calendarUI) error {
	call := func(req CalendarRequest, dst any) error {
		out, err := h.cfg.Calendar(ctx, claims, req)
		if err != nil {
			return err
		}
		return calendarDecode(out, dst)
	}
	back := calendarButton{"Your calendars", calendarUI{Action: "list"}}
	var pending calendar.PendingView
	if err := call(CalendarRequest{Operation: "pending", ID: target.Request.ID}, &pending); err != nil {
		return err
	}
	if target.Request.Calendars == nil {
		target.Request.Calendars = map[string]calendar.Permission{}
	}
	target.Request.Email = pending.Email
	if target.Action == "review" {
		text := "Connect account: " + pending.Email + "\nSelected calendars:\n"
		for _, c := range pending.Calendars {
			if p, ok := target.Request.Calendars[c.ID]; ok {
				text += fmt.Sprintf("%s [%s]: read=%t write=%t\n", c.Name, c.ID, p.Read, p.Write)
			}
		}
		if len(target.Request.Calendars) == 0 {
			return errors.New("select at least one calendar")
		}
		req := target.Request
		req.Operation = "activate"
		return h.calendarPost(ctx, chat, claims, text, []calendarButton{{"Confirm selected calendars", calendarUI{Action: "activate", Request: req}}, back})
	}
	page := target.Page
	if page < 0 || page*5 > len(pending.Calendars) {
		return errors.New("invalid page")
	}
	text := "Account: " + pending.Email + "\nTap to cycle each calendar: off → read → read/write → off. Review before connecting.\n"
	buttons := []calendarButton{}
	for i := page * 5; i < len(pending.Calendars) && i < (page+1)*5; i++ {
		c := pending.Calendars[i]
		p := target.Request.Calendars[c.ID]
		label := "off"
		if p.Read {
			label = "read"
		}
		if p.Write {
			label = "read/write"
		}
		text += fmt.Sprintf("\n%s: %s", c.Name, label)
		next := map[string]calendar.Permission{}
		for id, perm := range target.Request.Calendars {
			next[id] = perm
		}
		switch {
		case !p.Read && !p.Write:
			next[c.ID] = calendar.Permission{Read: true}
		case !p.Write && pending.Write && (c.AccessRole == "owner" || c.AccessRole == "writer"):
			next[c.ID] = calendar.Permission{Read: true, Write: true}
		default:
			delete(next, c.ID)
		}
		req := target.Request
		req.Calendars = next
		buttons = append(buttons, calendarButton{c.Name + ": " + label, calendarUI{Action: "select", Request: req, Page: page}})
	}
	if page > 0 {
		buttons = append(buttons, calendarButton{"Previous", calendarUI{Action: "select", Request: target.Request, Page: page - 1}})
	}
	if (page+1)*5 < len(pending.Calendars) {
		buttons = append(buttons, calendarButton{"Next", calendarUI{Action: "select", Request: target.Request, Page: page + 1}})
	}
	buttons = append(buttons, calendarButton{"Review selection", calendarUI{Action: "review", Request: target.Request}}, back)
	return h.calendarPost(ctx, chat, claims, text, buttons)
}
