package lru

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"testing"
)

// ---------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------

func mustNew[K comparable, V any](t testing.TB, capacity int) *Cache[K, V] {
	t.Helper()
	c, err := New[K, V](capacity)
	if err != nil {
		t.Fatalf("New(%d) returned error: %v", capacity, err)
	}
	return c
}

// checkInvariants verifies the structural invariants documented on Cache.
// The caller must hold c.mu (or otherwise have exclusive access). It is
// defined in a test file so that production code carries no test-only
// surface, and it is shared by the concurrency tests in this package.
//
// Every list walk is bounded so that a corrupted (cyclic) list produces an
// error instead of hanging the test.
func (c *Cache[K, V]) checkInvariants() error {
	if c.head == nil || c.tail == nil {
		return errors.New("sentinel is nil")
	}
	if c.head.prev != nil {
		return errors.New("head.prev must be nil")
	}
	if c.tail.next != nil {
		return errors.New("tail.next must be nil")
	}
	if len(c.items) > c.capacity {
		return fmt.Errorf("len %d exceeds capacity %d", len(c.items), c.capacity)
	}

	// Forward walk: head -> ... -> tail.
	seen := make(map[*node[K, V]]struct{}, len(c.items))
	count := 0
	prev := c.head
	for n := c.head.next; n != c.tail; n = n.next {
		if n == nil {
			return errors.New("forward walk hit nil before reaching tail")
		}
		if n == c.head {
			return errors.New("head appears inside the list")
		}
		if n.prev != prev {
			return fmt.Errorf("node %v: prev pointer does not match predecessor", n.key)
		}
		if _, dup := seen[n]; dup {
			return fmt.Errorf("node %v appears twice in the list", n.key)
		}
		seen[n] = struct{}{}
		if mapped, ok := c.items[n.key]; !ok || mapped != n {
			return fmt.Errorf("node %v is in the list but not mapped to itself", n.key)
		}
		count++
		if count > len(c.items) {
			return errors.New("list longer than map (cycle or orphan node)")
		}
		prev = n
	}
	if c.tail.prev != prev {
		return errors.New("tail.prev does not match last real node")
	}
	if count != len(c.items) {
		return fmt.Errorf("list has %d nodes but map has %d entries", count, len(c.items))
	}

	// Empty <=> head and tail adjacent.
	adjacent := c.head.next == c.tail && c.tail.prev == c.head
	if (len(c.items) == 0) != adjacent {
		return fmt.Errorf("emptiness mismatch: len=%d adjacent=%v", len(c.items), adjacent)
	}
	return nil
}

func assertInvariants[K comparable, V any](t testing.TB, c *Cache[K, V]) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkInvariants(); err != nil {
		t.Fatalf("invariant violated: %v", err)
	}
}

// order returns the keys from most recently used to least recently used.
func order[K comparable, V any](c *Cache[K, V]) []K {
	c.mu.Lock()
	defer c.mu.Unlock()
	var keys []K
	for n := c.head.next; n != c.tail; n = n.next {
		keys = append(keys, n.key)
	}
	return keys
}

func assertOrder[K comparable, V any](t testing.TB, c *Cache[K, V], wantMRUFirst ...K) {
	t.Helper()
	got := order(c)
	if !reflect.DeepEqual(got, wantMRUFirst) && !(len(got) == 0 && len(wantMRUFirst) == 0) {
		t.Fatalf("recency order (MRU first) = %v, want %v", got, wantMRUFirst)
	}
	assertInvariants(t, c)
}

// ---------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------

