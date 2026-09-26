package gateway

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

func TestMediaTurnsNeverFold(t *testing.T) {
	for _, mode := range []QueueMode{QueueDebounce, QueueSmart} {
		t.Run(string(mode), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				g := NewTurnGate(mode, time.Second, nil)
				first, d := g.acquire(context.Background(), "session", "first", "first", true)
				if d != Admitted {
					t.Fatal(d)
				}
				done := make(chan struct{}, 3)
				for i, media := range []bool{false, true, false} {
					go func() {
						l, d := g.acquire(context.Background(), "session", "turn", "text", media)
						if d != Admitted {
							t.Errorf("turn %d disposition %v", i, d)
						} else {
							if len(l.Batch) != 1 {
								t.Errorf("folded batch: %v", l.Batch)
							}
							l.Release()
						}
						done <- struct{}{}
					}()
					synctest.Wait()
				}
				first.Release()
				for range 3 {
					<-done
				}
			})
		})
	}
}
func TestMediaNotAbsorbedByIdleFoldWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := NewTurnGate(QueueDebounce, time.Second, nil)
		done := make(chan struct{}, 2)
		for _, media := range []bool{false, true} {
			go func() {
				l, d := g.acquire(context.Background(), "session", "turn", "text", media)
				if d != Admitted {
					t.Errorf("disposition %v", d)
				} else {
					if len(l.Batch) != 1 {
						t.Error("media folded")
					}
					l.Release()
				}
				done <- struct{}{}
			}()
			synctest.Wait()
		}
		for range 2 {
			<-done
		}
	})
}

func TestReleaseFoldsTextPrefixBeforeMedia(t *testing.T) {
	g := NewTurnGate(QueueDebounce, time.Second, nil)
	text := &waiter{batch: []string{"a"}, ready: make(chan Disposition, 1)}
	next := &waiter{batch: []string{"b"}, ready: make(chan Disposition, 1)}
	media := &waiter{noFold: true, batch: []string{"media"}, ready: make(chan Disposition, 1)}
	g.sessions["session"] = &gateState{running: true, waiters: []*waiter{text, next, media}}
	g.release("session")
	if len(text.batch) != 2 || len(g.sessions["session"].waiters) != 1 {
		t.Fatalf("text prefix not folded: %v", text.batch)
	}
}
