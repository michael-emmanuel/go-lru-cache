# Architecture

## 1. System overview

`lru.Cache[K, V]` is an in-process, fixed-capacity cache. It keeps at most
`capacity` key/value pairs and, when a new key arrives at a full cache,
discards the pair that was used least recently.

```
    +------------------------------------------------------+
    |                      Cache[K, V]                     |
    |                                                      |
    |   mu sync.Mutex      guards everything below         |
    |   capacity int       immutable after New             |
    |                                                      |
    |   +----------------------+                           |
    |   | items map[K]*node    |  key -> node, O(1) avg    |
    |   +----------+-----------+                           |
    |              |                                       |
    |              v                                       |
    |   +------------------------------------------------+ |
    |   | doubly linked list                              | |
    |   |                                                 | |
    |   | HEAD <-> C <-> B <-> A <-> TAIL                 | |
    |   |          MRU         LRU                        | |
    |   +------------------------------------------------+ |
    +------------------------------------------------------+
```

There is one package and no background goroutines. All work happens on the
caller's goroutine inside a critical section.

## 2. Component responsibilities

| Component | Responsibility |
|-----------|----------------|
| `items` map | Find the node for a key in O(1) average time. Defines membership: a key is cached if and only if it is in the map. |
| Linked list | Record recency order. Position encodes "how recently used". |
| `node` | Holds key, value and links. The key is stored so eviction can delete the map entry. |
| `head`, `tail` sentinels | Remove empty-list and boundary special cases from pointer code. Never hold data, never in the map. |
| `mu` | Makes every public operation atomic with respect to all others. |
| Helpers: `addToFront`, `removeNode`, `moveToFront`, `removeLRU` | The only code that edits `prev`/`next`. |
| Test-only `checkInvariants` | Verifies the invariants below after operations. |

## 3. Cache data model

```go
type Cache[K comparable, V any] struct {
    mu       sync.Mutex
    capacity int
    items    map[K]*node[K, V]
    head     *node[K, V] // sentinel, head.next is MRU
    tail     *node[K, V] // sentinel, tail.prev is LRU
}

type node[K comparable, V any] struct {
    key        K
    value      V
    prev, next *node[K, V]
}
```

`prev` points toward `head` (more recent). `next` points toward `tail` (less
recent).

## 4. Hash map / list relationship

Each cached entry exists in both structures and each structure holds a
reference to the other's view of it:

```
   items                          list
   +-----+----------+
   | "a" | *node ---+----------> [key:"a" value:1 prev next]
   +-----+----------+                   ^
                                        |
                          the same node is linked between its neighbors
```

The map points to the node; the node's `key` field points back to the map
entry by name. Two operations depend on this pairing:

- **Get/Set of an existing key:** map gives the node, list reorders it.
- **Eviction:** list gives the node at `tail.prev`, `node.key` identifies the
  map entry to delete.

If either structure is updated without the other, the cache is corrupt (see
Failure modes).

## 5. Request flow: Get

```
Get(key)
  |
  v
Lock
  |
  v
n, ok := items[key] ---- !ok ----> Unlock; return (zero V, false)
  |
  ok
  v
moveToFront(n)
     |-- n already head.next? yes -> done
     |-- removeNode(n)    unlink from current position
     '-- addToFront(n)    link after head
  |
  v
Unlock; return (n.value, true)
```

The lookup and the move are in one critical section. If the lock were
released between them, another goroutine could delete or evict `n`, and the
move would then operate on a node that is no longer in the list.

## 6. Request flow: Set

```
Set(key, value)
  |
  v
Lock
  |
  v
n, ok := items[key]
  |-- ok:  n.value = value; moveToFront(n); Unlock; return
  |
  '-- !ok:
        |
        v
      len(items) >= capacity ?
        |-- yes: removeLRU()     (see section 8)
        v
      n := &node{key, value}
      addToFront(n)
      items[key] = n
        |
        v
      Unlock
```

Eviction happens before insertion, so `len(items)` never exceeds `capacity`
even transiently, and the map is not asked to grow just to accommodate an
element that is about to be displaced.

## 7. Request flow: Delete

```
Delete(key)
  |
  v
Lock
  |
  v
n, ok := items[key] --- !ok ---> Unlock; return false
  |
  ok
  v
removeNode(n)         unlink from list
delete(items, key)    remove from map
  |
  v
Unlock; return true
```

## 8. Eviction flow

```
removeLRU()
  |
  v
n := tail.prev
  |-- n == head ? (cache empty)  -> return
  v
removeNode(n)            unlink: n.prev.next = n.next; n.next.prev = n.prev
delete(items, n.key)     the reason nodes store their key
```

