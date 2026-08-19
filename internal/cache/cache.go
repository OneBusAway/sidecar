// Package cache memoizes one value per key for a fixed TTL, collapsing
// concurrent misses into a single fetch. It exists so that N riders searching
// the same stop, or N instances of the same keystroke, cost one upstream call
// rather than N.
//
// It reads no clock of its own: the design bans time.Now outside cmd/, and a
// cache whose expiry cannot be advanced by a test is a cache with untestable
// expiry.
package cache

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// errUnexpectedType is returned if a value pulled out of singleflight or the
// entry list didn't have the type Get was instantiated with. A *Cache[V]
// only ever stores V, so this should be unreachable -- but handing back a
// silent zero value on a broken invariant would be worse than an error.
var errUnexpectedType = errors.New("cache: value has unexpected type")

// Cache memoizes one value per key for a fixed TTL. It is safe for concurrent
// use.
//
// There is deliberately no Set. A caller that could Set could store a value
// whose age the cache never measured, and the entire contract here is "this
// value is at most ttl old".
type Cache[V any] struct {
	ttl        time.Duration
	maxEntries int
	budget     time.Duration
	now        func() time.Time

	group singleflight.Group

	mu    sync.Mutex
	items map[string]*list.Element
	// order is oldest-first, so eviction reads from the front.
	order *list.List
}

type entry[V any] struct {
	key     string
	value   V
	expires time.Time
}

// New builds a cache holding at most maxEntries values, each valid for ttl.
// budget bounds a single fetch; see Get.
func New[V any](ttl time.Duration, maxEntries int, budget time.Duration, now func() time.Time) *Cache[V] {
	if maxEntries < 1 {
		maxEntries = 1
	}
	return &Cache[V]{
		ttl:        ttl,
		maxEntries: maxEntries,
		budget:     budget,
		now:        now,
		items:      make(map[string]*list.Element, maxEntries),
		order:      list.New(),
	}
}

// Get returns the cached value for key, or calls fetch and stores the result.
//
// Concurrent Gets for the same key while a fetch is in flight share that one
// fetch. Errors are returned to every waiter and are never cached: caching a
// failure would turn a brief upstream blip into an outage lasting a full ttl.
//
// Context handling is the subtle part, because two requirements pull against
// each other. A caller whose request is cancelled must stop waiting; the
// shared fetch must not die with that caller, since other waiters still need
// its result. So the fetch runs on a context detached from every caller
// (context.WithoutCancel) carrying a fresh budget measured from fetch start,
// while Get itself selects on the caller's ctx.Done(). That also forces
// DoChan rather than Do -- Do blocks uninterruptibly, so a cancelled caller
// could not stop waiting at all. WithoutCancel also carries forward whatever
// request-scoped values live on the context of whichever caller happened to
// win the race to start the shared fetch; that's deliberate -- there is no
// single "right" caller to attribute a shared fetch to, and the alternative
// (no values at all) is no more correct.
func (c *Cache[V]) Get(ctx context.Context, key string, fetch func(context.Context) (V, error)) (V, error) {
	if v, ok := c.lookup(key); ok {
		return v, nil
	}

	ch := c.group.DoChan(key, func() (any, error) {
		// A caller can lose the race between missing in the lookup above and
		// registering here: by the time it reaches DoChan, a different
		// in-flight call for this key may already have finished and been
		// stored. Re-checking here -- rather than papering over the gap with
		// a sleep in the caller -- turns "usually one upstream call" into
		// "always one," because a late arrival now costs a mutex lock
		// instead of a second fetch.
		if v, ok := c.lookup(key); ok {
			return v, nil
		}

		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.budget)
		defer cancel()

		v, err := fetch(fetchCtx)
		if err != nil {
			return nil, err
		}
		c.store(key, v)
		return v, nil
	})

	var zero V
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return zero, res.Err
		}
		v, ok := res.Val.(V)
		if !ok {
			return zero, errUnexpectedType
		}
		return v, nil
	}
}

// Len reports how many entries are held. It exists for tests asserting the
// entry cap.
func (c *Cache[V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

func (c *Cache[V]) lookup(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var zero V
	el, ok := c.items[key]
	if !ok {
		return zero, false
	}
	e, ok := el.Value.(*entry[V])
	if !ok {
		// A broken invariant, not a miss: without removing it, every future
		// lookup would re-hit this same non-*entry[V] element forever.
		c.removeLocked(el)
		return zero, false
	}
	if !c.now().Before(e.expires) {
		c.removeLocked(el)
		return zero, false
	}
	return e.value, true
}

func (c *Cache[V]) store(key string, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[key]; ok {
		c.removeLocked(el)
	}
	c.evictLocked()
	c.items[key] = c.order.PushBack(&entry[V]{
		key:     key,
		value:   v,
		expires: c.now().Add(c.ttl),
	})
}

// evictLocked makes room for one insert by dropping the oldest entry.
//
// Oldest-first is also expired-first here, but only as a consequence of one
// invariant: every entry shares a single cache-wide ttl, and store always
// stamps a new entry's expiry as c.now().Add(c.ttl) off a monotonic,
// non-decreasing clock. That pins insertion order and expiry order to the
// same sequence, so the front of the list is always both the oldest entry
// and, if anything in the cache has expired, the first one that did. A
// second pass that specifically hunts for expired entries would therefore
// never find one this oldest-first loop hadn't already reached -- it would
// just be a slower path to the identical answer, which is why there isn't
// one. That equivalence breaks the moment ttl becomes per-entry instead of
// per-cache: an entry stored later with a shorter ttl could expire before
// an older entry with a longer one, and oldest-first alone would then evict
// the wrong one.
func (c *Cache[V]) evictLocked() {
	for c.order.Len() >= c.maxEntries {
		front := c.order.Front()
		if front == nil {
			return
		}
		c.removeLocked(front)
	}
}

func (c *Cache[V]) removeLocked(el *list.Element) {
	if e, ok := el.Value.(*entry[V]); ok {
		delete(c.items, e.key)
	}
	c.order.Remove(el)
}
