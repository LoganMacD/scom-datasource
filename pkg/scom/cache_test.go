package scom

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock lets a test move a cache's notion of "now".
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestCache[V any](maxCost int, costOf func(V) int) (*ttlCache[V], *fakeClock) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	c := newTTLCache(maxCost, costOf)
	c.now = clock.Now
	return c, clock
}

func TestTTLCacheKeepsValuesUntilTheyExpire(t *testing.T) {
	c, clock := newTestCache[int](0, nil)
	loads := 0
	load := func(context.Context) (int, error) {
		loads++
		return loads, nil
	}

	for i := 0; i < 3; i++ {
		if v, err := c.get(context.Background(), "k", time.Minute, load); err != nil || v != 1 {
			t.Fatalf("get #%d = (%d, %v), want (1, nil)", i, v, err)
		}
	}
	if loads != 1 {
		t.Fatalf("loaded %d times within the TTL, want 1", loads)
	}

	clock.Advance(time.Minute)
	if v, _ := c.get(context.Background(), "k", time.Minute, load); v != 2 {
		t.Fatalf("got %d after expiry, want a fresh load (2)", v)
	}
}

func TestTTLCacheDoesNotKeepErrors(t *testing.T) {
	c, _ := newTestCache[int](0, nil)
	loads := 0
	failing := func(context.Context) (int, error) {
		loads++
		return 0, errors.New("boom")
	}
	for i := 0; i < 2; i++ {
		if _, err := c.get(context.Background(), "k", time.Minute, failing); err == nil {
			t.Fatal("got no error, want the load's error")
		}
	}
	if loads != 2 {
		t.Fatalf("loaded %d times, want every call to retry a failed load", loads)
	}
}

// A dashboard's panels ask for the same scope at the same moment; the
// database should see one query, not one per panel.
func TestTTLCacheCollapsesConcurrentLoads(t *testing.T) {
	c, _ := newTestCache[int](0, nil)
	var loads atomic.Int32
	release := make(chan struct{})
	load := func(context.Context) (int, error) {
		loads.Add(1)
		<-release
		return 42, nil
	}

	const callers = 10
	var wg sync.WaitGroup
	results := make([]int, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _ = c.get(context.Background(), "k", time.Minute, load)
		}(i)
	}
	// Let every caller reach the cache before the load finishes.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := loads.Load(); n != 1 {
		t.Fatalf("loaded %d times for %d concurrent callers, want 1", n, callers)
	}
	for i, v := range results {
		if v != 42 {
			t.Errorf("caller %d got %d, want 42", i, v)
		}
	}
}

// If the request that started a load is cancelled (its panel was closed),
// requests waiting on the same key mustn't fail with its cancellation.
func TestTTLCacheRetriesWhenTheLeaderIsCancelled(t *testing.T) {
	c, _ := newTestCache[int](0, nil)
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	started := make(chan struct{})
	var loads atomic.Int32

	go func() {
		_, _ = c.get(leaderCtx, "k", time.Minute, func(ctx context.Context) (int, error) {
			loads.Add(1)
			close(started)
			<-ctx.Done()
			return 0, ctx.Err()
		})
	}()
	<-started

	done := make(chan int)
	go func() {
		v, err := c.get(context.Background(), "k", time.Minute, func(context.Context) (int, error) {
			loads.Add(1)
			return 7, nil
		})
		if err != nil {
			t.Errorf("waiting caller got error %v, want its own load to succeed", err)
		}
		done <- v
	}()
	time.Sleep(10 * time.Millisecond)
	cancelLeader()

	if v := <-done; v != 7 {
		t.Fatalf("waiting caller got %d, want 7 from its own retry", v)
	}
	if n := loads.Load(); n != 2 {
		t.Fatalf("got %d loads, want 2 (the cancelled one and the retry)", n)
	}
}

func TestTTLCacheEvictsToStayWithinItsCost(t *testing.T) {
	c, clock := newTestCache(10, func(v []string) int { return len(v) })
	ctx := context.Background()
	value := func(n int) func(context.Context) ([]string, error) {
		return func(context.Context) ([]string, error) { return make([]string, n), nil }
	}

	_, _ = c.get(ctx, "a", time.Minute, value(4))
	clock.Advance(time.Second)
	_, _ = c.get(ctx, "b", time.Minute, value(4))
	clock.Advance(time.Second)
	_, _ = c.get(ctx, "c", time.Minute, value(4)) // over budget: "a" expires first, so it goes

	if _, ok := c.entries["a"]; ok {
		t.Error(`entry "a" (soonest to expire) survived, want it evicted`)
	}
	if _, ok := c.entries["c"]; !ok {
		t.Error(`entry "c" wasn't stored`)
	}
	if c.cost > 10 {
		t.Errorf("cache cost %d, want at most 10", c.cost)
	}

	// A value bigger than the whole budget is returned but not kept.
	got, err := c.get(ctx, "huge", time.Minute, value(11))
	if err != nil || len(got) != 11 {
		t.Fatalf("got (%d items, %v), want the value back", len(got), err)
	}
	if _, ok := c.entries["huge"]; ok {
		t.Error("an over-budget value was stored, want it skipped")
	}
}

func TestTTLCacheTTLOfOverridesTheLifetime(t *testing.T) {
	c, clock := newTestCache[retentionResult](0, nil)
	c.ttlOf = func(r retentionResult) time.Duration {
		if r.err != nil {
			return time.Second
		}
		return 0
	}
	loads := 0
	failed := func(context.Context) (retentionResult, error) {
		loads++
		return retentionResult{err: errors.New("permission denied")}, nil
	}

	_, _ = c.get(context.Background(), "perf", time.Hour, failed)
	_, _ = c.get(context.Background(), "perf", time.Hour, failed)
	if loads != 1 {
		t.Fatalf("loaded %d times, want the failure cached", loads)
	}
	clock.Advance(time.Second)
	_, _ = c.get(context.Background(), "perf", time.Hour, failed)
	if loads != 2 {
		t.Fatalf("loaded %d times, want the failure to expire after its shorter TTL", loads)
	}
}

func TestNilTTLCacheLoadsEveryTime(t *testing.T) {
	var c *ttlCache[int]
	loads := 0
	for i := 0; i < 2; i++ {
		_, _ = c.get(context.Background(), "k", time.Minute, func(context.Context) (int, error) {
			loads++
			return loads, nil
		})
	}
	if loads != 2 {
		t.Fatalf("loaded %d times, want a nil cache to cache nothing", loads)
	}
}

func TestIDSetKeyIgnoresOrderCaseAndDuplicates(t *testing.T) {
	a := idSetKey([]string{"AAAA-1", "bbbb-2"})
	b := idSetKey([]string{"bbbb-2", "aaaa-1", "BBBB-2"})
	if a != b {
		t.Errorf("same set gave different keys: %s vs %s", a, b)
	}
	if a == idSetKey([]string{"aaaa-1"}) {
		t.Error("different sets gave the same key")
	}
}

func TestScopeKeyDistinguishesClassFromGroup(t *testing.T) {
	if scopeKey("x", "", nil) == scopeKey("", "x", nil) {
		t.Error("a class and a group with the same id share a key")
	}
}
