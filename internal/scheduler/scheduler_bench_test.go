package scheduler

import (
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// BenchmarkSchedulerNextDueTime measures the O(n) cost of the
// sleep-until-due scan across a varying number of scheduled tasks.
// Baseline for the "watch if this grows large" caveat in
// SCHEDULER.md: a future engineer wondering whether the scan has
// become a hotspot can re-run this for data.
//
// At personal-scale (< 100 tasks) the scan is microseconds, and
// the sleep interval dwarfs it. A min-heap optimisation only pays
// off somewhere north of a few thousand tasks, at which point the
// maintenance cost of a heap-kept-in-sync-with-Raft-writes
// outweighs its benefit.
func BenchmarkSchedulerNextDueTime(b *testing.B) {
	for _, n := range []int{10, 100, 1000, 10000} {
		b.Run(fmt.Sprintf("tasks=%d", n), func(b *testing.B) {
			node, _ := singleNodeRaft(b, "bench-node")
			s, err := NewScheduler(Config{NodeID: "bench-node"}, node, NewHandlerRegistry())
			if err != nil {
				b.Fatal(err)
			}
			now := time.Now()
			// Spread NextRun across the full cron minute so the scan
			// isn't dominated by one path (all-due vs all-future).
			for i := range n {
				seedTask(b, node, &lobslawv1.ScheduledTaskRecord{
					Id:       fmt.Sprintf("t-%d", i),
					Schedule: "* * * * *",
					Enabled:  true,
					NextRun:  timestamppb.New(now.Add(time.Duration(i) * time.Second)),
				})
			}
			b.ResetTimer()
			for range b.N {
				if _, err := s.nextDueTime(now); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
