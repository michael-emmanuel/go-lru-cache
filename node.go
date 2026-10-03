package lru

// node is one element of the doubly linked list that records recency order.
//
// A node stores its own key in addition to the value. The key is required
// for eviction: when the least recently used node is unlinked from the list,
// the cache only holds a pointer to the node and must use node.key to delete
// the matching entry from the map. Without the key, eviction would require
// scanning the map, which is O(n).
//
// Nodes are created and mutated only while the owning Cache's mutex is held.
// A node is never shared outside the package.
type node[K comparable, V any] struct {
	key   K
	value V

	// prev points toward the most recently used end of the list (toward
	// head). next points toward the least recently used end (toward tail).
	//
	// Both are nil only for a node that is not currently linked into a
	// list. The sentinel head has prev == nil and the sentinel tail has
	// next == nil; every other linked node has both set.
	prev *node[K, V]
	next *node[K, V]
}
