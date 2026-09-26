package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func consoleTestIdentity() *pb.ConsoleIdentity {
	return &pb.ConsoleIdentity{Claims: &pb.Claims{UserId: "alice", Roles: []string{"owner"}, Scope: "personal"}, Principal: "user:alice"}
}

type approvalConsoleRunner struct{ resumed chan turn.Request }

func (*approvalConsoleRunner) Run(context.Context, turn.Request) (*turn.Response, error) {
	return &turn.Response{NeedsConfirmation: true, ConfirmationReason: "write needs approval", ConfirmationAction: "tool:exec", ConfirmationResource: "write_file", ToolCalls: []turn.ToolInvocation{{ToolName: "write_file", Error: "requires confirmation"}}}, nil
}
func (r *approvalConsoleRunner) Resume(ctx context.Context, req turn.Request, _ []turn.Message) (*turn.Response, error) {
	action, resource := turn.TakeApproval(ctx)
	if action != "tool:exec" || resource != "write_file" {
		return nil, status.Error(codes.PermissionDenied, "approval binding lost")
	}
	r.resumed <- req
	return &turn.Response{Reply: "done", ToolCalls: []turn.ToolInvocation{{ToolName: "write_file", Output: "written", ExecutionStatus: turn.ReceiptExecuted}}}, nil
}

func TestTypedChatApprovalRetainsActorClaimsAndAttemptEvidence(t *testing.T) {
	t.Parallel()
	runner := &approvalConsoleRunner{resumed: make(chan turn.Request, 1)}
	backend := startWebREST(t, runner, func(c *RESTConfig) {
		c.Bots = &memBots{recs: map[string]*pb.BotRecord{"worker": {Id: "worker", Owner: "user:alice"}}}
	})
	client := testConsoleClient(t, backend)
	const deadline = 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	stream, err := client.ChatConsole(ctx, &pb.ChatConsoleRequest{Identity: consoleTestIdentity(), Bot: "worker", Message: "write"})
	if err != nil {
		t.Fatal(err)
	}
	var prompt string
	for prompt == "" {
		event, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		prompt = event.GetNeedsConfirmation().GetPromptId()
	}
	bob := consoleTestIdentity()
	bob.Claims.UserId = "bob"
	bob.Principal = "user:bob"
	decision := &pb.MutateConsoleRequest{Identity: bob, Operation: &pb.MutateConsoleRequest_ResolvePrompt{ResolvePrompt: &pb.ConsolePromptDecision{Id: prompt, Approve: true}}}
	if _, err := client.MutateConsole(ctx, decision); status.Code(err) != codes.NotFound {
		t.Fatalf("cross-owner approval = %v", err)
	}
	decision.Identity = consoleTestIdentity()
	if _, err := client.MutateConsole(ctx, decision); err != nil {
		t.Fatal(err)
	}
	var reply *pb.ConsoleBotReply
	for reply == nil {
		event, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		reply = event.GetReply()
	}
	if reply.GetText() != "done" || reply.GetToolCalls() != 1 || !reflect.DeepEqual(reply.GetToolsUsed(), []string{"write_file"}) || !reflect.DeepEqual(reply.GetToolsAttempted(), []string{"write_file"}) {
		t.Fatalf("incorrect typed receipt: %v", reply)
	}
	req := <-runner.resumed
	if req.Principal != identity.Bot("worker") || req.Claims.UserID != "alice" || req.Claims.Scope != "personal" || !reflect.DeepEqual(req.Claims.Roles, []string{"owner"}) {
		t.Fatalf("resume identity changed: %+v", req)
	}
}

func TestRemoteConsolePreservesDisabledCapabilitiesDuringOutage(t *testing.T) {
	t.Parallel()
	backend := startWebREST(t, nil, nil)
	client := &outageConsoleClient{ConsoleServiceClient: testConsoleClient(t, backend)}
	front := startWebREST(t, nil, func(c *RESTConfig) { c.RemoteConsole = client })
	auth := http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}}
	for _, down := range []bool{false, true} {
		client.down.Store(down)
		response := doJSON(t, http.MethodGet, webBaseURL(front)+"/v1/capabilities", "", auth)
		var caps capabilitiesResponse
		if err := json.NewDecoder(response.Body).Decode(&caps); err != nil {
			t.Fatal(err)
		}
		if caps.Compute.Enabled || caps.ComputeTeams.Enabled || caps.Compute.Available || caps.ComputeTeams.Available {
			t.Fatalf("disabled became enabled during outage: %+v", caps)
		}
	}
}

