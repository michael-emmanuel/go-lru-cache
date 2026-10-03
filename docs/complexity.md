# Complexity Analysis

Four separate quantities are easy to conflate, so they are distinguished
here:

1. **Hash-map complexity.** Average O(1) for lookup, insert and delete. Not
   O(1) worst case.
2. **Linked-list complexity.** Given a pointer to a node, unlink and link are
   O(1) in the worst case: a fixed number of pointer assignments.
3. **Cache-operation complexity.** The sum of the above for each operation.
   Because the O(1) parts are all constant and the map part is O(1) on
   average, the cache operation is O(1) *average*.
4. **Space complexity.** O(capacity).

Lock acquisition is treated as O(1) work in the uncontended case. Under
contention, wait time depends on how many other goroutines hold the lock;
that is a latency effect, not an algorithmic one, and is discussed in
[concurrency.md](concurrency.md).

## Get

1. **Work.** Lock; one map lookup; on a hit, `moveToFront`: one comparison
   plus up to 8 pointer assignments (4 in `removeNode`, 4 in `addToFront`);
   unlock.
2. **Dominant cost.** The map lookup (hashing the key, probing a bucket)
   plus, in practice, cache misses from touching nodes in scattered memory.
3. **Why O(1) average.** Map lookup is O(1) average. The move touches the
   node, its two neighbors, and `head`/`head.next`; all are reachable
   through pointers, so no traversal occurs. A constant number of steps
   plus an average-O(1) lookup is O(1) average.
4. **Anything O(n)?** No.
5. **Space.** O(1) extra. A hit allocates nothing (benchmarks report
   0 allocs/op).

**Why `Get` does not become O(n).** The question to ask of any list
operation is "how does it find the node?" Here the answer is "from the map".
`Get` never walks the list. `moveToFront` needs the node's `prev` and
`next`, which the node itself holds. If the implementation had to search the
list for the key, `Get` would be O(n); the map exists specifically to avoid
that.

## Set

1. **Work.** Lock; one map lookup. Update path: assign value, `moveToFront`.
   Insert path: possibly `removeLRU`, allocate a node, `addToFront`, map
   insert; unlock.
2. **Dominant cost.** Map lookup and insert, and on insertion the node
   allocation.
3. **Why O(1) average.** `removeLRU` reads `tail.prev` (O(1)), unlinks it
   (O(1)), and deletes from the map (average O(1)). At most one eviction
   occurs per `Set`, because the cache holds `capacity` entries before the
   insert and one is removed to make room for one.
4. **Anything O(n)?** A map insert that triggers growth costs time
   proportional to the map size for that one call. This is amortized over
   many inserts and is part of why map complexity is stated as *amortized*
   average. It is bounded by capacity, since the map never holds more than
   `capacity` entries. Eviction before insertion avoids one unnecessary
   growth trigger.
5. **Space.** O(1) per new entry (one node, one map slot). Insertion into a
   full cache is net zero: one node is released for collection, one
   allocated.

## Delete

1. **Work.** Lock; map lookup; `removeNode`; `delete` from the map; unlock.
2. **Dominant cost.** The map operations.
3. **Why O(1) average.** Lookup and delete are average O(1); unlink is
   constant.
4. **Anything O(n)?** No. (Go does not shrink map storage on delete.)
5. **Space.** O(1). Frees one node for collection.

## Len and Capacity

`Len` returns `len(items)`, which Go maintains as a counter: O(1). It takes
the lock so that the value is consistent with the most recent completed
operation. `Capacity` returns an immutable field: O(1), no lock.

## Clear

1. **Work.** Lock; `clear(items)`; reconnect `head.next` and `tail.prev`;
   unlock.
2. **Dominant cost.** Emptying the map, which visits every slot.
3. **Why O(n).** `clear` on a map must reset all of its storage; there is no
   way to do that without work proportional to the map size. The linked list
   is not walked: nodes are not individually unlinked. Once `head` and `tail`
   are re-linked to each other and the map no longer references the old
   nodes, they are unreachable and the garbage collector reclaims them
   (the GC work is also proportional to the number of dead nodes, but is
   not on the caller's critical path).
4. **Anything else O(n)?** Only this.
5. **Space.** O(1) extra. The map keeps its allocated storage.

## Space complexity

For `n <= capacity` stored entries:

```
space = O(n)   which is   O(capacity) at most
```

Per entry: one map slot (key and a pointer, plus the map's own overhead) and
one node (key, value, two pointers). Constant-sized metadata (the mutex, the
two sentinels, capacity, map header) adds O(1). See
[production-considerations.md](production-considerations.md) for what this
means in bytes.

## Summary

| Operation | Time (average) | Worst-case notes | Extra space |
|-----------|----------------|------------------|-------------|
| Get | O(1) | Map worst case not constant | O(1) |
| Set | O(1) amortized | Map growth O(capacity) once in a while | O(1) per new entry |
| Delete | O(1) | Map worst case not constant | O(1) |
| Len | O(1) | | O(1) |
| Capacity | O(1) | | O(1) |
| Clear | O(n) | | O(1) |

## Empirical check

`BenchmarkCapacityScaling` measures `Get` at capacities from 256 to about one
million, a range of roughly 4000x. If `Get` were O(n), time per operation
would grow by about that factor. In repeated runs in a single-core container
it grew from about 31 ns to between 75 and 91 ns, roughly 2.5 to 3 times.
That is consistent with CPU cache and TLB effects of a larger pointer-linked
working set rather than linear growth, but a benchmark alone cannot separate
those explanations; a profiler or hardware counters would be needed to
attribute the cost precisely. Short benchmark runs were noisy (one
reported 214 ns at the largest size), so use `-count` and `benchstat` when
comparing. Big-O describes operation counts; it says nothing about the
memory hierarchy, which can dominate at large sizes.
