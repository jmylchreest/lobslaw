package secrets

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestResolverKeepsOneCleanupTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := NewResolver(nil, 2*time.Minute)
		var scheduled, pending atomic.Int64
		r.afterFunc = func(delay time.Duration, fn func()) *time.Timer {
			scheduled.Add(1)
			pending.Add(1)
			return time.AfterFunc(delay, func() {
				pending.Add(-1)
				fn()
			})
		}
		check := func(wantScheduled, wantPending int64) {
			t.Helper()
			synctest.Wait()
			if got := scheduled.Load(); got != wantScheduled {
				t.Fatalf("scheduled %d cleanup timers; want %d", got, wantScheduled)
			}
			if got := pending.Load(); got != wantPending {
				t.Fatalf("pending cleanup timers=%d; want %d", got, wantPending)
			}
		}
		var wg sync.WaitGroup
		for i := range 10 {
			wg.Go(func() { r.store(fmt.Sprint(i), "value") })
		}
		wg.Wait()
		check(1, 1)
		time.Sleep(time.Minute)
		check(2, 1)
		r.store("0", "refreshed")
		check(2, 1)
		time.Sleep(time.Minute)
		check(3, 1)
		if value, ok := r.cached("0"); !ok || value != "refreshed" {
			t.Fatal("cleanup lost refreshed entry")
		}
		time.Sleep(time.Minute)
		check(3, 0)
		// A drained resolver restarts exactly one chain, even on replacement.
		r.store("new", "value")
		r.store("new", "replacement")
		check(4, 1)
		time.Sleep(2 * time.Minute)
		check(5, 0)
	})
}

func TestResolverShortTTLDoesNotSpinCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const ttl = time.Millisecond
		r := NewResolver(nil, ttl)
		var scheduled atomic.Int64
		r.afterFunc = func(delay time.Duration, fn func()) *time.Timer {
			scheduled.Add(1)
			if delay < time.Second {
				t.Errorf("cleanup delay=%v; want at least one second", delay)
			}
			return time.AfterFunc(delay, fn)
		}
		r.store("lookup", "value")
		r.store("idle", "value")
		time.Sleep(ttl)
		synctest.Wait()
		if _, ok := r.cached("lookup"); ok {
			t.Fatal("cleanup floor extended lookup TTL")
		}
		// Refresh just before the first scan so its callback must rearm.
		time.Sleep(time.Second - ttl - ttl/2)
		r.store("idle", "fresh")
		time.Sleep(ttl / 2)
		synctest.Wait()
		if got := scheduled.Load(); got != 2 {
			t.Fatalf("scheduled %d timers through first scan; want 2", got)
		}
		if value, ok := r.cached("idle"); !ok || value != "fresh" {
			t.Fatal("first scan lost unexpired refresh")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		r.mu.Lock()
		defer r.mu.Unlock()
		if len(r.cache) != 0 || r.cleanupScheduled {
			t.Fatal("idle short-TTL cache did not drain")
		}
		if got := scheduled.Load(); got != 2 {
			t.Fatalf("cleanup kept scheduling after idle expiry: %d timers", got)
		}
	})
}

func TestResolverIdleCleanupAndRestart(t *testing.T) {
	for _, ttl := range []time.Duration{10 * time.Second, DefaultCacheTTL} {
		t.Run(ttl.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := NewResolver(nil, ttl)
				for range 2 {
					r.store("vault:key", "value")
					time.Sleep(ttl)
					synctest.Wait()
					r.mu.Lock()
					count, scheduled := len(r.cache), r.cleanupScheduled
					r.mu.Unlock()
					if count != 0 || scheduled {
						t.Fatalf("idle cleanup: entries=%d scheduled=%v", count, scheduled)
					}
				}
			})
		})
	}
}

func TestResolverLookupRemovesExpiredEntry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := NewResolver(nil, time.Minute)
		// Populate directly to isolate lookup expiry from timer cleanup.
		r.cache["vault:key"] = cacheEntry{value: "old", expiresAt: time.Now()}
		if value, ok := r.cached("vault:key"); ok || value != "" {
			t.Fatal("expired value returned")
		}
		if len(r.cache) != 0 {
			t.Fatal("expired entry retained")
		}
	})
}

func TestResolverCapacityEvictsNearestExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := NewResolver(nil, time.Hour)
		r.store("oldest", "old")
		time.Sleep(time.Second)
		for i := 1; i < maxCacheEntries; i++ {
			r.store(fmt.Sprint(i), "value")
		}
		r.store("new", "new")
		if len(r.cache) != maxCacheEntries {
			t.Fatalf("size=%d", len(r.cache))
		}
		if _, ok := r.cached("oldest"); ok {
			t.Fatal("nearest expiry not evicted")
		}
		if value, ok := r.cached("new"); !ok || value != "new" {
			t.Fatal("new entry missing")
		}
		r.store("new", "replacement")
		if len(r.cache) != maxCacheEntries {
			t.Fatal("replacement changed size")
		}
		// Do not leave the timer retaining the resolver after the test.
		time.Sleep(time.Hour)
		synctest.Wait()
	})
}

func TestResolverCapacityPrunesExpiredBeforeEvicting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := NewResolver(nil, time.Hour)
		for i := 0; i < maxCacheEntries; i++ {
			r.cache[fmt.Sprint(i)] = cacheEntry{value: "expired", expiresAt: time.Now()}
		}
		r.cache["0"] = cacheEntry{value: "live", expiresAt: time.Now().Add(time.Hour)}
		r.store("new", "new")
		if len(r.cache) != 2 {
			t.Fatalf("size=%d; want live and new", len(r.cache))
		}
		if value, ok := r.cached("0"); !ok || value != "live" {
			t.Fatal("live entry evicted")
		}
		time.Sleep(time.Hour)
		synctest.Wait()
	})
}

func TestResolverCleanupPreservesConcurrentRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := NewResolver(nil, time.Minute)
		r.store("key", "old")
		var wg sync.WaitGroup
		// Refresh at the original expiry, racing the cleanup callback.
		wg.Go(func() {
			time.Sleep(time.Minute)
			r.store("key", "fresh")
		})
		time.Sleep(time.Minute)
		wg.Wait()
		synctest.Wait()
		if value, ok := r.cached("key"); !ok || value != "fresh" {
			t.Fatal("cleanup lost refreshed value")
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		r.mu.Lock()
		defer r.mu.Unlock()
		if len(r.cache) != 0 || r.cleanupScheduled {
			t.Fatal("cleanup did not drain")
		}
	})
}
