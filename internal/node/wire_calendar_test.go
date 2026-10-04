package node

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestCalendarOperationPolicyKeepsReadAndWriteIndependent(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(t.TempDir(), "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	engine := policy.NewEngine(store, nil)
	engine.SetDefaults([]types.PolicyRule{{ID: "read", Subject: "role:calendar-reader", Action: "calendar:read", Resource: "google/*", Effect: types.EffectAllow}, {ID: "tools", Subject: "role:calendar-reader", Action: "tool:exec", Resource: "calendar_*", Effect: types.EffectAllow}})
	n := &Node{policyEngine: engine}
	ctx := turn.WithIdentity(context.Background(), turn.Identity{UserID: "alice", Principal: "user:alice", Roles: []string{"calendar-reader"}})
	if err := n.authorizeCalendar(ctx, "calendar:read", "google/connection/cal"); err != nil {
		t.Fatal(err)
	}
	if err := n.authorizeCalendar(ctx, "calendar:write", "google/connection/cal"); err == nil {
		t.Fatal("read/tool grants enabled writes")
	}
	if err := n.authorizeCalendar(context.Background(), "calendar:read", "google/connection/cal"); err == nil {
		t.Fatal("anonymous access")
	}
}
