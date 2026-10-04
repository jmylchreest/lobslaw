package console

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type testBots struct {
	rec    *pb.BotRecord
	writes int
}

func (b *testBots) List(context.Context) ([]*pb.BotRecord, error) {
	return []*pb.BotRecord{proto.Clone(b.rec).(*pb.BotRecord)}, nil
}
func (b *testBots) Get(context.Context, string) (*pb.BotRecord, error) {
	return proto.Clone(b.rec).(*pb.BotRecord), nil
}
func (b *testBots) Put(_ context.Context, r *pb.BotRecord, _ uint64) (*pb.BotRecord, error) {
	b.writes++
	b.rec = proto.Clone(r).(*pb.BotRecord)
	return r, nil
}
func (b *testBots) Delete(context.Context, string) error { b.writes++; return nil }
func TestOperationsEnforceOwnershipWithoutTransport(t *testing.T) {
	store := &testBots{rec: &pb.BotRecord{Id: "worker", Owner: "user:alice", Revision: 8}}
	svc := New(Config{Bots: store})
	for _, claims := range []*types.Claims{nil, {UserID: "bob"}, {UserID: ""}} {
		if _, err := svc.Bot(context.Background(), claims, "worker"); !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("cross-owner result: %v", err)
		}
		if err := svc.DeleteBot(context.Background(), claims, "worker"); err == nil {
			t.Fatal("cross-owner delete")
		}
	}
	if store.writes != 0 {
		t.Fatal("unauthorized write reached store")
	}
	if _, err := svc.Bot(context.Background(), &types.Claims{UserID: "alice"}, "worker"); err != nil {
		t.Fatal(err)
	}
	store.rec.Owner = ""
	if _, err := svc.Bot(context.Background(), &types.Claims{UserID: "alice"}, "worker"); err == nil {
		t.Fatal("unowned bot exposed")
	}
}
func TestBotPatchPreservesPresenceAndRejectsStaleRevision(t *testing.T) {
	store := &testBots{rec: &pb.BotRecord{Id: "worker", Owner: "user:alice", Revision: 8, Instructions: "keep", Tools: []string{"read_file"}, Enabled: true}}
	svc := New(Config{Bots: store})
	claims := &types.Claims{UserID: "alice"}
	rev := uint64(7)
	empty := []string{}
	disabled := false
	if _, err := svc.UpdateBot(context.Background(), claims, "worker", BotPatch{Revision: &rev, Tools: &empty}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale result %v", err)
	}
	rev = 8
	out, err := svc.UpdateBot(context.Background(), claims, "worker", BotPatch{Revision: &rev, Tools: &empty, Enabled: &disabled})
	if err != nil {
		t.Fatal(err)
	}
	if out.Instructions != "keep" || out.Enabled || len(out.Tools) != 0 || store.writes != 1 {
		t.Fatalf("patch presence lost: %+v", out)
	}
}
func TestCancelledOperationDoesNotReachStorage(t *testing.T) {
	store := &testBots{rec: &pb.BotRecord{Id: "worker", Owner: "user:alice"}}
	svc := New(Config{Bots: store})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.DeleteBot(ctx, &types.Claims{UserID: "alice"}, "worker"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if store.writes != 0 {
		t.Fatal("cancelled mutation executed")
	}
}

type testPrompts struct {
	record   PromptRecord
	resolved int
}

func (p *testPrompts) Get(string) (PromptRecord, error)   { return p.record, nil }
func (p *testPrompts) Resolve(string, bool, string) error { p.resolved++; return nil }
func TestPromptOperationsCheckOwnerAndExpiry(t *testing.T) {
	p := &testPrompts{record: PromptRecord{RaisedFor: "alice", View: PromptView{ID: "p", Decision: "pending", ExpiresAt: time.Now().Add(time.Hour)}}}
	s := New(Config{Prompts: p})
	if _, _, err := s.ResolvePrompt(context.Background(), &types.Claims{UserID: "bob"}, "p", true, "always"); !errors.Is(err, ErrPromptNotFound) {
		t.Fatal(err)
	}
	p.record.View.ExpiresAt = time.Now().Add(-time.Minute)
	if _, _, err := s.ResolvePrompt(context.Background(), &types.Claims{UserID: "alice"}, "p", true, "once"); !errors.Is(err, ErrPromptExpired) {
		t.Fatal(err)
	}
	if p.resolved != 0 {
		t.Fatal("unauthorized or expired approval applied")
	}
	p.record.View.ExpiresAt = time.Now().Add(time.Hour)
	decision, scope, err := s.ResolvePrompt(context.Background(), &types.Claims{UserID: "alice"}, "p", true, "unknown")
	if err != nil || decision != "approved" || scope != "once" || p.resolved != 1 {
		t.Fatalf("%s %s %v", decision, scope, err)
	}
}
func TestSharedLayerAndGatewayHaveNoStorageImports(t *testing.T) {
	for _, dir := range []string{".", "../gateway"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, entry.Name()), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, i := range f.Imports {
				path := strings.Trim(i.Path.Value, "\"")
				if path == "github.com/jmylchreest/lobslaw/internal/memory" || path == "go.etcd.io/bbolt" {
					t.Errorf("%s imports storage %s", entry.Name(), path)
				}
				if dir == "." && (path == "net/http" || strings.HasPrefix(path, "google.golang.org/grpc")) {
					t.Errorf("shared operation imports transport %s", path)
				}
			}
		}
	}
}
