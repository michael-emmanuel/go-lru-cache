// Command basic demonstrates the lru package: basic use, recency updates,
// eviction, and distinguishing a miss from a stored zero value.
//
// Run it from the repository root with:
//
//	go run ./examples/basic
package main

import (
	"errors"
	"fmt"
	"log"

	lru "github.com/michael-emmanuel/go-lru-cache"
)

func main() {
	// Invalid configuration is reported as an error, not a panic.
	if _, err := lru.New[string, string](0); errors.Is(err, lru.ErrInvalidCapacity) {
		fmt.Println("capacity 0 rejected:", err)
	}

	cache, err := lru.New[string, string](3)
	if err != nil {
		log.Fatal(err)
	}

	cache.Set("user:1001", "Alice")
	cache.Set("user:1002", "Bob")
	cache.Set("user:1003", "Charlie")
	fmt.Printf("len=%d capacity=%d\n", cache.Len(), cache.Capacity())

	// Reading user:1001 makes it the most recently used entry, so user:1002
	// becomes the least recently used.
	if name, ok := cache.Get("user:1001"); ok {
		fmt.Println("user:1001 =", name)
	}

	// The cache is full. Inserting a fourth key evicts the LRU entry.
	cache.Set("user:1004", "Diana")

	for _, key := range []string{"user:1001", "user:1002", "user:1003", "user:1004"} {
		if name, ok := cache.Get(key); ok {
			fmt.Printf("%s: hit (%s)\n", key, name)
		} else {
			fmt.Printf("%s: miss (evicted)\n", key)
		}
	}

	// The boolean result distinguishes a missing key from a cached zero
	// value.
	counts, err := lru.New[string, int](2)
	if err != nil {
		log.Fatal(err)
	}
	counts.Set("errors", 0)
	if n, ok := counts.Get("errors"); ok {
		fmt.Printf("errors: cached value %d\n", n)
	}
	if _, ok := counts.Get("warnings"); !ok {
		fmt.Println("warnings: not cached")
	}

	if counts.Delete("errors") {
		fmt.Println("deleted errors; len =", counts.Len())
	}
}
