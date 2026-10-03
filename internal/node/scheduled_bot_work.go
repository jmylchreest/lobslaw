package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/scheduler"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/promptgen"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/oklog/ulid/v2"
)

// Scheduling admits work; the ordinary durable task worker executes it. A slow
// model cannot outlive the scheduler lease, hide an approval or lose its reply.
func (n *Node) dispatchBotSchedule(ctx context.Context, task *pb.ScheduledTaskRecord) error {
	if n.inboxSvc == nil {
		return errors.New("scheduled bot work requires an inbox")
	}
	botID := strings.TrimPrefix(task.Owner, "bot:")
	owner := task.Params["requested_by"]
	if owner == "" {
		return errors.New("routine has no saved human owner; recreate it from an authenticated bot conversation")
	}
	enrolled := false
	for _, user := range n.cfg.Users {
		if identity.User(user.ID).String() == owner {
			enrolled = true
			break
		}
	}
	if !enrolled {
		return errors.New("routine owner is no longer enrolled")
	}
	authority, err := n.resolveTeamAuthority(ctx, owner, task.Owner)
	if err != nil {
		return err
	}
	var roles []string
	if err := json.Unmarshal([]byte(task.Params["roles"]), &roles); err != nil {
		return errors.New("routine has invalid saved authority")
	}
	authority.Claims.Roles = slices.DeleteFunc(roles, func(role string) bool { return !slices.Contains(authority.Claims.Roles, role) })
	authority.Claims.Scope = task.Params["scope"]
	at := task.GetNextRun().AsTime()
	if task.NextRun == nil {
		parsed, err := scheduler.ParseAgentSchedule(task.Schedule)
		if err != nil {
			return err
		}
		anchor := task.GetCreatedAt().AsTime()
		if task.LastRun != nil {
			anchor = task.LastRun.AsTime()
		}
		at = parsed.Next(anchor)
	}
	hash := sha256.Sum256([]byte(task.Id + ":" + at.UTC().Format(time.RFC3339Nano)))
	id, err := ulid.New(ulid.Timestamp(at), bytes.NewReader(hash[:]))
	if err != nil {
		return err
	}
	previous := ""
	if last := task.Params["last_item_id"]; last != "" && last != id.String() {
		item, err := n.inboxSvc.Get(ctx, botID, last)
		if err != nil && !errors.Is(err, memory.ErrInboxNotFound) {
			return err
		}
		if item != nil {
			switch item.Status {
			case pb.InboxStatus_INBOX_STATUS_PENDING, pb.InboxStatus_INBOX_STATUS_CLAIMED, pb.InboxStatus_INBOX_STATUS_WAITING:
				task.Params["last_dispatch_status"] = "skipped: previous occurrence is still active"
				return nil
			}
			previous = item.Result
			if item.Error != "" {
				previous += "\nPrevious error: " + item.Error
			}
			if len(previous) > 2000 {
				previous = previous[:2000] + "… (read the previous inbox item for full evidence)"
			}
		}
	}
	mode := task.Params["notify_on"]
	if mode != "always" && mode != "never" {
		mode = "match"
	}
	body := fmt.Sprintf("Execute occurrence of routine %s (%s). Do not recreate the schedule.\n\n%s\n\nSave progress for the next run with schedule_update(id=%s, checkpoint=...). Previous inbox item: %s.\n", task.Id, task.Name, task.Params["prompt"], task.Id, task.Params["last_item_id"])
	body += "Notification policy: " + mode + ". "
	if mode == "always" {
		body += "The final outcome will be announced automatically; do not duplicate it with notify.\n"
	} else if mode == "never" {
		body += "Keep routine outcomes quiet.\n"
	} else {
		body += "Use notify only for a meaningful change or when your owner needs attention.\n"
	}
	body += promptgen.WrapContext([]promptgen.ContextBlock{{Source: "routine-checkpoint", Trust: promptgen.TrustUntrusted, Content: task.Params["checkpoint"]}, {Source: "previous-occurrence", Trust: promptgen.TrustUntrusted, Content: previous}})
	// Retain the intended occurrence even when a Raft acknowledgement is lost.
	// The next tick checks that item before admitting overlapping work.
	task.Params["last_item_id"] = id.String()
	item, err := n.inboxSvc.PostOnce(ctx, &pb.BotInboxItem{
		Recipient: botID, Sender: "schedule:" + task.Id + ":" + mode, RequestedBy: owner,
		TaskClaims: turn.ClaimsToProto(authority.Claims), Kind: pb.InboxKind_INBOX_KIND_TASK,
		Subject: "Routine: " + task.Name, Body: body,
	}, id.String(), at)
	if err != nil {
		return err
	}
	task.Params["last_item_id"] = item.Id
	task.Params["last_dispatch_status"] = "queued"
	n.wakeInboxDrain()
	return nil
}

func (n *Node) reportScheduleError(ctx context.Context, task *pb.ScheduledTaskRecord, cause error) {
	if n.botSvc == nil || n.inboxSvc == nil || task.Params["last_error"] == cause.Error() {
		return
	}
	bot, err := n.botSvc.Get(ctx, strings.TrimPrefix(task.Owner, "bot:"))
	if err != nil || bot.Owner != task.Params["requested_by"] {
		return
	}
	message := fmt.Sprintf("Routine %q could not queue its work: %s. Inspect it with schedule_get (id %s), or pause it with schedule_update.", task.Name, cause, task.Id)
	if _, err := n.inboxSvc.Journal(ctx, &pb.BotInboxItem{Recipient: bot.Id, Sender: "notify:scheduler", RequestedBy: bot.Owner, Kind: pb.InboxKind_INBOX_KIND_FYI, Subject: "Routine needs attention", Body: message, Result: message}); err != nil {
		n.log.Warn("scheduler: could not deliver dispatch failure", "task_id", task.Id, "err", err)
	}
}
