package console

import (
	"context"
	"strings"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type SessionBrowser interface {
	ListFiltered(ctx context.Context, channel, userID string) ([]*lobslawv1.SessionRecord, error)
	LoadMessages(ctx context.Context, id string) ([]*lobslawv1.SessionMessage, error)
}

type SessionView struct {
	ID        string `json:"id"`
	Channel   string `json:"channel"`
	ChannelID string `json:"channel_id"`
	Title     string `json:"title,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	Messages  uint64 `json:"messages"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

type MessageView struct {
	Seq       uint64 `json:"seq"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	ToolCalls int    `json:"tool_calls,omitempty"`
	TurnID    string `json:"turn_id,omitempty"`
}

func SessionViewOf(rec *lobslawv1.SessionRecord) SessionView {
	out := SessionView{
		ID:        rec.GetId(),
		Channel:   rec.GetChannel(),
		ChannelID: rec.GetChannelId(),
		Title:     rec.GetTitle(),
		UserID:    rec.GetUserId(),
		Messages:  rec.GetNextSeq(),
	}
	if ts := rec.GetUpdatedAt(); ts != nil {
		out.UpdatedAt = ts.AsTime().UTC().Format(rfc3339)
	}
	return out
}

type RoutineAPI interface {
	TasksForOwner(owner string) ([]*lobslawv1.ScheduledTaskRecord, error)
}

// MemoryAPI is a bot's own memory, read-only.
//
// Read-only deliberately. Editing what a bot remembers from a console
// is a different feature with a different risk profile, and shipping
// the viewer first answers the question people actually have — "why
// did it say that" — without offering a way to rewrite the evidence.
type MemoryAPI interface {
	RecordsForOwner(ctx context.Context, owner string, limit int) ([]MemoryRecordView, int, error)
}

// MemoryRecordView is one remembered thing, flattened.
type MemoryRecordView struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Text      string   `json:"text"`
	Tags      []string `json:"tags,omitempty"`
	Scope     string   `json:"scope,omitempty"`
	CreatedAt string   `json:"created_at,omitempty"`
}

type RoutineView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Handler  string `json:"handler_ref"`
	Enabled  bool   `json:"enabled"`
	LastRun  string `json:"last_run,omitempty"`
	NextRun  string `json:"next_run,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
}

func (s *Service) Sessions(ctx context.Context, claims *types.Claims, botID string) ([]SessionView, error) {
	if _, err := s.ownedBot(ctx, claims, botID); err != nil {
		return nil, err
	}
	if s.cfg.Transcripts == nil {
		return nil, ErrUnavailable
	}
	records, err := s.cfg.Transcripts.ListFiltered(ctx, "bot", "")
	if err != nil {
		return nil, err
	}
	out := []SessionView{}
	for _, rec := range records {
		if rec.ChannelId == botID || strings.HasPrefix(rec.ChannelId, botID+".") {
			out = append(out, SessionViewOf(rec))
		}
	}
	return out, nil
}
func (s *Service) Transcript(ctx context.Context, claims *types.Claims, id string) ([]MessageView, error) {
	owner, err := caller(ctx, claims)
	if err != nil {
		return nil, err
	}
	if s.cfg.Transcripts == nil {
		return nil, ErrUnavailable
	}
	records, err := s.cfg.Transcripts.ListFiltered(ctx, "", "")
	if err != nil {
		return nil, err
	}
	allowed := false
	for _, rec := range records {
		if rec.Id != id {
			continue
		}
		allowed = Principal(rec.UserId) != "" && Principal(rec.UserId) == owner
		if rec.Channel == "bot" {
			botID, _, _ := strings.Cut(rec.ChannelId, ".")
			_, err := s.ownedBot(ctx, claims, botID)
			allowed = err == nil
		}
		break
	}
	if !allowed {
		return nil, ErrForbidden
	}
	messages, err := s.cfg.Transcripts.LoadMessages(ctx, id)
	if err != nil {
		return nil, err
	}
	out := []MessageView{}
	for _, m := range messages {
		out = append(out, MessageView{Seq: m.Seq, Role: m.Role, Content: m.Content, ToolCalls: len(m.ToolCalls), TurnID: m.TurnId})
	}
	return out, nil
}
func (s *Service) Routines(ctx context.Context, claims *types.Claims, botID string) ([]RoutineView, error) {
	bot, err := s.ownedBot(ctx, claims, botID)
	if err != nil {
		return nil, err
	}
	if s.cfg.Routines == nil {
		return nil, ErrUnavailable
	}
	tasks, err := s.cfg.Routines.TasksForOwner("bot:" + botID)
	if err != nil {
		return nil, err
	}
	out := []RoutineView{}
	for _, t := range tasks {
		if owner := t.Params["requested_by"]; owner != "" && bot.Owner != owner {
			continue
		}
		row := RoutineView{ID: t.Id, Name: t.Name, Schedule: t.Schedule, Handler: t.HandlerRef, Enabled: t.Enabled, Prompt: t.Params["prompt"]}
		if t.LastRun != nil {
			row.LastRun = t.LastRun.AsTime().UTC().Format(rfc3339)
		}
		if t.NextRun != nil {
			row.NextRun = t.NextRun.AsTime().UTC().Format(rfc3339)
		}
		out = append(out, row)
	}
	return out, nil
}
func (s *Service) Memory(ctx context.Context, claims *types.Claims, botID string) ([]MemoryRecordView, int, error) {
	if _, err := s.ownedBot(ctx, claims, botID); err != nil {
		return nil, 0, err
	}
	if s.cfg.Memory == nil {
		return nil, 0, ErrUnavailable
	}
	rows, total, err := s.cfg.Memory.RecordsForOwner(ctx, "bot:"+botID, 100)
	if rows == nil {
		rows = []MemoryRecordView{}
	}
	return rows, total, err
}
