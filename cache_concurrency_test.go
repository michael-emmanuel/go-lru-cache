package lru

import (
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// These tests are meaningful only when run with the race detector:
//
//	go test -race ./...
//
// Without -race they still check functional invariants under contention,
// but a missing lock would usually go unnoticed because torn writes are
// rare. None of the tests use sleeps or timing for synchronization: a
// barrier channel releases all goroutines at once so they contend, and
// WaitGroup provides the only ordering needed to inspect final state.

func workers() int {
	// At least 8 goroutines even on small CI machines, so scheduling
	// interleaves operations rather than running each goroutine to
	// completion.
	return max(8, 2*runtime.GOMAXPROCS(0))
}

// runConcurrently starts n goroutines, holds them at a barrier, releases
// them together, and waits for all to finish.
func runConcurrently(n int, fn func(id int)) {
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	ready.Add(n)
	done.Add(n)
	for id := range n {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			fn(id)
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()
}

// valueFor is a function of the key alone. Any Get that returns a value
// not equal to valueFor(key) proves that a node's key/value pairing was
// corrupted or that a value was delivered for the wrong key.
func valueFor(k int) int { return k*31 + 7 }

// Mixed Get/Set/Delete over a small key space with a small capacity
// maximizes both lock contention and eviction churn.
func TestConcurrentMixedOperations(t *testing.T) {
	const (
		capacity   = 16
		keySpace   = 64
		opsPerG    = 20000
		deleteRate = 10 // percent
	)
	c := mustNew[int, int](t, capacity)
	var corrupted atomic.Int64

	runConcurrently(workers(), func(id int) {
		rng := rand.New(rand.NewSource(int64(id)))
		for range opsPerG {
			k := rng.Intn(keySpace)
			switch r := rng.Intn(100); {
			case r < 45:
				if v, ok := c.Get(k); ok && v != valueFor(k) {
					corrupted.Add(1)
				}
			case r < 100-deleteRate:
				c.Set(k, valueFor(k))
			default:
				c.Delete(k)
			}
		}
	})

	if n := corrupted.Load(); n != 0 {
		t.Errorf("%d Gets returned a value that does not belong to the key", n)
	}
	if c.Len() > capacity {
		t.Errorf("Len() = %d exceeds capacity %d", c.Len(), capacity)
	}
	assertInvariants(t, c)
}

// Clear racing with every other operation. The interesting failure here is
// a node being unlinked from a list that Clear has already reset.
func TestConcurrentClear(t *testing.T) {
	const (
		capacity = 8
		keySpace = 32
		opsPerG  = 10000
	)
	c := mustNew[int, int](t, capacity)
	var corrupted atomic.Int64

	n := workers()
	runConcurrently(n, func(id int) {
		rng := rand.New(rand.NewSource(int64(id)))
		for i := range opsPerG {
			k := rng.Intn(keySpace)
			switch {
			case id%4 == 0 && i%200 == 0:
				c.Clear()
			case rng.Intn(2) == 0:
				if v, ok := c.Get(k); ok && v != valueFor(k) {
					corrupted.Add(1)
				}
			case rng.Intn(4) == 0:
				c.Delete(k)
			default:
				c.Set(k, valueFor(k))
			}
		}
	})

	if corrupted.Load() != 0 {
		t.Errorf("%d corrupted reads", corrupted.Load())
	}
	assertInvariants(t, c)

	c.Clear()
	if c.Len() != 0 {
		t.Errorf("Len() = %d after final Clear", c.Len())
	}
	assertInvariants(t, c)
}

// With capacity at least as large as the total key count, nothing is ever
// evicted. Each goroutine owns a disjoint key range, so its own writes are
// the only writes to its keys: every Get after a Set must hit and must
// return exactly what was written. This gives a deterministic functional
// check that does not depend on interleaving.
func TestConcurrentDisjointKeysAreNeverLost(t *testing.T) {
	const keysPerG = 500
	n := workers()
	c := mustNew[int, int](t, n*keysPerG)
	var failures atomic.Int64

	runConcurrently(n, func(id int) {
		base := id * keysPerG
		for round := range 4 {
			for i := range keysPerG {
				k := base + i
				c.Set(k, valueFor(k)+round)
				if v, ok := c.Get(k); !ok || v != valueFor(k)+round {
					failures.Add(1)
				}
			}
		}
	})

	if failures.Load() != 0 {
		t.Errorf("%d reads missed or returned a stale value", failures.Load())
	}
	if c.Len() != n*keysPerG {
		t.Errorf("Len() = %d, want %d", c.Len(), n*keysPerG)
	}
	assertInvariants(t, c)
}

// Len must never be observed above capacity, even while writers are
// evicting. This checks that eviction and insertion are one atomic step.
func TestConcurrentLenBoundedByCapacity(t *testing.T) {
	const capacity = 10
	c := mustNew[int, int](t, capacity)
	var violations atomic.Int64

	n := workers()
	runConcurrently(n, func(id int) {
		for i := range 20000 {
			if id%2 == 0 {
				c.Set(id*1_000_000+i, i)
			} else if c.Len() > capacity {
				violations.Add(1)
			}
		}
	})

	if violations.Load() != 0 {
		t.Errorf("Len() exceeded capacity %d on %d observations", capacity, violations.Load())
	}
	assertInvariants(t, c)
}

// Stress test: many goroutines, every operation type, tiny capacity, and
// periodic invariant checks taken while writers are still running. The
// checks run under the cache lock, so they observe only quiescent states
// between operations; a torn pointer update would be visible.
func TestStress(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in -short mode")
	}
	const (
		capacity = 4
		keySpace = 12
		opsPerG  = 50000
	)
	c := mustNew[int, int](t, capacity)
	var corrupted, invariantFailures atomic.Int64

	runConcurrently(2*workers(), func(id int) {
		rng := rand.New(rand.NewSource(int64(id) * 7919))
		for i := range opsPerG {
			k := rng.Intn(keySpace)
			switch r := rng.Intn(100); {
			case r < 40:
				if v, ok := c.Get(k); ok && v != valueFor(k) {
					corrupted.Add(1)
				}
			case r < 75:
				c.Set(k, valueFor(k))
			case r < 90:
				c.Delete(k)
			case r < 92:
				c.Clear()
			default:
				_ = c.Len()
			}

			if i%1000 == 0 {
				c.mu.Lock()
				if err := c.checkInvariants(); err != nil {
					invariantFailures.Add(1)
				}
				c.mu.Unlock()
			}
		}
	})

	if corrupted.Load() != 0 {
		t.Errorf("%d corrupted reads", corrupted.Load())
	}
	if invariantFailures.Load() != 0 {
		t.Errorf("%d mid-run invariant violations", invariantFailures.Load())
	}
	assertInvariants(t, c)
}

// Capacity is read without the lock. Reading it concurrently with writers
// must be race-free because it is immutable after New.
func TestConcurrentCapacityReads(t *testing.T) {
	c := mustNew[int, int](t, 5)
	runConcurrently(workers(), func(id int) {
		for i := range 5000 {
			if c.Capacity() != 5 {
				t.Error("Capacity changed")
				return
			}
			c.Set(i%20, i)
		}
	})
	assertInvariants(t, c)
}
