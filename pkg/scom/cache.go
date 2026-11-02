package scom

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
)

// How long each kind of lookup is cached, per data source.
//
// Class/group membership and hosting change as SCOM discovery runs, so scope
// results are kept for a few minutes: long enough that every panel on a
// dashboard, and the next few auto-refreshes, share one lookup. Everything
// else (retention settings, resolution states, the class and group lists,
// class properties, counter picker lists) only changes when someone edits
// SCOM's settings or imports a management pack.
const (
	scopeCacheTTL    = 5 * time.Minute
	metadataCacheTTL = 15 * time.Minute
	// failedRetentionCacheTTL is how long a failed retention read is kept, so
	// a missing permission doesn't add a failing query to every performance
	// query, but a fix is picked up quickly.
	failedRetentionCacheTTL = time.Minute
)

// Cache size limits, counted in items (ids, options or columns), not bytes.
// 500,000 ids is roughly 30 MB.
const (
	maxCachedIDs     = 500_000
	maxCachedOptions = 200_000
	maxCachedColumns = 50_000
)

var errCacheLoadPanicked = errors.New("cache load panicked")

type cacheEntry[V any] struct {
	value   V
	expires time.Time
	cost    int
}

type cacheCall[V any] struct {
	done            chan struct{}
	value           V
	err             error
	leaderCancelled bool
}

// ttlCache keeps values for a set time and collapses concurrent loads of the
// same key into one, so a dashboard whose panels all ask for the same thing
// at once runs the query once. Errors are never kept. Values are shared
// between callers and must be treated as read-only.
//
// Size is bounded by a total cost (the number of ids or options held). When a
// new value would go over it, the entries closest to expiry are dropped
// first, and a value bigger than the whole budget is returned but not kept.
//
// A nil *ttlCache is valid and caches nothing.
type ttlCache[V any] struct {
	maxCost int
	costOf  func(V) int
	// ttlOf, if set, overrides one value's lifetime; returning 0 keeps the
	// TTL passed to get.
	ttlOf func(V) time.Duration
	now   func() time.Time

	mu       sync.Mutex
	entries  map[string]*cacheEntry[V]
	inflight map[string]*cacheCall[V]
	cost     int
}

// newTTLCache returns an empty cache. maxCost 0 means unbounded; costOf nil
// counts every entry as 1.
func newTTLCache[V any](maxCost int, costOf func(V) int) *ttlCache[V] {
	return &ttlCache[V]{
		maxCost:  maxCost,
		costOf:   costOf,
		now:      time.Now,
		entries:  map[string]*cacheEntry[V]{},
		inflight: map[string]*cacheCall[V]{},
	}
}

// get returns the cached value for key, or runs load and keeps its result for
// ttl. Concurrent callers for the same key wait for the first one's load.
func (c *ttlCache[V]) get(ctx context.Context, key string, ttl time.Duration, load func(context.Context) (V, error)) (V, error) {
	if c == nil {
		return load(ctx)
	}
	for {
		c.mu.Lock()
		if e, ok := c.entries[key]; ok {
			if c.now().Before(e.expires) {
				c.mu.Unlock()
				return e.value, nil
			}
			c.removeLocked(key)
		}
		if call, ok := c.inflight[key]; ok {
			c.mu.Unlock()
			select {
			case <-call.done:
			case <-ctx.Done():
				var zero V
				return zero, ctx.Err()
			}
			// The request that started the load was cancelled, but this one
			// wasn't: load again rather than fail a live request.
			if call.err != nil && call.leaderCancelled && ctx.Err() == nil {
				continue
			}
			return call.value, call.err
		}
		call := &cacheCall[V]{done: make(chan struct{})}
		c.inflight[key] = call
		c.mu.Unlock()

		c.run(ctx, key, ttl, call, load)
		return call.value, call.err
	}
}

