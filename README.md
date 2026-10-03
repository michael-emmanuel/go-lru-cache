# LRU Cache in Go

A generic, fixed-capacity, concurrency-safe least-recently-used (LRU) cache
implemented from first principles with a hash map and a hand-written doubly
linked list.

## Overview

The package `lru` provides `Cache[K comparable, V any]`. It stores up to a
fixed number of entries and, when full, evicts the entry that was used least
recently. `Get`, `Set` and `Delete` run in O(1) average time.

The project deliberately avoids `container/list`, third-party cache
libraries and `sync.Map`. The goal is for the mechanism to be visible in the
source: a `map[K]*node[K,V]` for lookup, a doubly linked list with sentinel
head and tail nodes for recency order, and one `sync.Mutex` guarding both.

The module path in `go.mod` is `github.com/example/lru-cache`. Replace it
with your own path before publishing.

## Features

- Generic API: `Cache[K comparable, V any]`, no `interface{}` and no reflection.
- O(1) average-time `Get`, `Set`, `Delete`; O(1) eviction.
- Hash map plus doubly linked list with sentinel nodes; pointer manipulation
  confined to four small helpers.
- Safe for concurrent use; a single `sync.Mutex` protects all shared state.
- LRU eviction with precisely specified recency semantics.
- Unit tests, a randomized model-based test against a naive reference
  implementation, and an internal invariant checker.
- Concurrency tests designed to be run under the race detector.
- Benchmarks for hit, miss, insert, update, delete, mixed workloads and
  capacity scaling.

## Architecture

```
  map[K]*node[K,V]                 doubly linked list (recency order)

  +-------+-------+
  | "a"   |   ----+--------------------------+
  | "b"   |   ----+---------------+          |
  | "c"   |   ----+--+            |          |
  +-------+-------+  |            |          |
                     v            v          v
   HEAD <-> [ c ] <-> [ b ] <-> [ a ] <-> TAIL
   (sentinel) MRU                 LRU      (sentinel)
```

The map answers "where is the node for this key?" in O(1) average time. The
list answers "which entry is least recently used?" and supports unlinking
and relinking a node in O(1) because every node knows both neighbors. Each
node also stores its key so that evicting the node at the tail can delete the
matching map entry without searching.

See [docs/architecture.md](docs/architecture.md) for request flows and
[docs/data-structures.md](docs/data-structures.md) for the underlying
structures.

## Why Hash Map + Doubly Linked List?

Neither structure is sufficient alone.

- **Hash map alone.** Lookup is O(1) average, but a map has no notion of
  order. Finding the least recently used key would require scanning every
  entry (O(n)) or maintaining separate ordering metadata, which is the
  linked list.
- **Linked list alone.** Insertion at the front and removal of a known node
  are O(1), but finding the node for a key requires traversal (O(n)).
- **Combined.** The map provides the node pointer in O(1) average time. The
  list then reorders that node in O(1) without traversal. Eviction reads
  `tail.prev` and removes it, also O(1).

A doubly linked list is required because unlinking a node from the middle
needs its predecessor's `next` pointer, and in a singly linked list the only
way to reach the predecessor is to traverse from the head.

## Concurrency Model

