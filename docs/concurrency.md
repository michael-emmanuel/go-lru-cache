# Concurrency

## Shared mutable state

A `Cache` is shared by every goroutine that holds a pointer to it. The
mutable state is:

| State | Mutated by |
|-------|------------|
| `items` (the map) | `Set` (insert, and delete on eviction), `Delete`, `Clear` |
| `head.next`, `tail.prev` | any operation that adds, removes or moves a node |
| each node's `prev`, `next` | `Get` (hit), `Set`, `Delete`, eviction |
| each node's `value` | `Set` on an existing key |

`capacity` is the one field that is not mutated after `New` returns.

Note the first row of the second column: `Get` appears. That is the central
fact of this design, discussed below.

## Data races and Go maps

A data race occurs when two goroutines access the same memory location
concurrently, at least one access is a write, and there is no
synchronization ordering them. Races are undefined behavior in the sense
that the Go memory model gives no guarantee about what a racing read
observes.

Go maps are not safe for concurrent use when any goroutine writes. The
runtime actively detects some misuse and aborts with
`fatal error: concurrent map read and map write` or
`concurrent map writes`. This is a crash, not a recoverable panic. A map
write may also trigger growth, which rearranges internal storage while a
concurrent reader may be walking it. Therefore every access to `items`,
reads included, must be ordered with respect to writes.

## Why the list must be synchronized together with the map

The map and the list encode the same set of entries. An operation like
eviction is two steps: unlink the node, delete the map entry. If another
goroutine runs between those steps, it can observe, and act on, a state
where the node is gone from the list but present in the map (or the reverse).
Locking the map and the list separately would protect each data structure
individually and still permit that inconsistent view. One lock covers both,
so the pair changes atomically.

The same reasoning applies within a single operation. `Get` does a lookup
then a move. If the lock were dropped between them, another goroutine could
evict the node after the lookup, and `Get` would then "move" a node that is
no longer in the list, writing through stale pointers into its former
neighbors.

## Mutexes and critical sections

A `sync.Mutex` provides mutual exclusion: at most one goroutine holds it.
The code between `Lock` and `Unlock` is the critical section.

In this cache the critical section of every public method is the whole
method body (`defer c.mu.Unlock()` immediately after `Lock`). That is
deliberately coarse. The work inside is small (a map operation and a few
pointer writes), there is no I/O or user callback inside, and a coarse
critical section is easy to prove correct. Narrowing it would not
meaningfully shorten hold times and would create windows where the map and
list disagree.

`defer` guarantees the unlock even if the body panics. That matters for
the case where `K` is an interface holding an unhashable value: the map
operation panics, the lock is released, and the cache remains usable (see
`TestUnhashableInterfaceKeyPanicsButLeavesCacheUsable`).

## Happens-before, at a practical level

The Go memory model says that for a `sync.Mutex`, the n-th call to `Unlock`
happens before the (n+1)-th call to `Lock` returns. In practice: everything
goroutine A wrote before it unlocked is visible to goroutine B after B
locks. That is what makes it safe for goroutine B to read pointers that A
just wrote. Without the lock there is no such ordering, and B may see a
partially updated list, or stale values from CPU caches or compiler
reordering.

`Capacity` reads a field without locking. That is safe because the field is
written once in `New`, before the `*Cache` is shared. Whatever mechanism
publishes the pointer to other goroutines (a channel send, a `go`
statement, a mutex, an atomic) provides the happens-before edge that makes
the earlier write visible. Reading an immutable field needs nothing beyond
what is already required to share the cache at all.

## Why Get mutates cache state

An LRU cache's ordering must reflect reads. A `Get` that did not update
recency would turn the policy into FIFO. So a hit performs `moveToFront`:

```
removeNode(n)         n.prev.next = n.next
                      n.next.prev = n.prev
addToFront(n)         n.prev = head;  n.next = head.next
                      head.next.prev = n
                      head.next = n
```

That is writes to the neighbors' pointers, to `n`'s pointers, and to
`head.next`. It is a write operation that happens to be spelled "Get".

## Why `RLock -> lookup -> move` is incorrect

```go
// WRONG
c.mu.RLock()
n, ok := c.items[key]
if ok {
    c.moveToFront(n)   // writes to shared state under a read lock
}
c.mu.RUnlock()
```

`RLock` admits any number of concurrent holders. Two goroutines can then
run `moveToFront` at the same time. Consider two `Get`s on keys `B` and `C`
in the list `HEAD <-> A <-> B <-> C <-> TAIL`:

- Both unlink their node. Goroutine 1 sets `A.next = C` (skipping B) while
  goroutine 2 sets `B.next = TAIL` (skipping C). Whichever write lands
  last wins, and one of the removals is lost.
