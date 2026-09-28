package node

import (
	"context"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/gateway"
	"github.com/jmylchreest/lobslaw/internal/memory"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func learnedConsoleClient(t *testing.T, n *Node) pb.ConsoleServiceClient {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	dir := t.TempDir()
	backendCreds := soulTestCreds(t, dir, "review-backend")
	webCreds := soulTestCreds(t, dir, "review-web")
	server := grpc.NewServer(grpc.Creds(backendCreds.ServerCreds()))
	pb.RegisterConsoleServiceServer(server, gateway.NewServer(gateway.RESTConfig{Learned: n.learnedReviews()}, nil))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///127.0.0.1", grpc.WithTransportCredentials(webCreds.ClientCreds()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return pb.NewConsoleServiceClient(conn)
}

func TestConsoleLearnedReviewTypedTransportUsesHumanService(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"user", "operator"} {
		t.Run(role, func(t *testing.T) {
			t.Parallel()
			n := reviewNode(t)
			n.botSvc = memory.NewBotService(nil, n.store)
			bot, err := proto.Marshal(&pb.BotRecord{Id: "worker", Owner: "user:alice", Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := n.store.Put(memory.BucketBots, "worker", bot); err != nil {
				t.Fatal(err)
			}
			seedRule(t, n.store, &pb.PolicyRule{Id: "learned-bob", Subject: "user:bob", Action: "command:exec", Resource: "learned", Effect: "allow", Priority: 50})
			proposal, err := n.selfTaught.Propose(t.Context(), &pb.SelfTaughtRecord{Kind: pb.SelfTaughtKind_SELF_TAUGHT_KIND_SKILL, Name: "worker-guide", Body: "private procedure", Owner: "bot:worker", Files: map[string]string{"references/private.txt": "full reference"}}, memory.ProposeIntent{})
			if err != nil {
				t.Fatal(err)
			}
			client := learnedConsoleClient(t, n)
			identity := func(user string) *pb.ConsoleIdentity {
				return &pb.ConsoleIdentity{Principal: "user:" + user, Claims: &pb.Claims{UserId: user, Roles: []string{role}}}
			}
			query := &pb.QueryConsoleRequest{Identity: identity("alice"), Query: &pb.QueryConsoleRequest_LearnedReviews{LearnedReviews: &pb.ConsoleEmpty{}}}
			list, err := client.QueryConsole(t.Context(), query)
			if err != nil || len(list.GetLearnedReviews().GetReviews()) != 1 {
				t.Fatalf("owner list: %v %v", list, err)
			}
			query.Query = &pb.QueryConsoleRequest_LearnedReview{LearnedReview: &pb.ConsoleTarget{Id: proposal.Id}}
			view, err := client.QueryConsole(t.Context(), query)
			if err != nil {
				t.Fatal(err)
			}
			review := view.GetLearnedReview()
			if review.Author != "bot:worker" || review.Body != proposal.Body || review.Files["references/private.txt"] != "full reference" || review.Digest == "" {
				t.Fatalf("inspection incomplete: %v", review)
			}
			decision := &pb.MutateConsoleRequest{Identity: identity("alice"), Operation: &pb.MutateConsoleRequest_DecideLearnedReview{DecideLearnedReview: &pb.ConsoleLearnedDecision{Id: review.Id, Revision: review.Revision, Digest: review.Digest, Approve: true}}}
			for _, user := range []string{"bob", "ungranted"} {
				listing, listErr := client.QueryConsole(t.Context(), &pb.QueryConsoleRequest{Identity: identity(user), Query: &pb.QueryConsoleRequest_LearnedReviews{LearnedReviews: &pb.ConsoleEmpty{}}})
				if user == "bob" && (listErr != nil || len(listing.GetLearnedReviews().GetReviews()) != 0) {
					t.Fatalf("cross-owner listing: %v %v", listing, listErr)
				}
				if user == "ungranted" && status.Code(listErr) != codes.PermissionDenied {
					t.Fatalf("policy denied listing: %v", listErr)
				}
				query.Identity = identity(user)
				if _, err := client.QueryConsole(t.Context(), query); status.Code(err) != codes.NotFound && status.Code(err) != codes.PermissionDenied {
					t.Fatalf("cross-owner/policy read: %v", err)
				}
				decision.Identity = identity(user)
				if _, err := client.MutateConsole(t.Context(), decision); status.Code(err) != codes.NotFound && status.Code(err) != codes.PermissionDenied {
					t.Fatalf("cross-owner/policy decision: %v", err)
				}
			}
			decision.Identity = identity("alice")
			decision.GetDecideLearnedReview().Digest = "wrong-digest"
			if _, err := client.MutateConsole(t.Context(), decision); status.Code(err) != codes.Aborted {
				t.Fatalf("digest conflict: %v", err)
			}
			decision.GetDecideLearnedReview().Digest = review.Digest
			decision.GetDecideLearnedReview().Revision++
			if _, err := client.MutateConsole(t.Context(), decision); status.Code(err) != codes.Aborted {
				t.Fatalf("revision conflict: %v", err)
			}
			decision.GetDecideLearnedReview().Revision = review.Revision
			out, err := client.MutateConsole(t.Context(), decision)
			if err != nil || !strings.Contains(out.GetLearnedDecision().GetMessage(), "installed and active") {
				t.Fatalf("activation result: %v %v", out, err)
			}
			stored, err := n.selfTaught.Get(proposal.Id)
			if err != nil || stored.Owner != "bot:worker" || stored.State != pb.SelfTaughtState_SELF_TAUGHT_STATE_ACTIVE {
				t.Fatalf("authorship/activation changed: %v %v", stored, err)
			}
			if _, err := client.MutateConsole(t.Context(), decision); status.Code(err) != codes.Aborted {
				t.Fatalf("stale replay: %v", err)
			}
			assertConsoleLearnedRejection(t, n, client, identity("alice"))
		})
	}
}

func assertConsoleLearnedRejection(t *testing.T, n *Node, client pb.ConsoleServiceClient, identity *pb.ConsoleIdentity) {
	t.Helper()
	rejected, err := n.selfTaught.Propose(t.Context(), &pb.SelfTaughtRecord{Kind: pb.SelfTaughtKind_SELF_TAUGHT_KIND_SKILL, Name: "reject-guide", Body: "do not activate", Owner: "bot:worker"}, memory.ProposeIntent{})
	if err != nil {
		t.Fatal(err)
	}
	query := &pb.QueryConsoleRequest{Identity: identity, Query: &pb.QueryConsoleRequest_LearnedReview{LearnedReview: &pb.ConsoleTarget{Id: rejected.Id}}}
	view, err := client.QueryConsole(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	review := view.GetLearnedReview()
	decision := &pb.MutateConsoleRequest{Identity: identity, Operation: &pb.MutateConsoleRequest_DecideLearnedReview{DecideLearnedReview: &pb.ConsoleLearnedDecision{Id: review.Id, Revision: review.Revision, Digest: review.Digest, Approve: false}}}
	out, err := client.MutateConsole(t.Context(), decision)
	if err != nil || !strings.Contains(out.GetLearnedDecision().GetMessage(), "denied and archived") {
		t.Fatalf("reject: %v %v", out, err)
	}
	if _, err := n.skillRegistry.Get("reject-guide"); err == nil {
		t.Fatal("rejection activated skill")
	}
}
