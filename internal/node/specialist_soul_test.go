package node

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestSpecialistSoulToolsTargetSameOverlayLocallyAndRemotely(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	leader, _ := bootSoulNode(t, Config{NodeID: "soul-owner", Creds: soulTestCreds(t, dir, "soul-owner"),
		Functions: []types.NodeFunction{types.FunctionMemory, types.FunctionCompute}, MemoryKey: key,
		ListenAddr: "127.0.0.1:0", DataDir: filepath.Join(dir, "memory"), Bootstrap: true, SnapshotTarget: "storage:test",
		LLMProvider: compute.NewMockProvider(compute.MockResponse{Content: "ok"}),
	})
	remote, _ := bootSoulNode(t, Config{NodeID: "soul-worker", Creds: soulTestCreds(t, dir, "soul-worker"),
		Functions: []types.NodeFunction{types.FunctionCompute}, ListenAddr: "127.0.0.1:0", SeedNodes: []string{leader.ListenAddr()},
		LLMProvider: compute.NewMockProvider(compute.MockResponse{Content: "ok"}),
	})
	soulBuiltin(t, leader, "soul_tune", map[string]string{"field": "name", "value": "ChiefOnly"})
	ctx := context.Background()
	chief, err := leader.soulTuneSvc.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for name, n := range map[string]*Node{"local": leader, "remote": remote} {
		t.Run(name, func(t *testing.T) {
			botID := "specialist-" + name
			invoke := func(tool string, args map[string]string) map[string]any {
				t.Helper()
				fn, ok := n.builtinsRegistry.Get(tool)
				if !ok {
					t.Fatalf("missing tool %s", tool)
				}
				out, code, err := fn(turn.WithIdentity(ctx, turn.Identity{BotID: botID, UserID: "alice", Scope: "owner"}), args)
				if err != nil || code != 0 {
					t.Fatalf("%s: %d %v %s", tool, code, err, out)
				}
				current, err := leader.soulTuneSvc.Get(ctx)
				if err != nil || !proto.Equal(current, chief) {
					t.Fatalf("%s changed chief: %v %v", tool, current, err)
				}
				var result map[string]any
				if err := json.Unmarshal(out, &result); err != nil {
					t.Fatal(err)
				}
				return result
			}
			if got := invoke("soul_get", nil)["name"]; got == "ChiefOnly" {
				t.Fatal("specialist inherited chief overlay")
			}
			invoke("soul_tune", map[string]string{"field": "name", "value": "WorkerOnly"})
			invoke("soul_tune", map[string]string{"field": "directness", "delta": "1"})
			invoke("soul_tune", map[string]string{"field": "emoji_usage", "value": "moderate"})
			invoke("soul_fragment_add", map[string]string{"text": "Worker fragment"})
			invoke("soul_fragment_remove", map[string]string{"needle": "Worker fragment"})
			invoke("soul_reset", map[string]string{"field": "name"})
			invoke("soul_history_rollback", map[string]string{"steps": "1"})
			if got := invoke("soul_get", nil)["name"]; got != "WorkerOnly" {
				t.Fatalf("rollback targeted wrong history: %v", got)
			}
			snapshot, err := n.soulSnapshotFor(ctx, botID)
			if err != nil || snapshot.Config.Name != "WorkerOnly" {
				t.Fatalf("tool and prompt disagree: %v %v", snapshot, err)
			}
		})
	}
}