- Both insert after `HEAD`. Both read `head.next == A`, both write
  `head.next`, and one node is dropped from the forward chain while its
  `prev` pointer still refers into the list.

The result is a list where the forward chain, the backward chain and the
map disagree: a lost node, a node reachable from the map but not the list,
or a cycle. The race detector reports these as data races on the pointer
fields. Making the pointer writes atomic one at a time would not help; the
operation needs multi-word atomicity, which only mutual exclusion gives.

## Mutex vs RWMutex

`sync.RWMutex` allows many concurrent readers or one writer. It pays off
when (a) most operations are genuinely read-only and (b) critical sections
are long enough that parallel readers save more time than the extra
bookkeeping costs.

Neither holds here.

1. **Reads are not read-only.** `Get` is the dominant operation and it
   mutates. It needs the exclusive lock. The only operations that could use
   `RLock` are `Len` and (if it took the lock) `Capacity`, which are rarely
   on a hot path.
2. **Critical sections are tiny.** A hit is a map lookup and four pointer
   writes. With such short sections, the cost is dominated by the lock
   itself. `RWMutex` has more state to update per acquisition (reader
   counts) than `Mutex`, so the uncontended fast path is slower.
3. **Readers can add contention under load.** With a writer pending,
   `RWMutex` blocks new readers, and all readers contend on the shared
   reader counter. An `RWMutex` is not automatically better for read-heavy
   workloads.

A plain `Mutex` is simpler, correct, and at least as fast for this
operation mix. If a variant existed where hits did not mutate shared state
(for example a design that records recency approximately via per-entry
atomic timestamps, or buffers recency updates), `RWMutex` might become
worthwhile, but that is a different design with different tradeoffs and
should be justified by benchmarks.

## Deadlock considerations

Deadlock requires a goroutine to wait for a lock it cannot get. Here:

- There is one lock, so there is no lock-ordering cycle.
- Internal helpers never call `Lock`, so no method re-enters the lock
  (Go mutexes are not reentrant: a goroutine that locks a mutex it already
  holds blocks forever).
- No user code runs inside the critical section. There are no callbacks, no
  loader functions and no channel operations, so user code cannot block while
  holding the lock or call back into the cache.
- Keys and values are stored, not inspected, except that the map calls the
  key's equality and hash functions. For built-in comparable types these
  cannot block.

If a future feature adds eviction callbacks or a loader, call them *after*
releasing the lock, or they become a deadlock hazard (a callback that calls
`cache.Get` would self-deadlock).

## Lock scope

Lock scope is "whole method". There is no lock-free fast path, no
fine-grained locking per node, and no lock held across more than one
public call. Callers who need compound atomicity (for example "get, and if
absent compute and set") must provide it themselves; two separate calls are
not atomic together. That gap is how cache stampedes arise (see
[production-considerations.md](production-considerations.md)).

## Contention

Because every operation takes the same lock, operations are fully
serialized. With `G` goroutines calling the cache, throughput cannot exceed
one critical section at a time. When the critical section is short and the
callers do other work between calls, contention is low. When callers hit the
cache in a tight loop on many cores, goroutines queue on the mutex, the lock
word bounces between cores' caches, and extra cores stop helping. Go's mutex
has a fast path for the uncontended case and falls back to parking
goroutines under contention, which keeps the cache correct but adds latency.

How bad this gets depends on core count, call rate and work done outside
the cache. It should be measured with `go test -bench -cpu=1,2,4,8`, a mutex
profile (`-mutexprofile`) and a block profile on the target workload, not
assumed.

## What would break without the mutex

Remove the mutex and the cache is incorrect in several independent ways:

- Concurrent map access aborts the process with a fatal runtime error.
- Racing list updates lose nodes, create cycles or leave dangling pointers.
- The map and list diverge, so `Len` is wrong and eviction can delete the
  wrong entry or none.
- `Len` and `Get` can observe torn or stale state.

The test suite demonstrates this: removing the lock from `Get` or `Len`
makes `go test -race` report data races and fail several concurrency tests.

## How correctness is checked

- `go test -race ./...` is part of the validation process. The race
  detector instruments memory accesses and reports unsynchronized ones at
  run time. It can only find races that actually execute, so the tests are
  built to create contention: a start barrier releases all goroutines
  together, the key space is small relative to the number of goroutines,
  and the capacity is small so that evictions happen constantly.
- Values are a function of the key, so any `Get` that returns a mismatched
  value is detected without needing to know the interleaving.
- Invariants are checked under the lock after the workload and periodically
  during the stress test.
- No test uses `time.Sleep` for synchronization.

The race detector is evidence, not proof. It does not find races on code
paths that never run, and it says nothing about logical errors that are
properly synchronized (for example evicting the wrong node). The
model-based test covers those.