func TestNew(t *testing.T) {
	c, err := New[string, int](5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Capacity() != 5 {
		t.Errorf("Capacity() = %d, want 5", c.Capacity())
	}
	if c.Len() != 0 {
		t.Errorf("Len() = %d, want 0", c.Len())
	}
	assertOrder(t, c)
}

func TestNewInvalidCapacity(t *testing.T) {
	for _, capacity := range []int{0, -1, -100, math.MinInt} {
		c, err := New[string, int](capacity)
		if c != nil {
			t.Errorf("New(%d) returned non-nil cache", capacity)
		}
		if !errors.Is(err, ErrInvalidCapacity) {
			t.Errorf("New(%d) error = %v, want ErrInvalidCapacity", capacity, err)
		}
	}
}

// ---------------------------------------------------------------------
// Get, Set, Delete basics
// ---------------------------------------------------------------------

func TestGetMissing(t *testing.T) {
	c := mustNew[string, int](t, 2)
	v, ok := c.Get("absent")
	if ok || v != 0 {
		t.Errorf("Get(absent) = (%d, %v), want (0, false)", v, ok)
	}
	assertOrder(t, c)
}

func TestSetThenGet(t *testing.T) {
	c := mustNew[string, int](t, 2)
	c.Set("a", 1)
	v, ok := c.Get("a")
	if !ok || v != 1 {
		t.Errorf("Get(a) = (%d, %v), want (1, true)", v, ok)
	}
	if c.Len() != 1 {
		t.Errorf("Len() = %d, want 1", c.Len())
	}
}

func TestSetExistingKeyUpdatesValueWithoutGrowing(t *testing.T) {
	c := mustNew[string, int](t, 3)
	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("a", 10)

	if c.Len() != 2 {
		t.Errorf("Len() = %d, want 2", c.Len())
	}
	if v, _ := c.Get("a"); v != 10 {
		t.Errorf("Get(a) = %d, want 10", v)
	}
	assertInvariants(t, c)
}

func TestRepeatedSetSameKey(t *testing.T) {
	c := mustNew[string, int](t, 2)
	for i := range 100 {
		c.Set("k", i)
	}
	if c.Len() != 1 {
		t.Errorf("Len() = %d, want 1", c.Len())
	}
	if v, ok := c.Get("k"); !ok || v != 99 {
		t.Errorf("Get(k) = (%d, %v), want (99, true)", v, ok)
	}
	assertOrder(t, c, "k")
}

func TestRepeatedGetSameKey(t *testing.T) {
	c := mustNew[string, int](t, 3)
	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("c", 3)
	for range 50 {
		if v, ok := c.Get("a"); !ok || v != 1 {
			t.Fatalf("Get(a) = (%d, %v), want (1, true)", v, ok)
		}
	}
	assertOrder(t, c, "a", "c", "b")
}

func TestDeleteMissing(t *testing.T) {
	c := mustNew[string, int](t, 2)
	if c.Delete("nope") {
		t.Error("Delete of missing key returned true")
	}
	c.Set("a", 1)
	if c.Delete("nope") {
		t.Error("Delete of missing key returned true on non-empty cache")
	}
	if c.Len() != 1 {
		t.Errorf("Len() = %d, want 1", c.Len())
	}
	assertOrder(t, c, "a")
}

func TestDeleteExisting(t *testing.T) {
	c := mustNew[string, int](t, 3)
	c.Set("a", 1)
	c.Set("b", 2)

	if !c.Delete("a") {
		t.Fatal("Delete(a) = false, want true")
	}
	if _, ok := c.Get("a"); ok {
		t.Error("key a still present after Delete")
	}
	if c.Len() != 1 {
		t.Errorf("Len() = %d, want 1", c.Len())
	}
	if c.Delete("a") {
		t.Error("second Delete(a) = true, want false")
	}
	assertOrder(t, c, "b")
}

// Deleting from each position exercises the pointer rewiring at the head
// end, in the middle, and at the tail end of the list.
func TestDeleteFromEachPosition(t *testing.T) {
	tests := []struct {
		name   string
		remove string
		want   []string // MRU first
	}{
		{"MRU", "c", []string{"b", "a"}},
		{"middle", "b", []string{"c", "a"}},
		{"LRU", "a", []string{"c", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := mustNew[string, int](t, 3)
			c.Set("a", 1)
			c.Set("b", 2)
			c.Set("c", 3)
			if !c.Delete(tt.remove) {
				t.Fatalf("Delete(%s) = false", tt.remove)
			}
			assertOrder(t, c, tt.want...)
		})
	}
}

func TestDeleteLastRemainingEntryThenReuse(t *testing.T) {
	c := mustNew[int, int](t, 2)
	c.Set(1, 1)
	c.Delete(1)
	assertOrder(t, c)
	c.Set(2, 2)
	assertOrder(t, c, 2)
}

// ---------------------------------------------------------------------
// Recency and eviction
// ---------------------------------------------------------------------

func TestEvictionFollowsRecencyOrder(t *testing.T) {
	c := mustNew[string, int](t, 3)

	c.Set("A", 1)
	c.Set("B", 2)
	c.Set("C", 3)
	assertOrder(t, c, "C", "B", "A") // A = LRU, C = MRU

	if _, ok := c.Get("A"); !ok {
		t.Fatal("Get(A) missed")
	}
	assertOrder(t, c, "A", "C", "B") // B = LRU, A = MRU

	c.Set("D", 4) // cache is full: must evict B
	assertOrder(t, c, "D", "A", "C")

	if _, ok := c.Get("B"); ok {
		t.Error("B should have been evicted")
	}
	for _, k := range []string{"A", "C", "D"} {
		if _, ok := c.Get(k); !ok {
			t.Errorf("%s should still be cached", k)
		}
	}
	if c.Len() != 3 {
		t.Errorf("Len() = %d, want 3", c.Len())
	}
}

func TestSetExistingKeyBecomesMRU(t *testing.T) {
	c := mustNew[string, int](t, 3)
	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("c", 3)

	c.Set("a", 100) // a was LRU; updating it must protect it from eviction
	assertOrder(t, c, "a", "c", "b")

	c.Set("d", 4) // evicts b, not a
	if _, ok := c.Get("b"); ok {
		t.Error("b should have been evicted")
	}
	if v, ok := c.Get("a"); !ok || v != 100 {
		t.Errorf("Get(a) = (%d, %v), want (100, true)", v, ok)
	}
}

func TestGetOfMissingKeyDoesNotChangeOrder(t *testing.T) {
	c := mustNew[string, int](t, 3)
	c.Set("a", 1)
	c.Set("b", 2)
	c.Get("zzz")
	assertOrder(t, c, "b", "a")
}

func TestGetOfMRUKeepsOrder(t *testing.T) {
	c := mustNew[string, int](t, 3)
	c.Set("a", 1)
	c.Set("b", 2)
	c.Get("b")
	assertOrder(t, c, "b", "a")
}

func TestCapacityOne(t *testing.T) {
	c := mustNew[string, int](t, 1)
	c.Set("a", 1)
	assertOrder(t, c, "a")

	c.Set("b", 2) // evicts a
	if _, ok := c.Get("a"); ok {
		t.Error("a should have been evicted")
	}
	if v, ok := c.Get("b"); !ok || v != 2 {
		t.Errorf("Get(b) = (%d, %v), want (2, true)", v, ok)
	}

	c.Set("b", 3) // update in place
	if c.Len() != 1 {
		t.Errorf("Len() = %d, want 1", c.Len())
	}
	if !c.Delete("b") || c.Len() != 0 {
		t.Error("Delete(b) failed to empty the cache")
	}
	assertOrder(t, c)
}

func TestCapacityTwo(t *testing.T) {
	c := mustNew[string, int](t, 2)
	c.Set("a", 1)
	c.Set("b", 2)
	assertOrder(t, c, "b", "a")

	c.Get("a")
	c.Set("c", 3) // evicts b
	assertOrder(t, c, "c", "a")

	c.Set("d", 4) // evicts a
	assertOrder(t, c, "d", "c")
}

func TestLenNeverExceedsCapacity(t *testing.T) {
	const capacity = 7
	c := mustNew[int, int](t, capacity)
	for i := range 1000 {
		c.Set(i, i)
		if c.Len() > capacity {
			t.Fatalf("Len() = %d after %d inserts, exceeds capacity %d", c.Len(), i+1, capacity)
		}
	}
	if c.Len() != capacity {
		t.Errorf("Len() = %d, want %d", c.Len(), capacity)
	}
	// The survivors must be the most recently inserted keys.
	assertOrder(t, c, 999, 998, 997, 996, 995, 994, 993)
}

// ---------------------------------------------------------------------
// Clear
// ---------------------------------------------------------------------

func TestClear(t *testing.T) {
	c := mustNew[string, int](t, 3)
	c.Set("a", 1)
	c.Set("b", 2)
	c.Clear()

	if c.Len() != 0 {
		t.Errorf("Len() after Clear = %d, want 0", c.Len())
	}
	if c.Capacity() != 3 {
		t.Errorf("Capacity() after Clear = %d, want 3", c.Capacity())
	}
	if _, ok := c.Get("a"); ok {
		t.Error("a still present after Clear")
	}
	assertOrder(t, c)
}

func TestClearEmptyCache(t *testing.T) {
	c := mustNew[string, int](t, 3)
	c.Clear()
	assertOrder(t, c)
}

func TestCacheIsReusableAfterClear(t *testing.T) {
	c := mustNew[int, int](t, 2)
	c.Set(1, 1)
	c.Set(2, 2)
	c.Clear()

	c.Set(3, 3)
	c.Set(4, 4)
	c.Set(5, 5) // capacity is still enforced: evicts 3
	assertOrder(t, c, 5, 4)
}

// ---------------------------------------------------------------------
// Zero values and generic instantiations
// ---------------------------------------------------------------------

func TestZeroValuesAreDistinguishableFromMisses(t *testing.T) {
	t.Run("int", func(t *testing.T) {
		c := mustNew[string, int](t, 2)
		c.Set("zero", 0)
		v, ok := c.Get("zero")
		if !ok || v != 0 {
			t.Errorf("Get = (%d, %v), want (0, true)", v, ok)
		}
		if _, ok := c.Get("missing"); ok {
			t.Error("missing key reported as present")
		}
	})
	t.Run("string", func(t *testing.T) {
		c := mustNew[int, string](t, 2)
		c.Set(1, "")
		if v, ok := c.Get(1); !ok || v != "" {
			t.Errorf("Get = (%q, %v), want (\"\", true)", v, ok)
		}
	})
	t.Run("nil pointer", func(t *testing.T) {
		c := mustNew[string, *int](t, 2)
		c.Set("nil", nil)
		if v, ok := c.Get("nil"); !ok || v != nil {
			t.Errorf("Get = (%v, %v), want (nil, true)", v, ok)
		}
	})
	t.Run("zero key", func(t *testing.T) {
		c := mustNew[int, string](t, 2)
		c.Set(0, "zero-key")
		if v, ok := c.Get(0); !ok || v != "zero-key" {
			t.Errorf("Get(0) = (%q, %v)", v, ok)
		}
	})
}

type user struct {
	ID   int
	Name string
}

type compositeKey struct {
	Tenant string
	ID     int
}

func TestGenericInstantiations(t *testing.T) {
	t.Run("string to struct", func(t *testing.T) {
		c := mustNew[string, user](t, 2)
		c.Set("u1", user{1, "Alice"})
		if v, ok := c.Get("u1"); !ok || v != (user{1, "Alice"}) {
			t.Errorf("Get(u1) = (%+v, %v)", v, ok)
		}
	})
	t.Run("int to string", func(t *testing.T) {
		c := mustNew[int, string](t, 2)
		c.Set(7, "seven")
		if v, ok := c.Get(7); !ok || v != "seven" {
			t.Errorf("Get(7) = (%q, %v)", v, ok)
		}
	})
	t.Run("struct key", func(t *testing.T) {
		c := mustNew[compositeKey, int](t, 2)
		c.Set(compositeKey{"a", 1}, 10)
		c.Set(compositeKey{"b", 1}, 20)
		if v, ok := c.Get(compositeKey{"a", 1}); !ok || v != 10 {
			t.Errorf("Get(a/1) = (%d, %v), want (10, true)", v, ok)
		}
		if _, ok := c.Get(compositeKey{"a", 2}); ok {
			t.Error("distinct struct key matched")
		}
	})
	t.Run("array key", func(t *testing.T) {
		c := mustNew[[2]int, string](t, 2)
		c.Set([2]int{1, 2}, "x")
		if v, ok := c.Get([2]int{1, 2}); !ok || v != "x" {
			t.Errorf("Get = (%q, %v)", v, ok)
		}
	})
	t.Run("pointer values are stored by reference", func(t *testing.T) {
		c := mustNew[string, *user](t, 2)
		u := &user{1, "Alice"}
		c.Set("u1", u)
		got, ok := c.Get("u1")
		if !ok || got != u {
			t.Fatalf("Get returned a different pointer: %p vs %p", got, u)
		}
		got.Name = "Alicia"
		if u.Name != "Alicia" {
			t.Error("mutation through returned pointer not visible to original")
		}
	})
	t.Run("slice and map values", func(t *testing.T) {
		c := mustNew[int, []string](t, 2)
		c.Set(1, []string{"a", "b"})
		if v, ok := c.Get(1); !ok || len(v) != 2 {
			t.Errorf("Get(1) = (%v, %v)", v, ok)
		}
	})
}

// A hash-map operation on an unhashable dynamic key type panics. The panic
// happens before any cache state is modified, and the deferred unlock
// releases the mutex, so the cache stays usable. If Unlock were not
// deferred this test would deadlock on the following Set.
func TestUnhashableInterfaceKeyPanicsButLeavesCacheUsable(t *testing.T) {
	c := mustNew[any, int](t, 2)
	c.Set("ok", 1)

	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected panic for unhashable key")
			}
		}()
		c.Set([]int{1, 2, 3}, 2)
	}()

	c.Set("after", 3)
	assertOrder(t, c, any("after"), any("ok"))
}

