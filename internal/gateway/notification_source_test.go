package gateway

import (
	"fmt"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestNotificationSourceFindsLateFailureLocallyAndThroughPeer(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(t.TempDir(), "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	put := func(item *pb.BotInboxItem) {
		t.Helper()
		raw, err := proto.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(memory.BucketBotInbox, "worker:"+item.Id, raw); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i <= 200; i++ {
		put(&pb.BotInboxItem{Id: fmt.Sprintf("%026d", i), Recipient: "worker", RequestedBy: "user:alice", Sender: "bot:worker", Status: pb.InboxStatus_INBOX_STATUS_DONE, CreatedAt: timestamppb.Now()})
	}
	put(&pb.BotInboxItem{Id: fmt.Sprintf("%026d", 0), Recipient: "worker", RequestedBy: "user:alice", Status: pb.InboxStatus_INBOX_STATUS_FAILED, Error: "New failure on old work", CompletedAt: timestamppb.Now()})
	put(&pb.BotInboxItem{Id: "previous-owner", Recipient: "worker", RequestedBy: "user:bob", Status: pb.InboxStatus_INBOX_STATUS_FAILED, Error: "Private previous-owner evidence", CompletedAt: timestamppb.Now()})
	backend := startWebREST(t, nil, func(c *RESTConfig) {
		c.Bots = &memBots{recs: map[string]*pb.BotRecord{"worker": {Id: "worker", Owner: "user:alice", Enabled: true}}}
		c.Inbox = memory.NewInboxService(nil, store, 0)
	})
	client := testConsoleClient(t, backend)
	front := startWebREST(t, nil, func(c *RESTConfig) { c.RemoteConsole = client })
	page, err := client.QueryConsole(t.Context(), &pb.QueryConsoleRequest{Identity: consoleTestIdentity(), Query: &pb.QueryConsoleRequest_NotificationCandidates{NotificationCandidates: &pb.ConsoleNotificationQuery{Bot: "worker", Limit: 20}}})
	if err != nil || len(page.GetNotificationCandidates().GetItems()) != 1 {
		t.Fatalf("peer exposed another requester's evidence: %v %v", page, err)
	}
	for _, server := range []*Server{backend, front} {
		events, err := server.notificationEvents(t.Context(), "user:alice")
		if err != nil || len(events) != 1 || events[0].Body != "New failure on old work" {
			t.Fatalf("missed late failure: %+v %v", events, err)
		}
	}
	_, err = client.QueryConsole(t.Context(), &pb.QueryConsoleRequest{Identity: &pb.ConsoleIdentity{Claims: &pb.Claims{UserId: "bob"}, Principal: "user:bob"}, Query: &pb.QueryConsoleRequest_NotificationCandidates{NotificationCandidates: &pb.ConsoleNotificationQuery{Bot: "worker", Limit: 20}}})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("foreign owner read notification index: %v", err)
	}
}
