# Production Considerations

This document describes what would have to change before this cache is used
inside a larger production system. The implementation is a correct,
single-lock, process-local LRU. It is a sound starting point, not a drop-in
replacement for a tuned library. Nothing below has been validated at scale;
statements about behavior under load are reasoning to be confirmed by
measurement.

## Memory model

For each entry the cache holds:

- a map slot: the key, a pointer to the node, and the map's per-slot
  overhead;
- a separately heap-allocated node: `key`, `value`, `prev`, `next`.

For `Cache[int, int]` on a 64-bit platform the node is 32 bytes (8 + 8 + 8 +
8), which matches the 32 B/op reported by `BenchmarkSet/insert-with-eviction`.
For `Cache[string, T]` the key header is 16 bytes (pointer + length) and the
string bytes live elsewhere. The Go allocator rounds each allocation up to a
size class, so a 40-byte node occupies 48 bytes.

An LRU cache has higher overhead than a plain `map[K]V` because it adds, per
entry, two pointers (16 bytes), a second copy of the key (in the node), an
allocation header/rounding, and a map slot that holds a pointer rather than
the value inline. The cost buys O(1) eviction ordering.

Effects of the pointer-heavy layout:

- **Allocations.** Each new entry performs one heap allocation for its
  node. High churn (frequent evictions) means a steady allocation rate.
  A free-list or `sync.Pool` of nodes could reduce this but adds
  complexity and a risk of use-after-free-style bugs; it was not done.
- **Cache locality.** Nodes are scattered across the heap, so walking from
  map to node to neighbors incurs CPU cache misses. This is a likely
  contributor to the slowdown seen at large capacities in
  `BenchmarkCapacityScaling`.
- **Garbage collection.** Every node is a pointer-containing object the GC
  must trace and every `prev`/`next` is a pointer to follow. Large caches
  increase mark time and heap size. Setting `GOGC` or a soft memory limit
  (`GOMEMLIMIT`) interacts with this.
- **Footprint.** If `V` is a pointer or contains pointers, the cache keeps
  the referent alive until eviction. A cache of large objects can hold far
  more memory than its entry count suggests.

An array-backed list (nodes in a slice, links as `int32` indices) would
improve locality and shrink the footprint, at the expense of readability.
It is a reasonable optimization if profiling shows allocation or GC cost is
significant.

## Capacity

Capacity counts entries, not bytes. It governs three things together:

- **Hit rate.** A larger cache holds more of the working set and misses
  less, with diminishing returns that depend on the access distribution. If
  the working set fits, hit rate approaches the compulsory-miss limit. If
  accesses are close to uniform over a key space much larger than capacity,
  hit rate is roughly capacity / key space no matter what policy is used.
- **Memory.** Memory grows linearly with capacity times average entry size.
  Variable-size values make entry-count capacity a poor proxy for bytes.
- **Eviction frequency.** Smaller capacity means more evictions per second,
  so more allocation churn, more recompute at the backing store, and more
  time spent in the cache's write path.

Choosing capacity should be driven by measured hit rate versus memory on
representative traffic, not guessed. LRU is also vulnerable to scans: a
single pass over many keys pushes the whole working set out. If that is a
real access pattern, a scan-resistant policy (2Q, ARC, TinyLFU-style
admission) is warranted.

## Contention

Every operation takes one mutex, so operations are serialized. Under heavy
parallel load from many cores, goroutines queue on the lock and the lock
word's cache line moves between cores, so adding cores can stop improving
throughput. Because `Get` writes, there is no read-only fast path to
exploit.

Whether this matters depends on the workload and must be measured. Tools:
`go test -bench . -cpu=1,2,4,8`, `-mutexprofile`, `-blockprofile`, and
the application's own latency histograms.

Options if contention is demonstrated, from least to most invasive:

1. **Reduce calls.** Batch or memoize at the call site; cache closer to the
   consumer so fewer goroutines share one instance.
2. **Sharding.** Split into `N` independent caches, choose a shard with
   `hash(key) % N`, and give each its own mutex. Contention drops roughly by
   a factor of `N` if keys spread evenly. Tradeoffs: LRU order is per
   shard, not global, so eviction is approximate; capacity is split across
   shards, so a hot shard can evict while others have room; the key must be
   hashed (with a stable hash for the key type); `Len` and `Clear` must visit
   all shards. This is deliberately not implemented here.
3. **Workload partitioning.** Give different tenants or key classes their
   own caches.
4. **Relaxed recency.** Record recency updates in a buffer and apply them in
   batches (the approach used by some high-throughput cache libraries), so
   hits do not take the exclusive lock on every call. This weakens strict
   LRU semantics and is considerably more complex.

## Metrics

Useful counters and gauges, in rough priority:

- hits and misses (hit ratio is the primary measure of effectiveness);
- evictions (a rising eviction rate with a falling hit ratio indicates the
  cache is too small or the access pattern changed);
- current entries versus capacity;
- operation latency, especially p99, and time spent waiting on the lock;
- bytes held, if entries can be sized.

