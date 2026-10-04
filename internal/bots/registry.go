package bots

import (
	"errors"
	"strings"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

var ErrNotFound = errors.New("bots: no such bot")
var ErrInboxNotFound = errors.New("inbox: no such item")
var ErrInboxFull = errors.New("inbox: recipient's queue is full")

const ChiefBotID = "chief"
const MaxInboxRecentItems = 1000

type InboxFilter struct {
	Statuses []pb.InboxStatus
	Kinds    []pb.InboxKind
	Limit    int
}

func MayModify(rec *pb.BotRecord, principal string) bool {
	return principal != "" && strings.TrimSpace(rec.GetOwner()) != "" && strings.TrimSpace(rec.GetOwner()) == strings.TrimSpace(principal) && strings.TrimSpace(principal) != ""
}
func MayModifyGroup(rec *pb.GroupRecord, principal string) bool {
	return principal != "" && strings.TrimSpace(rec.GetOwner()) != "" && strings.TrimSpace(rec.GetOwner()) == strings.TrimSpace(principal) && strings.TrimSpace(principal) != ""
}
func InboxStatusName(s pb.InboxStatus) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "INBOX_STATUS_"))
}
func InboxKindName(k pb.InboxKind) string {
	return strings.ToLower(strings.TrimPrefix(k.String(), "INBOX_KIND_"))
}

// MaxNotificationPage bounds owner-facing outbox pages across transports.
const MaxNotificationPage = 256