// ---------------------------------------------------------------------
// Model-based test
// ---------------------------------------------------------------------

// refLRU is a deliberately naive LRU (a slice scanned linearly) used as a
// specification. It is obviously correct and O(n); the real cache must
// agree with it on every observable result.
type refLRU struct {
	capacity int
	keys     []int // MRU first
	vals     map[int]int
}

func newRef(capacity int) *refLRU {
	return &refLRU{capacity: capacity, vals: map[int]int{}}
}

func (r *refLRU) indexOf(k int) int {
	for i, x := range r.keys {
		if x == k {
			return i
		}
	}
	return -1
}

func (r *refLRU) touch(k int) {
	if i := r.indexOf(k); i >= 0 {
		r.keys = append(r.keys[:i], r.keys[i+1:]...)
	}
	r.keys = append([]int{k}, r.keys...)
}

func (r *refLRU) get(k int) (int, bool) {
	v, ok := r.vals[k]
	if ok {
		r.touch(k)
	}
	return v, ok
}

func (r *refLRU) set(k, v int) {
	if _, ok := r.vals[k]; !ok && len(r.keys) >= r.capacity {
		lru := r.keys[len(r.keys)-1]
		r.keys = r.keys[:len(r.keys)-1]
		delete(r.vals, lru)
	}
	r.vals[k] = v
	r.touch(k)
}