Hit/miss/eviction counters can be plain `int64` fields updated under the
existing lock, or atomics. An eviction callback would let callers do their
own accounting or cleanup, but must run after the lock is released to avoid
deadlock and long critical sections.

## Observability

- **Metrics:** export the counters above through the application's metrics
  system (Prometheus, OpenTelemetry metrics, and so on). The cache package
  itself should not depend on one.
- **Structured logging:** log rare, high-signal events (configuration, a
  sudden change in hit ratio detected by the application). Do not log
  per-operation; at cache call rates that would dominate cost.
- **Tracing:** trace the miss path (the load from the backing store), not
  the hit path. A span around a few hundred nanoseconds of in-memory work
  costs more than the work.
- **Profiling:** CPU, allocation, mutex and block profiles are the
  evidence for any optimization.

## TTL

To add time-based expiry:

- Store an `expiresAt` in each node (a monotonic-clock-based timestamp).
- **Lazy expiry:** on `Get`, if the entry is expired, treat it as a miss and
  remove it. Cheap, but expired entries that are never read occupy capacity
  until evicted.
- **Active expiry:** a background goroutine periodically scans and removes
  expired entries. A full scan under the lock is O(n) and stalls callers;
  alternatives are scanning in small batches or maintaining a min-heap or
  timing wheel keyed by expiry (O(log n) per insert/update instead of O(1)).
- **Interaction with LRU:** expiry and recency are independent. An entry
  can be both recently used and expired. Decide whether `Get` refreshes the
  TTL (sliding expiration) or not (absolute expiration), and whether an
  expired entry should still count against capacity until removed.
- The clock should be injectable for tests; never use `time.Sleep` to test
  expiry.

TTL bounds staleness but does not prevent it: a value can be stale for up to
its TTL.

## Distributed systems

This cache lives in one process's memory. Two instances of an application
each have their own cache with no knowledge of each other. This is a
deliberate scope limit, because distribution requires solving problems the
data structure does not address.

If multiple instances need to share cached data, the main options are:

- **External shared cache** (such as Redis or Memcached). Entries are shared
  and survive application restarts, at the price of a network hop per
  operation, a new failure domain, and serialization of values. An
  in-process LRU is often still kept in front of it as a "near cache".
- **Consistent hashing / routing.** Route each key to a single owning
  instance so each key is cached once. This needs membership tracking and
  rebalancing when instances join or leave.
- **Invalidation.** With a near cache, other instances' copies must be
  invalidated or must expire when data changes; pub/sub invalidation
  messages are common. Without invalidation, instances serve inconsistent
  values until TTL or eviction.

Coherence, replication and partition behavior are all consistency-versus-
availability decisions that depend on the application. None is solved here.

## Cache stampede

An LRU cache does not prevent a thundering herd. If a popular key is absent
or expires, every concurrent caller that sees the miss will independently
compute the value (database query, RPC) and `Set` it. The cache offers no
way to say "someone is already loading this key".

The usual remedy is request coalescing ("single flight"): the first miss
for a key starts the load and later callers wait for its result. This can be
done by the caller with `golang.org/x/sync/singleflight`, or inside the
cache with a `GetOrLoad(key, loadFn)` method that tracks in-flight loads.
Related techniques: jittering TTLs so many keys do not expire together,
serving stale values while refreshing in the background, and negative
caching to avoid repeated loads of nonexistent keys (cache penetration).

## Failure modes

| Failure | What happens | Detection / mitigation |
|---------|--------------|------------------------|
| Memory pressure | Capacity bounds entry count, not bytes. Large values can exhaust memory while the cache is "within capacity". The GC works harder as the heap grows. | Track heap size, GC pause and `GOMEMLIMIT`; size capacity from measured entry sizes; consider byte-weighted capacity. |
| Excessive eviction | Capacity too small or scan pattern; hit ratio drops, load on the backing store rises. | Eviction rate and hit-ratio metrics; resize or change policy. |
| Lock contention | Tail latency rises with parallelism; throughput plateaus. | Mutex and block profiles; sharding. |
| Stale values | No TTL or invalidation; values persist until evicted. | Add TTL; invalidate on write; document staleness tolerance. |
| Cache misses (cold start, flush) | Backing store sees full load. | Cache warming; stampede protection; rate limiting loads. |
| Process restart | All contents lost. | The cache must never be the only copy of data; warm after start if needed. |
| Unhashable interface key | Runtime panic from the map; cache remains consistent and unlocked. | Use concrete key types; validate keys at boundaries. |
| Misuse: copying a Cache | Copies share the map and list but not the mutex; undefined behavior. | `go vet` copylocks check; pass `*Cache`. |

## Beyond this project's scope

Not implemented, on purpose: sharding, TTL, metrics, loaders, eviction
callbacks, byte-weighted capacity, persistence, distribution. Each adds API
surface and interacts with the locking design. The project demonstrates the
core mechanism and where it would be extended.
