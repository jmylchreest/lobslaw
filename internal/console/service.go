// Package console provides owned application operations shared by browser and peer transports.
package console

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/bots"
	"github.com/jmylchreest/lobslaw/internal/identity"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type BotAPI interface {
	List(ctx context.Context) ([]*lobslawv1.BotRecord, error)
	Get(ctx context.Context, id string) (*lobslawv1.BotRecord, error)
	Put(ctx context.Context, rec *lobslawv1.BotRecord, expectedRevision uint64) (*lobslawv1.BotRecord, error)
	Delete(ctx context.Context, id string) error
}

// InboxAPI is the same for the queues.
type InboxAPI interface {
	List(ctx context.Context, recipient string, f bots.InboxFilter) ([]*lobslawv1.BotInboxItem, error)
	Recent(ctx context.Context, recipient string, limit int) ([]*lobslawv1.ConsoleInboxItem, error)
	Post(ctx context.Context, item *lobslawv1.BotInboxItem) (*lobslawv1.BotInboxItem, error)
	Get(ctx context.Context, recipient, id string) (*lobslawv1.BotInboxItem, error)
	Cancel(ctx context.Context, recipient, id string) (*lobslawv1.BotInboxItem, error)
	Retry(ctx context.Context, recipient, id string) (*lobslawv1.BotInboxItem, error)
}

type GroupAPI interface {
	List(ctx context.Context) ([]*lobslawv1.GroupRecord, error)
	Get(ctx context.Context, id string) (*lobslawv1.GroupRecord, error)
	Put(ctx context.Context, rec *lobslawv1.GroupRecord, expectedRevision uint64) (*lobslawv1.GroupRecord, error)
	Delete(ctx context.Context, id string) error
}

var ErrForbidden = errors.New("that resource is not owned by this account")
var ErrUnauthenticated = errors.New("authentication required")
var ErrUnavailable = errors.New("console service unavailable")
var ErrConflict = types.ErrConflict
var ErrInvalid = errors.New("invalid console request")

type Config struct {
	Plan        PlanService
	Prompts     PromptAPI
	Tools       ToolCatalogue
	Transcripts SessionBrowser
	Routines    RoutineAPI
	Memory      MemoryAPI
	Bots        BotAPI
	Groups      GroupAPI
	Inbox       InboxAPI
	Logger      *slog.Logger
}
type Service struct {
	cfg Config
	log *slog.Logger
}

func New(cfg Config) *Service {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Service{cfg: cfg, log: log}
}
func Principal(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if strings.HasPrefix(id, identity.KindUser+":") {
		return id
	}
	return identity.User(id).String()
}
func caller(ctx context.Context, claims *types.Claims) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if claims == nil || strings.TrimSpace(claims.UserID) == "" {
		return "", ErrUnauthenticated
	}
	return Principal(claims.UserID), nil
}
func (s *Service) ownedBot(ctx context.Context, claims *types.Claims, id string) (*lobslawv1.BotRecord, error) {
	owner, err := caller(ctx, claims)
	if err != nil {
		return nil, err
	}
	if s.cfg.Bots == nil {
		return nil, ErrUnavailable
	}
	rec, err := s.cfg.Bots.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !bots.MayModify(rec, owner) {
		return nil, ErrForbidden
	}
	return rec, nil
}
func (s *Service) ownedGroup(ctx context.Context, claims *types.Claims, id string) (*lobslawv1.GroupRecord, error) {
	owner, err := caller(ctx, claims)
	if err != nil {
		return nil, err
	}
	if s.cfg.Groups == nil {
		return nil, ErrUnavailable
	}
	rec, err := s.cfg.Groups.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !bots.MayModifyGroup(rec, owner) {
		return nil, ErrForbidden
	}
	return rec, nil
}
func (s *Service) audit(claims *types.Claims, action, target, detail string) {
	s.log.Info("registry audit", "action", action, "target", target, "detail", detail, "actor", Principal(claims.UserID))
}
func (s *Service) EnsureOwnersTeam(ctx context.Context, principal string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return s.ensureOwnersTeam(ctx, principal)
}
