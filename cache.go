// Package lru implements a generic, fixed-capacity, least-recently-used
// (LRU) cache that is safe for concurrent use.
//
// # Design
//
// A Cache combines two data structures:
//
//   - a hash map from key to *node, which gives O(1) average lookup, and
//   - a doubly linked list of nodes ordered by recency of use, which gives
//     O(1) removal, O(1) insertion at the front, and O(1) eviction from the
//     back.
//
// The list is bracketed by two sentinel nodes, head and tail. The node
// after head is the most recently used (MRU) entry; the node before tail
// is the least recently used (LRU) entry.
//
// # Capacity
//
// The capacity passed to New is the maximum number of entries. It counts
// entries, not bytes. When Set would add an entry to a full cache, the
// least recently used entry is evicted first, so Len never exceeds
// Capacity.
//
// # Recency
//
// An entry becomes the most recently used entry when it is inserted, when
// its value is replaced by Set, and when it is returned by a successful
// Get. Delete, Len, Capacity and Clear do not affect the relative order of
// other entries.
//
// # Concurrency
//
// A Cache is safe for concurrent use by multiple goroutines. All state is
// protected by a single sync.Mutex. Get updates recency ordering and is
// therefore a mutation; it takes the same exclusive lock as Set and
// Delete. Operations are linearizable: each one appears to take effect
// atomically at some point between its call and its return.
//
// A Cache must not be copied after first use. The zero value of Cache is
// not usable; construct one with New.
//
// # Complexity
//
// Get, Set and Delete run in O(1) average time. The average-case
// qualifier is inherited from Go's hash map, whose worst case is not
// constant. Len and Capacity are O(1). Clear is O(n) in the number of
// entries it removes. Space is O(Capacity).
//
// # Keys and values
//
// K must be comparable because it is used as a Go map key. If K is an
// interface type, a key whose dynamic type is not hashable (for example a
// slice) causes the map operation to panic. The cache remains consistent
// and unlocked after such a panic, because the panic occurs before any
// state is modified and the mutex is released with defer.
//
// Values are stored and returned by value. If V is a pointer, slice or map,
// the cache stores the reference and does not copy the referent.
package lru

import (
	"errors"
	"fmt"
	"sync"
)

// ErrInvalidCapacity is returned by New when the requested capacity is not
// greater than zero. Use errors.Is to test for it.
var ErrInvalidCapacity = errors.New("lru: capacity must be greater than zero")

// Cache is a fixed-capacity LRU cache. Create one with New.
//
// Invariants, which hold whenever mu is not held by an in-progress
// operation:
//
//  1. len(items) equals the number of real nodes between head and tail.
//  2. For every (k, n) in items, n.key == k and n is linked in the list
//     exactly once.
//  3. Every real node in the list is referenced by exactly one map entry.
//  4. head.prev == nil, tail.next == nil, and for every linked node x
//     other than the sentinels, x.prev.next == x and x.next.prev == x.
//  5. The cache is empty if and only if head.next == tail and
//     tail.prev == head.
//  6. 0 <= len(items) <= capacity.
type Cache[K comparable, V any] struct {
	// mu guards items and every pointer reachable from head and tail,
	// including each node's key, value, prev and next fields. capacity is
	// the one exception: it is written once in New and never modified, so
	// it may be read without the lock.
	mu sync.Mutex

	capacity int
	items    map[K]*node[K, V]

	// head and tail are sentinels. They never hold user data and are never
	// present in items. They exist so that linking and unlinking a node
	// never has to special-case an empty list or the first/last element.
	head *node[K, V]
	tail *node[K, V]
}

// New returns an empty Cache that holds at most capacity entries.
//
// It returns an error wrapping ErrInvalidCapacity if capacity is not
// greater than zero.
//
// The internal map is not pre-sized to capacity. Pre-sizing would avoid
// incremental map growth but would commit memory proportional to capacity
// up front, which is undesirable for caches that are configured large and
// fill slowly.
func New[K comparable, V any](capacity int) (*Cache[K, V], error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidCapacity, capacity)
	}
	c := &Cache[K, V]{
		capacity: capacity,
		items:    make(map[K]*node[K, V]),
		head:     &node[K, V]{},
		tail:     &node[K, V]{},
	}
	c.head.next = c.tail
	c.tail.prev = c.head
	return c, nil
}

