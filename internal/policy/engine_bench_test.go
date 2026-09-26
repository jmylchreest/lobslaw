package policy

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Includes encrypted Store reads, decoding, sorting and matching. Seeding and
// its fsyncs are outside the timer. A matching request walks to the final rule;
// a denied request walks the whole set.
func BenchmarkEngineEvaluate(b *testing.B) {
	for _, count := range []int{0, 10, 50, 200, 1000} {
		b.Run(fmt.Sprintf("rules=%d", count), func(b *testing.B) {
			key, err := crypto.GenerateKey()
			if err != nil {
				b.Fatal(err)
			}
			store, err := memory.OpenStore(filepath.Join(b.TempDir(), "state.db"), key)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = store.Close() })
			for i := 0; i < count; i++ {
				r := &lobslawv1.PolicyRule{Id: fmt.Sprintf("rule-%04d", i), Subject: "user:alice", Action: "read", Resource: fmt.Sprintf("file-%d", i), Effect: "allow", Priority: int32((i * 37) % count)}
				raw, err := proto.Marshal(r)
				if err != nil {
					b.Fatal(err)
				}
				if err := store.Put(memory.BucketPolicyRules, r.Id, raw); err != nil {
					b.Fatal(err)
				}
			}
			engine := NewEngine(store, nil)
			claims := &types.Claims{UserID: "alice"}
			for _, match := range []bool{false, true} {
				if match && count == 0 {
					continue
				}
				b.Run(fmt.Sprintf("match=%t", match), func(b *testing.B) {
					resource := "missing"
					want := types.EffectDeny
					if match {
						resource = "file-0"
						want = types.EffectAllow
					}
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						decision, err := engine.Evaluate(context.Background(), claims, "read", resource)
						if err != nil || decision.Effect != want {
							b.Fatalf("decision=%+v err=%v", decision, err)
						}
					}
				})
			}
		})
	}
}
