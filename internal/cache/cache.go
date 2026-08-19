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
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

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
// could not stop waiting at all.
func (c *Cache[V]) Get(ctx context.Context, key string, fetch func(context.Context) (V, error)) (V, error) {
	if v, ok := c.lookup(key); ok {
		return v, nil
	}

	ch := c.group.DoChan(key, func() (any, error) {
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
			return zero, nil
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

// evictLocked makes room for one insert: expired entries first, then the
// oldest. Preferring expired entries keeps a burst of distinct keys from
// throwing away live values while dead ones sit in the map.
func (c *Cache[V]) evictLocked() {
	now := c.now()
	for el := c.order.Front(); el != nil && c.order.Len() >= c.maxEntries; {
		next := el.Next()
		if e, ok := el.Value.(*entry[V]); ok && !now.Before(e.expires) {
			c.removeLocked(el)
		}
		el = next
	}
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