// Get returns the value stored for key and reports whether the key was
// present. A successful Get makes the entry the most recently used.
//
// The boolean result is the only way to distinguish a missing key from a
// stored zero value.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	// Get reorders the recency list, so the map lookup and the
	// move-to-front must form one critical section. A read lock would let
	// two goroutines rewrite the same prev/next pointers concurrently.
	c.mu.Lock()
	defer c.mu.Unlock()

	n, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	c.moveToFront(n)
	return n.value, true
}

// Set stores value under key and makes the entry the most recently used.
//
// If key is already present its value is replaced and Len is unchanged.
// Otherwise a new entry is added, and if the cache was full the least
// recently used entry is evicted first.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if n, ok := c.items[key]; ok {
		n.value = value
		c.moveToFront(n)
		return
	}

	// Evict before inserting so the map never holds more than capacity
	// entries, even transiently. That avoids triggering a map growth for
	// an element that is about to be replaced.
	if len(c.items) >= c.capacity {
		c.removeLRU()
	}

	n := &node[K, V]{key: key, value: value}
	c.addToFront(n)
	c.items[key] = n
}

// Delete removes key from the cache and reports whether it was present.
func (c *Cache[K, V]) Delete(key K) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	n, ok := c.items[key]
	if !ok {
		return false
	}
	c.removeNode(n)
	delete(c.items, key)
	return true
}

// Len returns the number of entries currently in the cache.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// Capacity returns the maximum number of entries the cache can hold.
func (c *Cache[K, V]) Capacity() int {
	// No lock: capacity is assigned in New and never modified afterwards,
	// so concurrent reads cannot race with a write.
	return c.capacity
}

// Clear removes all entries. The capacity is unchanged.
//
// Clear is O(n) because the map must be emptied. The nodes themselves are
// not unlinked one by one: once head and tail are reconnected and the map
// no longer references them, the old nodes are unreachable and the garbage
// collector reclaims them.
func (c *Cache[K, V]) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	clear(c.items)
	c.head.next = c.tail
	c.tail.prev = c.head
}

// The methods below manipulate the linked list and the map. They assume
// c.mu is held and never lock it themselves; Go mutexes are not reentrant,
// so locking inside them would deadlock when called from a public method.
//
// All pointer surgery lives here, so the list invariants only need to be
// reasoned about in these four functions.

// addToFront links n immediately after head, making it the MRU node. n must
// not currently be linked.
func (c *Cache[K, V]) addToFront(n *node[K, V]) {
	first := c.head.next
	n.prev = c.head
	n.next = first
	c.head.next = n
	first.prev = n
}

// removeNode unlinks n from the list. It does not touch the map; callers
// that are discarding the entry must also delete it from c.items.
//
// n's own pointers are cleared so that a stale reference fails loudly (nil
// dereference) instead of silently corrupting the list, and so that an
// unlinked node does not keep its former neighbors alive.
func (c *Cache[K, V]) removeNode(n *node[K, V]) {
	n.prev.next = n.next
	n.next.prev = n.prev
	n.prev = nil
	n.next = nil
}

// moveToFront marks an already-linked node as the MRU node.
func (c *Cache[K, V]) moveToFront(n *node[K, V]) {
	if c.head.next == n {
		return // already MRU; skip four pointer writes
	}
	c.removeNode(n)
	c.addToFront(n)
}

// removeLRU unlinks the least recently used node and deletes its map entry.
// It is a no-op on an empty cache.
func (c *Cache[K, V]) removeLRU() {
	n := c.tail.prev
	if n == c.head {
		return
	}
	c.removeNode(n)
	delete(c.items, n.key)
}
