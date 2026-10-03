# Data Structures

This document builds up the structures used by the cache from first
principles and shows why this particular combination yields O(1) LRU
operations.

## Hash map

A hash map stores key/value pairs and finds a value by key without scanning.

**Mechanism.** A hash function turns a key into an integer. That integer,
reduced modulo the number of buckets, selects a bucket. The lookup then
examines only the entries in that bucket.

**Collisions.** Two different keys can hash to the same bucket. Maps handle
this by keeping several entries per bucket (chaining or bucket arrays) or by
probing other slots. As long as the hash function spreads keys well and the
table is resized as it fills, buckets stay small, so a lookup does a roughly
constant amount of work.

**Complexity.** Lookup, insert and delete are O(1) *on average*. The worst
case is worse (if many keys collide, a lookup degenerates toward scanning a
bucket), and an insert that triggers a resize costs O(n) for that one
operation, amortized across many inserts. Go's runtime seeds each map's hash
function randomly, and the details of how maps grow differ between Go
versions, but the honest claim is "O(1) average", not "O(1) worst case".

**Why `K` must be `comparable`.** The map must decide whether two keys are
the same key. Go defines this with `==`. A type is `comparable` if `==` is
defined for it: numbers, strings, booleans, pointers, channels, arrays and
structs of comparable types. Slices, maps and functions are not comparable,
so they cannot be map keys. Generics express the same rule through the
`comparable` constraint: `map[K]V` only compiles when `K` is `comparable`.
If `K` is an interface type, `comparable` is satisfied at compile time, but
a value whose dynamic type is not hashable (for example a `[]int` stored in
an `any`) panics at run time on the first map operation.

**What a map cannot do.** It does not remember order of use. Go's map
iteration order is intentionally randomized. Nothing in a map tells you
which key was touched least recently.

## Singly linked list

Each node holds a value and a `next` pointer. Inserting at the head is O(1).
Removing the *head* is O(1).

Removing an arbitrary node `x` requires changing the `next` pointer of the
node *before* `x`. A singly linked list has no pointer to that node. Finding
it means walking from the head, which is O(n). Removing the *tail* has the
same problem: to make the second-to-last node the new tail, you must reach
it. Since an LRU cache must both move arbitrary nodes (on every `Get`) and
remove from the tail (on every eviction), a singly linked list cannot give
O(1).

Searching for a key is also O(n), which is why a list alone is not a cache.

## Doubly linked list

Each node has `prev` and `next`:

```
        prev            prev            prev
   +---------+     +---------+     +---------+
   |    A    | <-> |    B    | <-> |    C    |
   +---------+     +---------+     +---------+
        next            next            next
```

Because a node knows both neighbors, it can unlink itself without
traversal:

```
removeNode(n):
    n.prev.next = n.next     // predecessor skips n
    n.next.prev = n.prev     // successor skips n
```

Inserting a node between `p` and `q` likewise needs only `p` and `q`.

| Operation, given a pointer to the node | Time |
|----------------------------------------|------|
| Remove node | O(1) |
| Insert after a known node | O(1) |
| Move to front (remove + insert) | O(1) |
| Remove the last node (via `tail.prev`) | O(1) |
| Find a node by key | O(n) |

The last row is the weakness the hash map fixes.

**Why both directions matter.** `next` lets you walk toward the LRU end.
`prev` lets you reach a node's predecessor, which is what makes removal from
the middle and removal of the tail O(1). With both links and a pointer to the
tail, the LRU entry is always one step away.

## Sentinel nodes

A sentinel is a dummy node that is always present, so real nodes always have
real neighbors.

Without sentinels, linking a node requires cases:

```
if list is empty:           head = tail = n
else if inserting at front: n.next = head; head.prev = n; head = n
...
if removing the only node:  head = tail = nil
else if removing head:      head = head.next; head.prev = nil
else if removing tail:      tail = tail.prev; tail.next = nil
else:                       n.prev.next = n.next; n.next.prev = n.prev
```

With sentinels `head` and `tail`:

```
empty:      HEAD <-> TAIL
one entry:  HEAD <-> A <-> TAIL
```

Every real node has non-nil `prev` and `next`, so `removeNode` is the same
two assignments whether the node is first, last, only, or in the middle.
`addToFront` is four assignments with no branches. The cost is two extra
nodes per cache, which hold no data and are never in the map.

The empty-cache check becomes `head.next == tail`, and the LRU node is simply
`tail.prev` (or "none" if that is `head`).

## Combined design

The cache uses `map[K]*node[K,V]` plus the sentinel-bracketed list, and the
two cover each other's weaknesses.

| Need | Provided by | Cost |
|------|-------------|------|
| Find entry by key | map | O(1) average |
| Know which entry is least recent | list order (`tail.prev`) | O(1) |
| Mark entry as most recent | list move-to-front | O(1) |
| Evict least recent | list unlink + `delete(map, node.key)` | O(1) average |

**Worked example.** Capacity 3. Notation: list from MRU to LRU.

```
Set(A)   list: A                map: {A}
Set(B)   list: B A              map: {A, B}
Set(C)   list: C B A            map: {A, B, C}

Get(A)   map[A] -> node A (O(1)); unlink A, relink at front
         list: A C B

Set(D)   map has 3 entries = capacity, so evict tail.prev = B
         unlink B; delete(map, "B")        (B's node carries its key)
         list: A C
         insert D at front
         list: D A C                map: {A, C, D}
```

At no point is the list traversed to find a key, and at no point is the map
scanned to find the oldest entry.

**Why the node stores the key.** During eviction we hold only a pointer to
the LRU node. To remove its entry from the map we need its key. Storing the
key in the node makes that O(1) rather than a map scan.