Example, capacity 3, before `Set("D")`:

```
HEAD <-> A <-> C <-> B <-> TAIL        B is tail.prev, so B is evicted

HEAD <-> A <-> C <-> TAIL              after removeLRU()
HEAD <-> D <-> A <-> C <-> TAIL        after addToFront(D)
```

## 9. Concurrency model

One `sync.Mutex` guards the map, the list and every node field. Every
exported method except `Capacity` locks for its whole body and unlocks with
`defer`. Internal helpers assume the lock is held and never lock themselves
(Go mutexes are not reentrant). There is exactly one lock, so lock-ordering
deadlocks are impossible by construction. See
[concurrency.md](concurrency.md).

## 10. Invariants

Enforced by construction (all pointer edits go through four helpers) and
verified in tests by `checkInvariants`:

1. Every map entry points to exactly one node linked in the list.
2. Every real node in the list has exactly one map entry, under its own key.
3. No node appears twice in the list.
4. `head` precedes the first real node; `tail` follows the last.
5. Empty cache: `head.next == tail` and `tail.prev == head`.
6. Non-empty cache: `head.next != tail` and `tail.prev != head`.
7. `len(items)` equals the number of real nodes.
8. For every linked node `x`: `x.prev.next == x` and `x.next.prev == x`.
9. `0 <= len(items) <= capacity`.
10. A successful `Get` or any `Set` leaves that key at `head.next`.
11. Eviction removes exactly `tail.prev`.

Invariants 1-9 are structural and checked directly. Invariants 10 and 11 are
behavioral and checked by the ordering tests and the model-based test.

## 11. Failure modes

| Failure | Consequence | Mitigation in this project |
|---------|-------------|----------------------------|
| Node unlinked but map entry kept | `Get` returns a node with nil links; nil dereference or silent corruption | `removeNode` nils the node's pointers so misuse fails loudly; invariant checker; tests |
| Map entry deleted but node left linked | List longer than map; `Len` wrong; later eviction deletes wrong state | Invariant 7; delete tests assert order and invariants |
| Evicting the wrong end | Hot entries discarded | Eviction-order tests and model test |
| Data race on list pointers | Cycles, lost nodes, crashes | Single mutex; `go test -race` |
| Unhashable dynamic key (`K` is an interface) | Runtime panic from the map | Panic occurs before mutation; lock released by `defer`; covered by a test |
| Memory growth | Capacity counts entries, not bytes | Documented limitation |
| Contention | Throughput capped by one lock | Documented; sharding in future evolution |
| Process restart | All entries lost | Documented; cache is not a source of truth |

## 12. Complexity

| Operation | Time | Notes |
|-----------|------|-------|
| Get | O(1) average | map lookup average O(1); list move O(1) |
| Set | O(1) average | map lookup and insert; list ops O(1); at most one eviction |
| Delete | O(1) average | map lookup and delete; list unlink O(1) |
| Len, Capacity | O(1) | |
| Clear | O(n) | map must be emptied |

Space is O(capacity). See [complexity.md](complexity.md).

## 13. Design tradeoffs

- **Mutex over RWMutex.** Every operation that can be hot (`Get`, `Set`,
  `Delete`) mutates. See [concurrency.md](concurrency.md).
- **Sentinels over nil checks.** Slightly more memory, substantially simpler
  link/unlink code.
- **Pointer nodes over an array-backed list.** Easier to read and reason
  about; worse locality and more GC work.
- **Evict-then-insert.** Keeps `len(items) <= capacity` always true.
- **Entry-count capacity.** Simple and predictable; cannot bound bytes.
- **No pre-sized map.** Avoids committing memory up front for large
  capacities; costs incremental map growth while filling.
- **Minimal API.** Six methods. No callbacks, loaders, iterators or stats,
  each of which would add surface area and interact with the lock.

## 14. Future evolution

In rough order of how commonly they are needed:

1. **Metrics.** Hit, miss, eviction counters (cheap under the existing lock
   or via atomics) and an eviction callback.
2. **Loader with single-flight.** `GetOrLoad(key, fn)` to prevent stampedes.
3. **TTL.** Store an expiry in each node; check on `Get`; add a janitor or
   lazy purge. See [production-considerations.md](production-considerations.md).
4. **Sharding.** `N` independent caches selected by `hash(key) % N`, each
   with its own lock. Approximate global LRU in exchange for scalability.
5. **Cost-based capacity.** Weight entries by size to bound bytes.
6. **Different policy.** LFU, 2Q or TinyLFU-style admission to resist scans.
7. **Array-backed list.** Replace pointers with indices into a slice to
   improve locality and reduce GC pressure, if profiling justifies it.
