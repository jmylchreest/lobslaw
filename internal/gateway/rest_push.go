package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/internal/push"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func (s *Server) initPush() error {
	if s.cfg.LoginSessionFile == "" || !s.consoleEnabled() {
		return nil
	}
	service, err := push.Open(filepath.Join(filepath.Dir(s.cfg.LoginSessionFile), "web-push.json"), egress.For("gateway/web-push").HTTPClient())
	if err != nil {
		return err
	}
	s.push = service
	return nil
}

func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	authn, err := s.authenticateRequest(r)
	if err != nil {
		s.jsonErr(w, 401, "sign in to manage notifications")
		return
	}
	user, ok := s.enrolledUser(r.Context(), authn.Claims.UserID)
	if !ok {
		s.jsonErr(w, 403, "enrolled user required")
		return
	}
	if s.push == nil {
		s.jsonErr(w, 503, "web push requires persistent storage on the web node")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(map[string]any{"public_key": s.push.PublicKey()})
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		s.jsonErr(w, 405, "method not allowed")
		return
	}
	if err := s.checkCookieCSRF(r, authn); err != nil {
		s.jsonErr(w, 403, err.Error())
		return
	}
	var sub webpush.Subscription
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&sub); err != nil {
		s.jsonErr(w, 400, "invalid subscription")
		return
	}
	owner := canonicalUserPrincipal(user.ID)
	if r.Method == http.MethodDelete {
		err = s.push.Remove(owner, sub.Endpoint)
	} else {
		expires := time.Now().Add(DefaultLoginSessionTTL)
		if authn.FromCookie {
			sess := s.logins.get(authn.LoginID)
			if sess == nil {
				s.jsonErr(w, 401, "session expired")
				return
			}
			expires = sess.ExpiresAt
		} else if !authn.Claims.ExpiresAt.IsZero() && authn.Claims.ExpiresAt.Before(expires) {
			expires = authn.Claims.ExpiresAt
		}
		err = s.push.Subscribe(owner, authn.LoginID, expires, sub)
	}
	if err != nil {
		s.jsonErr(w, 400, err.Error())
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (s *Server) runPush(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		leg, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := s.push.Dispatch(leg, s.pushEvents)
		cancel()
		if err != nil && ctx.Err() == nil {
			s.log.Warn("web push delivery deferred", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) pushEvents(ctx context.Context, owner string) ([]push.Event, error) {
	user, ok := s.enrolledUser(ctx, strings.TrimPrefix(owner, "user:"))
	if !ok {
		return nil, nil
	}
	claims := &types.Claims{UserID: user.ID, Roles: user.Roles, Scope: s.cfg.DefaultScope}
	var bots []*pb.ConsoleBot
	if s.cfg.RemoteConsole != nil {
		reply, err := s.cfg.RemoteConsole.QueryConsole(ctx, &pb.QueryConsoleRequest{Identity: consoleIdentity(claims), Query: &pb.QueryConsoleRequest_Bots{Bots: &pb.ConsoleEmpty{}}})
		if err != nil {
			return nil, err
		}
		bots = reply.GetBots().GetBots()
	} else if s.cfg.Bots != nil {
		records, err := s.cfg.Bots.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, b := range records {
			if b.Owner == owner {
				bots = append(bots, &pb.ConsoleBot{Id: b.Id, DisplayName: b.DisplayName})
			}
		}
	}
	var events []push.Event
	for _, bot := range bots {
		var items []*pb.ConsoleInboxItem
		if s.cfg.RemoteConsole != nil {
			reply, err := s.cfg.RemoteConsole.QueryConsole(ctx, &pb.QueryConsoleRequest{Identity: consoleIdentity(claims), Query: &pb.QueryConsoleRequest_Inbox{Inbox: &pb.ConsoleList{Id: bot.Id, Status: "all", Limit: 200}}})
			if err != nil {
				return nil, err
			}
			items = reply.GetInbox().GetItems()
		} else if s.cfg.Inbox != nil {
			var err error
			items, err = s.cfg.Inbox.Recent(ctx, bot.Id, 200)
			if err != nil {
				return nil, err
			}
		}
		for _, item := range items {
			if item.RequestedBy != owner {
				continue
			}
			var task *pb.TaskApprovalRecord
			if item.Status == "waiting" && item.TaskId != "" {
				query := &pb.GetTaskApprovalRequest{Id: item.TaskId, Owner: owner}
				if s.cfg.RemoteConsole != nil {
					reply, err := s.cfg.RemoteConsole.QueryConsole(ctx, &pb.QueryConsoleRequest{Identity: consoleIdentity(claims), Query: &pb.QueryConsoleRequest_TaskApproval{TaskApproval: query}})
					if err != nil {
						return nil, err
					}
					task = reply.GetTaskApproval().GetRecord()
				} else if s.cfg.TaskApprovals != nil {
					reply, err := s.cfg.TaskApprovals.GetTaskApproval(ctx, query)
					if err != nil {
						return nil, err
					}
					task = reply.GetRecord()
				}
			}
			if event, ok := inboxPushEvent(bot, item, task); ok {
				events = append(events, event)
			}
		}
	}
	return events, nil
}

func inboxPushEvent(bot *pb.ConsoleBot, item *pb.ConsoleInboxItem, task *pb.TaskApprovalRecord) (push.Event, bool) {
	name := bot.DisplayName
	if name == "" {
		name = bot.Id
	}
	e := push.Event{ID: fmt.Sprintf("%s:%s:%d", item.Id, item.Status, item.Revision), Title: name, URL: "/bots/" + url.PathEscape(bot.Id), Icon: "/__agent-icons/" + url.PathEscape(bot.Id)}
	e.At, _ = time.Parse(time.RFC3339Nano, item.CompletedAt)
	if e.At.IsZero() {
		e.At, _ = time.Parse(time.RFC3339Nano, item.CreatedAt)
	}
	switch {
	case item.Status == "waiting":
		// Admission marks an inbox item WAITING while its durable task is
		// RUNNING too. Only the task record can establish a need for attention.
		if task == nil || (task.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_WAITING && task.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_OUTCOME_UNKNOWN) {
			return push.Event{}, false
		}
		e.ID = fmt.Sprintf("%s:attention:%d", item.Id, task.Revision)
		e.Attention = true
		e.At = time.Now()
		e.Title += " needs your attention"
		e.Body = item.Subject
		if item.TaskId != "" {
			e.URL = "/approvals/" + url.PathEscape(item.TaskId)
		}
	case item.Status == "failed":
		if strings.Contains(item.Error, "TASK_APPROVAL_STATE_CANCELLED") || strings.Contains(item.Error, "TASK_APPROVAL_STATE_DENIED") {
			return push.Event{}, false
		}
		e.Attention = true
		e.Title += " — task needs attention"
		e.Body = item.Error
		if item.TaskId != "" {
			e.URL = "/approvals/" + url.PathEscape(item.TaskId)
		}
	case item.Status == "done" && strings.HasPrefix(item.Sender, "notify:"):
		if parts := strings.Split(item.Sender, ":"); len(parts) == 3 {
			expires, err := strconv.ParseInt(parts[2], 10, 64)
			if err != nil || time.Now().After(time.Unix(expires, 0)) {
				return push.Event{}, false
			}
			e.Expires = time.Unix(expires, 0)
		}
		e.Body = item.Result
		if e.Body == "" {
			e.Body = item.Body
		}
	case item.Status == "done" && strings.HasPrefix(item.Sender, "schedule:") && strings.HasSuffix(item.Sender, ":always"):
		e.Title += " — routine complete"
		e.Body = item.Result
	default:
		return push.Event{}, false
	}
	if len([]rune(e.Body)) > 300 {
		e.Body = string([]rune(e.Body)[:300]) + "…"
	}
	return e, true
}