func (c *ttlCache[V]) run(ctx context.Context, key string, ttl time.Duration, call *cacheCall[V], load func(context.Context) (V, error)) {
	call.err = errCacheLoadPanicked // replaced below unless load panics
	defer func() {
		call.leaderCancelled = ctx.Err() != nil
		c.mu.Lock()
		delete(c.inflight, key)
		if call.err == nil {
			c.storeLocked(key, ttl, call.value)
		}
		c.mu.Unlock()
		close(call.done)
	}()
	call.value, call.err = load(ctx)
}

func (c *ttlCache[V]) storeLocked(key string, ttl time.Duration, v V) {
	if c.ttlOf != nil {
		if d := c.ttlOf(v); d > 0 {
			ttl = d
		}
	}
	if ttl <= 0 {
		return
	}
	cost := 1
	if c.costOf != nil {
		cost = max(c.costOf(v), 1)
	}
	if c.maxCost > 0 && cost > c.maxCost {
		return
	}
	now := c.now()
	c.removeLocked(key)
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			c.removeLocked(k)
		}
	}
	for c.maxCost > 0 && c.cost+cost > c.maxCost && len(c.entries) > 0 {
		c.removeLocked(c.soonestExpiringLocked())
	}
	c.entries[key] = &cacheEntry[V]{value: v, expires: now.Add(ttl), cost: cost}
	c.cost += cost
}

func (c *ttlCache[V]) soonestExpiringLocked() string {
	var key string
	var soonest time.Time
	found := false
	for k, e := range c.entries {
		if !found || e.expires.Before(soonest) {
			key, soonest, found = k, e.expires, true
		}
	}
	return key
}

func (c *ttlCache[V]) removeLocked(key string) {
	if e, ok := c.entries[key]; ok {
		c.cost -= e.cost
		delete(c.entries, key)
	}
}

// dbCaches holds one data source's cached lookups. It lives on DB, so it's
// dropped with the connections whenever the data source's settings change.
//
// Sharing results between users is safe: every user of a data source
// already queries SCOM through the same SQL logins.
type dbCaches struct {
	ids        *ttlCache[[]string]           // resolved and hosting-expanded scopes
	options    *ttlCache[[]Option]           // picker and variable lists
	retention  *ttlCache[retentionResult]    // DW perf retention settings
	classProps *ttlCache[classPropertyLists] // per-class property views and columns
}

func newDBCaches() *dbCaches {
	retention := newTTLCache[retentionResult](0, nil)
	retention.ttlOf = func(r retentionResult) time.Duration {
		if r.err != nil {
			return failedRetentionCacheTTL
		}
		return 0
	}
	return &dbCaches{
		ids:        newTTLCache(maxCachedIDs, func(ids []string) int { return len(ids) }),
		options:    newTTLCache(maxCachedOptions, func(opts []Option) int { return len(opts) }),
		retention:  retention,
		classProps: newTTLCache(maxCachedColumns, func(p classPropertyLists) int { return len(p.Columns) }),
	}
}

// idSetKey is a cache key for a set of ids. Order, case and duplicates don't
// change what a set resolves to, so they don't change the key either.
func idSetKey(ids []string) string {
	norm := make([]string, len(ids))
	for i, id := range ids {
		norm[i] = strings.ToLower(strings.TrimSpace(id))
	}
	slices.Sort(norm)
	norm = slices.Compact(norm)
	sum := sha256.Sum256([]byte(strings.Join(norm, ",")))
	return hex.EncodeToString(sum[:])
}

// scopeKey identifies a class/group/instance selection.
func scopeKey(classID, groupID string, instanceIDs []string) string {
	return "c=" + strings.ToLower(classID) + "|g=" + strings.ToLower(groupID) + "|i=" + idSetKey(instanceIDs)
}

// cacheKey joins key parts with a separator that can't appear in SCOM names.
func cacheKey(parts ...string) string {
	return strings.Join(parts, "\x00")
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
