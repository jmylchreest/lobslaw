package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Unary peer dispatch must not depend on HTTP request/response adapters.
func TestConsoleUnaryDispatchHasNoHTTPBridge(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "console_remote.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || (fn.Name.Name != "QueryConsole" && fn.Name.Name != "MutateConsole") {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if ok && sel.Sel.Name == "consoleOperation" {
				t.Error("peer dispatch still invokes the synthetic HTTP bridge")
			}
			return true
		})
	}
}

func TestCancelledConsoleMutationDoesNotWrite(t *testing.T) {
	bots := &memBots{recs: map[string]*pb.BotRecord{"worker": {Id: "worker", Owner: "user:alice", Revision: 9007199254740993}}}
	backend := NewServer(RESTConfig{Bots: bots}, nil)
	ctx := context.WithValue(context.Background(), forwardedConsoleIdentity{}, &types.Claims{UserID: "alice"})
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err := backend.mutateConsoleOperations(ctx, &pb.MutateConsoleRequest{Operation: &pb.MutateConsoleRequest_DeleteBot{DeleteBot: &pb.ConsoleTarget{Id: "worker"}}})
	if status.Code(err) != codes.Canceled {
		t.Fatalf("cancellation: %v", err)
	}
	if _, ok := bots.recs["worker"]; !ok {
		t.Fatal("cancelled mutation reached registry")
	}
}

type failingConsoleStream struct {
	grpc.ServerStreamingServer[pb.ChatConsoleResponse]
}

func (failingConsoleStream) Send(*pb.ChatConsoleResponse) error { return errors.New("disconnected") }
func TestConsoleStreamFailureCancelsTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &consoleEvents{stream: failingConsoleStream{}, cancel: cancel}
	if err := sink.event("typing", &pb.ConsoleProgress{}); err == nil {
		t.Fatal("lost send failure")
	}
	if ctx.Err() != context.Canceled {
		t.Fatal("stream failure did not cancel runner context")
	}
}
func TestConsoleChatHasNoSyntheticHTTP(t *testing.T) {
	for _, path := range []string{"console_remote.go", "console_stream.go", "console_chat.go", "console_bot_chat.go"} {
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ident, ok := n.(*ast.Ident)
			if ok {
				switch ident.Name {
				case "consoleLocalRequest", "consoleResult", "newConsoleResult":
					t.Errorf("%s still uses %s", path, ident.Name)
				}
			}
			return true
		})
	}
}

func TestRESTRevisionStringsWorkLocallyAndOverPeers(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(fmt.Sprint(remote), func(t *testing.T) {
			const rev = uint64(9007199254740993)
			bots := &memBots{recs: map[string]*pb.BotRecord{"worker": {Id: "worker", Owner: "user:alice", Revision: rev, Enabled: true, Description: "keep"}}}
			backend := startWebREST(t, &captureRunner{}, func(c *RESTConfig) { c.Bots = bots })
			front := backend
			if remote {
				client := testConsoleClient(t, backend)
				front = startWebREST(t, nil, func(c *RESTConfig) { c.RemoteConsole = client })
			}
			auth := http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}}
			response := doJSON(t, http.MethodGet, webBaseURL(front)+"/v1/bots/worker", "", auth)
			var body map[string]any
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["revision"] != "9007199254740993" {
				t.Fatalf("unsafe revision: %#v", body["revision"])
			}
			response = doJSON(t, http.MethodPatch, webBaseURL(front)+"/v1/bots/worker", `{"revision":"9007199254740993","enabled":false,"tools":[]}`, auth)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("patch status: %d", response.StatusCode)
			}
			if bots.recs["worker"].Enabled || bots.recs["worker"].Description != "keep" {
				t.Fatal("patch lost presence")
			}
			response = doJSON(t, http.MethodPatch, webBaseURL(front)+"/v1/bots/worker", `{"revision":"9007199254740993","enabled":true}`, auth)
			if response.StatusCode != http.StatusConflict {
				t.Fatalf("stale revision accepted: %d", response.StatusCode)
			}
		})
	}
}
