package lru

import (
	"fmt"
	"math/rand"
	"testing"
)

// What these benchmarks measure
//
// Every benchmark uses int keys and int values, so results isolate the
// cost of the data-structure mechanics (map lookup, pointer updates,
// allocation of nodes, mutex acquisition) rather than key hashing or value
// copying. Numbers depend on hardware, Go version, GOMAXPROCS and
// background load; compare runs on the same machine and treat absolute
// values as indicative only.
//
// Serial benchmarks (Get, Set, Delete, MixedWorkload) measure the
// single-goroutine cost of an operation including an uncontended
// lock/unlock. The Parallel benchmarks use b.RunParallel to add
// contention on the single mutex and show how throughput scales (or fails
// to) as goroutines are added.
//
// Key sequences are generated before the timer starts so the benchmarks
// measure the cache, not the random number generator.

const benchCapacity = 1 << 12 // 4096 entries

func benchKeys(n, keySpace int, seed int64) []int {
	rng := rand.New(rand.NewSource(seed))
	keys := make([]int, n)
	for i := range keys {
		keys[i] = rng.Intn(keySpace)
	}
	return keys
}

func filled(b *testing.B, capacity int) *Cache[int, int] {
	b.Helper()
	c := mustNew[int, int](b, capacity)
	for i := range capacity {
		c.Set(i, i)
	}
	return c
}

// BenchmarkGet measures a Get that hits: map lookup plus a move to front.
// Keys are drawn uniformly from the resident set, so most hits relocate a
// node that is not already MRU.
func BenchmarkGet(b *testing.B) {
	c := filled(b, benchCapacity)
	keys := benchKeys(1<<16, benchCapacity, 1)
	mask := len(keys) - 1

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Get(keys[i&mask])
	}
}

// BenchmarkGetMiss measures a Get for an absent key: map lookup only.
func BenchmarkGetMiss(b *testing.B) {
	c := filled(b, benchCapacity)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Get(benchCapacity + i)
	}
}

// BenchmarkSet compares the three distinct cost profiles of Set.
func BenchmarkSet(b *testing.B) {
	b.Run("update-existing", func(b *testing.B) {
		// No allocation expected: value is overwritten and node moved.
		c := filled(b, benchCapacity)
		keys := benchKeys(1<<16, benchCapacity, 2)
		mask := len(keys) - 1

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			c.Set(keys[i&mask], i)
		}
	})
	b.Run("insert-with-eviction", func(b *testing.B) {
		// Every key is new and the cache is full, so each Set evicts one
		// node and allocates one node.
		c := filled(b, benchCapacity)

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			c.Set(benchCapacity+i, i)
		}
	})
	b.Run("insert-no-eviction", func(b *testing.B) {
		// Capacity is large enough that nothing is evicted; this includes
		// map growth. The cache is rebuilt in batches so memory stays
		// bounded for large b.N.
		const batch = 1 << 16
		b.ReportAllocs()
		b.ResetTimer()
		var c *Cache[int, int]
		for i := 0; i < b.N; i++ {
			if i%batch == 0 {
				b.StopTimer()
				c = mustNew[int, int](b, batch)
				b.StartTimer()
			}
			c.Set(i%batch, i)
		}
	})
}

// BenchmarkDelete measures removal of a present key. The cache is refilled
// with the timer stopped, in batches, so each timed Delete hits.
func BenchmarkDelete(b *testing.B) {
	const batch = benchCapacity
	c := mustNew[int, int](b, batch)
	perm := rand.New(rand.NewSource(3)).Perm(batch)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		j := i % batch
		if j == 0 {
			b.StopTimer()
			c.Clear()
			for k := range batch {
				c.Set(k, k)
			}
			b.StartTimer()
		}
		c.Delete(perm[j])
	}
}

// workload describes a read/write mix and how large the key space is
// relative to capacity. A key space larger than capacity produces misses
// and therefore evictions.
type workload struct {
	name         string
	readPercent  int
	keySpaceMult float64 // key space = capacity * keySpaceMult
}

var workloads = []workload{
	// Working set fits in cache: nearly all reads hit, few evictions.
	{"read-mostly-fits", 90, 0.5},
	// Reads and writes evenly mixed, working set fits.
	{"balanced-fits", 50, 0.5},
	// Key space is 4x capacity with mostly reads: frequent misses and, if
	// the caller populates on miss, frequent evictions.
	{"read-mostly-eviction", 90, 4},
	// Write-heavy with 4x key space: every few operations evict.
	{"write-heavy-eviction", 20, 4},
}

func runMixed(c *Cache[int, int], keys []int, readPercent int, i int) {
	k := keys[i%len(keys)]
	// Derive the read/write choice from the key sequence index with a
	// cheap deterministic hash, avoiding a shared RNG (which would add its
	// own contention in the parallel benchmark).
	if int((uint32(i)*2654435761)>>8%100) < readPercent {
		if _, ok := c.Get(k); !ok {
			c.Set(k, i) // populate on miss, as a typical cache user would
		}
	} else {
		c.Set(k, i)
	}
}

// BenchmarkMixedWorkload runs each workload on a single goroutine.
func BenchmarkMixedWorkload(b *testing.B) {
	for _, w := range workloads {
		b.Run(w.name, func(b *testing.B) {
			c := filled(b, benchCapacity)
			keys := benchKeys(1<<16, int(float64(benchCapacity)*w.keySpaceMult), 4)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				runMixed(c, keys, w.readPercent, i)
			}
		})
	}
}

// BenchmarkMixedWorkloadParallel runs each workload from many goroutines
// against one cache. Run with -cpu=1,2,4,8 to see how a single mutex
// scales: with a global lock, per-operation time typically stops improving
// (and can worsen) as parallelism increases, because critical sections are
// serialized.
func BenchmarkMixedWorkloadParallel(b *testing.B) {
	for _, w := range workloads {
		b.Run(w.name, func(b *testing.B) {
			c := filled(b, benchCapacity)
			keys := benchKeys(1<<16, int(float64(benchCapacity)*w.keySpaceMult), 5)

			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := rand.Intn(len(keys)) // de-synchronize starting points
				for pb.Next() {
					runMixed(c, keys, w.readPercent, i)
					i++
				}
			})
		})
	}
}

// BenchmarkCapacityScaling checks the O(1) claim empirically: time per Get
// should stay roughly flat as capacity grows by orders of magnitude. (It
// may rise modestly at large sizes because of CPU cache misses; that is a
// memory-hierarchy effect, not algorithmic growth.)
func BenchmarkCapacityScaling(b *testing.B) {
	for _, capacity := range []int{1 << 8, 1 << 12, 1 << 16, 1 << 20} {
		b.Run(fmt.Sprintf("capacity=%d", capacity), func(b *testing.B) {
			c := filled(b, capacity)
			keys := benchKeys(1<<16, capacity, 6)
			mask := len(keys) - 1

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				c.Get(keys[i&mask])
			}
		})
	}
}