func TestConsoleRejectsNonPeersAndMismatchedPrincipals(t *testing.T) {
	t.Parallel()
	s := NewServer(RESTConfig{}, nil)
	operator := &x509.Certificate{Subject: pkix.Name{OrganizationalUnit: []string{mtls.OperatorOU}}}
	for _, ctx := range []context.Context{
		context.Background(),
		peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}}}),
		peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{operator}}}}}),
	} {
		_, err := s.QueryConsole(ctx, &pb.QueryConsoleRequest{Identity: consoleTestIdentity(), Query: &pb.QueryConsoleRequest_Bots{Bots: &pb.ConsoleEmpty{}}})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("unverified identity accepted: %v", err)
		}
		_, err = s.MutateConsole(ctx, &pb.MutateConsoleRequest{Identity: consoleTestIdentity()})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("unverified mutation accepted: %v", err)
		}
	}
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{}}}}}})
	identity := consoleTestIdentity()
	identity.Principal = "user:bob"
	_, err := s.QueryConsole(ctx, &pb.QueryConsoleRequest{Identity: identity, Query: &pb.QueryConsoleRequest_Capabilities{Capabilities: &pb.ConsoleEmpty{}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("claims/principal disagreement accepted: %v", err)
	}
}

func TestConsoleContractCannotCarryAnHTTPTunnel(t *testing.T) {
	t.Parallel()
	service := pb.File_lobslaw_v1_lobslaw_proto.Services().ByName("ConsoleService")
	if service.Methods().ByName("ConsoleForward") != nil {
		t.Fatal("HTTP tunnel still exposed")
	}
	seen := map[protoreflect.FullName]bool{}
	var check func(protoreflect.MessageDescriptor)
	check = func(message protoreflect.MessageDescriptor) {
		if seen[message.FullName()] {
			return
		}
		seen[message.FullName()] = true
		for i := 0; i < message.Fields().Len(); i++ {
			field := message.Fields().Get(i)
			// TaskApprovalRecord contains private continuation fields in the
			// shared contract; the owner service strips these, tested there.
			if !strings.HasPrefix(string(message.Name()), "Console") && !strings.HasSuffix(string(message.Name()), "ConsoleRequest") && !strings.HasSuffix(string(message.Name()), "ConsoleResponse") {
				continue
			}
			if field.Kind() == protoreflect.BytesKind || field.Name() == "method" || field.Name() == "path" || field.Name() == "content_type" {
				t.Fatalf("opaque HTTP field: %s", field.FullName())
			}
			if field.Message() != nil {
				check(field.Message())
			}
		}
	}
	for i := 0; i < service.Methods().Len(); i++ {
		method := service.Methods().Get(i)
		check(method.Input())
		check(method.Output())
	}
}

func TestTypedConsolePreservesPartialUpdatesAndRevision(t *testing.T) {
	t.Parallel()
	const revision uint64 = 9007199254740993
	bots := &memBots{recs: map[string]*pb.BotRecord{"worker": {Id: "worker", Owner: "user:alice", Revision: revision, Enabled: true, Description: "keep", Tools: []string{"read_file"}}}}
	backend := startWebREST(t, &captureRunner{}, func(c *RESTConfig) { c.Bots = bots })
	client := testConsoleClient(t, backend)
	name, enabled := "Renamed", false
	out, err := client.MutateConsole(context.Background(), &pb.MutateConsoleRequest{Identity: consoleTestIdentity(), Operation: &pb.MutateConsoleRequest_UpdateBot{UpdateBot: &pb.ConsoleBotPatch{Id: "worker", Revision: &[]uint64{revision}[0], DisplayName: &name, Enabled: &enabled, Tools: &pb.ConsoleStrings{}}}})
	if err != nil {
		t.Fatal(err)
	}
	if out.GetBot().GetEnabled() || out.GetBot().GetDescription() != "keep" || len(out.GetBot().GetTools()) != 0 || out.GetBot().GetDisplayName() != name {
		t.Fatalf("patch lost presence: %v", out)
	}
	front := startWebREST(t, nil, func(c *RESTConfig) { c.RemoteConsole = client })
	auth := http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}}
	response := doJSON(t, http.MethodGet, webBaseURL(front)+"/v1/bots/worker", "", auth)
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["enabled"] != false || body["tools"] == nil || body["may_message"] == nil {
		t.Fatalf("REST defaults lost: %#v", body)
	}
	_, err = client.QueryConsole(context.Background(), &pb.QueryConsoleRequest{Identity: consoleTestIdentity(), Query: &pb.QueryConsoleRequest_Bot{Bot: &pb.ConsoleTarget{Id: "worker/messages"}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("identifier selected a route: %v", err)
	}
}

func TestLogoutCancelsRemainingStreamsAfterOneFinishes(t *testing.T) {
	t.Parallel()
	s := NewServer(RESTConfig{}, nil)
	s.logins.put(&loginSession{ID: "login", UserID: "alice"})
	first, done := s.bindStream(context.Background(), "login")
	second, stopSecond := s.bindStream(context.Background(), "login")
	defer stopSecond()
	third, stopThird := s.bindStream(context.Background(), "login")
	defer stopThird()
	done()
	if first.Err() == nil || second.Err() != nil || third.Err() != nil {
		t.Fatal("finishing one stream affected another")
	}
	s.logins.revoke("login")
	if second.Err() == nil || third.Err() == nil {
		t.Fatal("logout left another active stream running")
	}
	late, stopLate := s.bindStream(context.Background(), "login")
	defer stopLate()
	if late.Err() == nil {
		t.Fatal("stream registered after logout remained active")
	}
}

func TestReceiptDoesNotCallRefusedOrPendingToolsExecuted(t *testing.T) {
	t.Parallel()
	calls := []turn.ToolInvocation{
		{ToolName: "write_file", Error: "policy denied"},
		{ToolName: "shell_command", Error: "requires confirmation"},
		{ToolName: "read_file", ExecutionStatus: turn.ReceiptExecuted},
		{ToolName: "shell_command", ExecutionStatus: turn.ReceiptExecuted, ExitCode: 1, Output: "process failed"},
	}
	if got := invokedToolNames(calls); !reflect.DeepEqual(got, []string{"read_file", "shell_command"}) {
		t.Fatalf("executed = %v", got)
	}
	if got := unconfirmedToolNames(calls); !reflect.DeepEqual(got, []string{"shell_command", "write_file"}) {
		t.Fatalf("unconfirmed = %v", got)
	}
	if got := returnedToolCount(calls); got != 2 {
		t.Fatalf("returned calls = %d", got)
	}
}

func TestConsoleTaskDecisionsUseSharedOwnerContract(t *testing.T) {
	t.Parallel()
	api := new(taskAPISpy)
	backend := startWebREST(t, nil, func(c *RESTConfig) { c.TaskApprovals = api })
	client := testConsoleClient(t, backend)
	// A typed request's owner is not authority: the peer assertion is.
	_, err := client.QueryConsole(context.Background(), &pb.QueryConsoleRequest{Identity: consoleTestIdentity(), Query: &pb.QueryConsoleRequest_TaskApprovals{TaskApprovals: &pb.ListTaskApprovalRequest{Owner: "user:bob"}}})
	if err != nil || api.owner != "user:alice" {
		t.Fatalf("owner=%s err=%v", api.owner, err)
	}
	front := startWebREST(t, nil, func(c *RESTConfig) { c.RemoteConsole = client })
	login := doJSON(t, http.MethodPost, webBaseURL(front)+"/v1/session", "", http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}})
	cookie := loginCookie(login)
	if cookie == nil {
		t.Fatal("no login cookie")
	}
	path := webBaseURL(front) + "/v1/task-approvals/task/decide"
	body := `{"revision":"9007199254740993","choice":"once"}`
	response := doJSON(t, http.MethodPost, path, body, cookieHeader(cookie))
	if response.StatusCode != http.StatusForbidden {
		t.Fatal("cookie decision bypassed CSRF")
	}
	response = doJSON(t, http.MethodPost, path, body, cookieHeader(cookie, originFor(webBaseURL(front))))
	if response.StatusCode != http.StatusOK || api.owner != "user:alice" || api.revision != 9007199254740993 {
		raw, _ := io.ReadAll(response.Body)
		t.Fatalf("decision: %d %s owner=%s", response.StatusCode, raw, api.owner)
	}
}

func TestLocalTaskCookieStillRejectsAnonymousAndForgedOwner(t *testing.T) {
	t.Parallel()
	api := new(taskAPISpy)
	s := NewServer(RESTConfig{TaskApprovals: api}, nil)
	s.logins.put(&loginSession{ID: "login", UserID: "alice"})
	r := httptest.NewRequest(http.MethodPost, "http://console/v1/task-approvals/task/decide", strings.NewReader(`{"owner":"user:bob","revision":"1","choice":"once"}`))
	r.AddCookie(&http.Cookie{Name: LoginCookieName, Value: "login"})
	r.Header.Set("Origin", "http://console")
	w := httptest.NewRecorder()
	s.handleTaskApprovals(w, r)
	if w.Code != http.StatusBadRequest || api.calls != 0 {
		t.Fatal("body-selected owner accepted")
	}
}