All mutable state (the map, the list, and every node's fields) is guarded by
one `sync.Mutex`. Every exported method that touches that state acquires the
lock for its entire duration, so each operation is atomic with respect to
every other.

`Get` is not a read-only operation: a hit moves the node to the front of the
list, which rewrites four pointers. Using `sync.RWMutex.RLock` for `Get`
would allow two goroutines to rewrite the same pointers concurrently, so
`Get` takes the exclusive lock. A plain `Mutex` is used instead of
`RWMutex` because, with this design, only `Len` could use a shared lock, and
`RWMutex` carries more overhead than `Mutex`. `Capacity` needs no lock
because it is immutable after construction.

Correctness is validated with `go test -race`. See
[docs/concurrency.md](docs/concurrency.md).

## API

```go
func New[K comparable, V any](capacity int) (*Cache[K, V], error)

func (c *Cache[K, V]) Get(key K) (V, bool)
func (c *Cache[K, V]) Set(key K, value V)
func (c *Cache[K, V]) Delete(key K) bool
func (c *Cache[K, V]) Len() int
func (c *Cache[K, V]) Capacity() int
func (c *Cache[K, V]) Clear()

var ErrInvalidCapacity error // returned (wrapped) by New when capacity <= 0
```

```go
cache, err := lru.New[string, string](3)
if err != nil {
    log.Fatal(err)
}

cache.Set("user:1", "Alice")

value, ok := cache.Get("user:1") // "Alice", true
_, ok = cache.Get("user:2")      // "", false
```

Semantics worth knowing:

- A successful `Get`, and every `Set`, makes the entry most recently used.
- `Set` on an existing key replaces the value and does not change `Len`.
- `Get` returns `(zero, false)` for a missing key. Use the boolean, not the
  value, to detect misses; a stored zero value returns `(zero, true)`.
- The zero value of `Cache` is not usable. Always construct with `New`.
- A `Cache` must not be copied after first use (`go vet` reports this).

## Example

A complete program is in [examples/basic/main.go](examples/basic/main.go):

```go
package main

import (
	"fmt"
	"log"

	lru "github.com/example/lru-cache"
)

func main() {
	cache, err := lru.New[string, string](3)
	if err != nil {
		log.Fatal(err)
	}

	cache.Set("user:1001", "Alice")
	cache.Set("user:1002", "Bob")
	cache.Set("user:1003", "Charlie")

	if name, ok := cache.Get("user:1001"); ok {
		fmt.Println(name) // Alice; user:1002 is now least recently used
	}

	cache.Set("user:1004", "Diana") // evicts user:1002

	_, ok := cache.Get("user:1002")
	fmt.Println(ok) // false
}
```

Run it with `go run ./examples/basic`.

## Complexity

| Operation | Average Time | Space                        |
| --------- | ------------ | ---------------------------- |
| Get       | O(1)         | O(1)                         |
| Set       | O(1)         | O(1) amortized per new entry |
| Delete    | O(1)         | O(1)                         |
| Len       | O(1)         | O(1)                         |
| Capacity  | O(1)         | O(1)                         |
| Clear     | O(n)         | O(1)                         |

Total space is O(capacity). The "average" qualifier reflects Go's hash map:
lookups are O(1) in expectation under a good hash function, but the worst
case is not constant. Linked-list operations are O(1) worst case. See
[docs/complexity.md](docs/complexity.md).

## Testing

```
go test ./...
go test -race ./...
```

The race detector is part of the validation process, not an optional extra.
It requires cgo and a C toolchain on most platforms.

The suite contains:

- Behavioral unit tests for each operation and edge case (capacity 1 and 2,
  invalid capacity, zero values, struct keys, pointer values, delete from
  head, middle and tail).
- An explicit worked example (capacity 3, Set A, B, C, Get A, Set D) that
  asserts the exact recency order after every step.
- A model-based test that runs thousands of seeded random operations against
  a deliberately naive slice-based LRU and compares results and recency
  order after every step.
- A structural invariant checker (`checkInvariants`) called after operations
  in tests. It verifies pointer symmetry, absence of duplicates, map/list
  agreement and sentinel conditions.
- Concurrency tests with a start barrier, a small key space and no sleeps:
  mixed operations, `Clear` under contention, disjoint-key correctness,
  capacity bound, and a stress test.

During development the tests were checked by deliberately introducing bugs
(missing unlink, missing map delete, wrong `prev` pointer, evicting the wrong
end, removing the lock) and confirming each was detected.

## Benchmarks

```
go test -run='^$' -bench=. -benchmem ./...
```

or `make benchmark`. Benchmarks use `int` keys and values to isolate the data
structure from hashing and copy costs. For multi-core scaling, add
`-cpu=1,2,4,8`.

Sample results, for orientation only. Collected in a single-vCPU container
(Intel Xeon @ 2.10 GHz, Go 1.22.2) with `-count=4`; the range shown is the
spread across those runs. It says nothing about multi-core contention and
should not be compared across machines:

```
BenchmarkGet                              34-38 ns/op    0 B/op   0 allocs/op
BenchmarkSet/update-existing              ~37 ns/op      0 B/op   0 allocs/op
BenchmarkSet/insert-with-eviction        159-179 ns/op  32 B/op   1 allocs/op
BenchmarkDelete                           67-70 ns/op    0 B/op   0 allocs/op
BenchmarkCapacityScaling/capacity=256     31-32 ns/op
BenchmarkCapacityScaling/capacity=4096    34-37 ns/op
BenchmarkCapacityScaling/capacity=65536   53-62 ns/op
BenchmarkCapacityScaling/capacity=1048576 75-91 ns/op
```

Short runs (`-benchtime=200ms`) were noisier; one produced 214 ns/op at
capacity 1,048,576. Use `-count` and a tool such as `benchstat` before
drawing conclusions.

The capacity-scaling rows show per-operation time rising at larger
capacities while the entry count grows about 4000x. That is consistent with
CPU cache misses on a large, pointer-linked working set rather than
algorithmic growth, but the benchmark does not by itself prove that; use a
profiler or hardware counters to confirm on your hardware. Insertion costs
more than update because it allocates a node.

## Design Tradeoffs

- **Mutex vs RWMutex.** `Get` mutates recency order, so it needs exclusive
  access. A `Mutex` is simpler, cheaper per operation, and correct. See
  [docs/concurrency.md](docs/concurrency.md).
- **Sentinel nodes.** Dummy `head` and `tail` nodes remove every
  empty-list and end-of-list special case from link/unlink code, at the cost
  of two small allocations per cache.
- **Generics.** Type safety and no boxing of keys or values into `any`, at
  the cost of a more complex type signature. `K` must be `comparable`
  because it is a Go map key.
- **Memory overhead.** Each entry costs a map slot plus a separately
  allocated node (key, value, two pointers). A plain map stores keys and
  values inline in buckets. See
  [docs/production-considerations.md](docs/production-considerations.md).
- **Pointer-heavy list.** Nodes are scattered across the heap, which
  reduces CPU-cache locality and gives the garbage collector more pointers
  to trace. An array-backed list with integer indices would improve
  locality; it was not used because it makes the mechanism harder to read.
- **Map and list consistency.** The two structures must change together.
  This is why all mutation happens under one lock and why pointer surgery is
  centralized in `addToFront`, `removeNode`, `moveToFront` and `removeLRU`.
- **Contention.** One global lock serializes every operation. Under heavy
  multi-core load, the lock becomes the throughput ceiling. Sharding is the
  standard remedy and is intentionally out of scope.
- **Single-process scope.** The cache lives in one process's memory.

## Limitations

- Process-local only. There is no coherence between instances and no
  distribution.
- No TTL or expiry. Stale entries remain until evicted or deleted.
- No persistence. Contents are lost when the process exits.
- No cache-stampede (thundering herd) protection. Concurrent misses for the
  same key will each compute the value.
- No sharding. A single mutex limits scalability on many cores.
- No metrics (hit/miss/eviction counters) or eviction callbacks.
- Capacity counts entries, not bytes. Large values are not weighted.
- No negative caching or loader function (`GetOrLoad`).
- Interface-typed keys with unhashable dynamic types cause a runtime panic
  inherited from Go maps.
- `Clear` retains the map's allocated buckets (it uses the `clear` builtin),
  so memory is not returned to the runtime until the cache is discarded.

## Repository Layout

```
cache.go                        Cache, public API, list helpers, package docs
node.go                         linked-list node
cache_test.go                   unit tests, model test, invariant checker
cache_concurrency_test.go       race and stress tests
cache_benchmark_test.go         benchmarks
docs/                           architecture, data structures, concurrency,
                                complexity, production notes,
examples/basic/main.go          runnable example
Makefile                        test, race, benchmark, fmt, vet, check
```

## Make Targets

| Target           | Command                                      |
| ---------------- | -------------------------------------------- |
| `make test`      | `go test ./...`                              |
| `make race`      | `go test -race ./...`                        |
| `make benchmark` | `go test -run='^$' -bench=. -benchmem ./...` |
| `make fmt`       | `gofmt -w .`                                 |
| `make vet`       | `go vet ./...`                               |
| `make check`     | format check, vet, tests, race tests         |

Requires Go 1.22 or later (the code uses the `clear` builtin and
range-over-int in tests).

## License

MIT. See [LICENSE](LICENSE).