func (r *refLRU) delete(k int) bool {
	i := r.indexOf(k)
	if i < 0 {
		return false
	}
	r.keys = append(r.keys[:i], r.keys[i+1:]...)
	delete(r.vals, k)
	return true
}

func TestMatchesReferenceModel(t *testing.T) {
	for _, capacity := range []int{1, 2, 3, 8, 32} {
		t.Run(fmt.Sprintf("capacity=%d", capacity), func(t *testing.T) {
			rng := rand.New(rand.NewSource(int64(capacity)))
			c := mustNew[int, int](t, capacity)
			ref := newRef(capacity)
			keySpace := capacity * 2 // forces frequent eviction and misses

			for step := range 5000 {
				k := rng.Intn(keySpace)
				switch op := rng.Intn(10); {
				case op < 4:
					gv, gok := c.Get(k)
					wv, wok := ref.get(k)
					if gv != wv || gok != wok {
						t.Fatalf("step %d Get(%d) = (%d, %v), want (%d, %v)", step, k, gv, gok, wv, wok)
					}
				case op < 8:
					v := rng.Int()
					c.Set(k, v)
					ref.set(k, v)
				case op < 9:
					if g, w := c.Delete(k), ref.delete(k); g != w {
						t.Fatalf("step %d Delete(%d) = %v, want %v", step, k, g, w)
					}
				default:
					if step%500 == 0 { // clear rarely so the cache actually fills
						c.Clear()
						ref = newRef(capacity)
					}
				}

				if c.Len() != len(ref.keys) {
					t.Fatalf("step %d Len() = %d, want %d", step, c.Len(), len(ref.keys))
				}
				got := order(c)
				if len(got) != len(ref.keys) || (len(got) > 0 && !reflect.DeepEqual(got, ref.keys)) {
					t.Fatalf("step %d order = %v, want %v", step, got, ref.keys)
				}
				assertInvariants(t, c)
			}
		})
	}
}
