package mem

import (
	"bytes"
	"slices"
	"sort"
)

// A copy-on-write B+ tree. Trees are immutable once published: a writer
// copies every node it changes, except nodes it already copied during the
// same batch (marked with the batch's generation), which it changes in
// place. A snapshot is therefore just a root pointer.
//
// Leaves hold keys and values. Internal nodes hold children and separator
// keys: keys[i] is the smallest key in children[i+1], so a key k belongs
// in the child after the last separator <= k.

type node struct {
	gen      uint64
	keys     [][]byte
	vals     [][]byte // leaves only
	children []*node  // internal nodes only
}

func (n *node) leaf() bool { return n.children == nil }

// size is the number of entries (leaf) or children (internal node).
func (n *node) size() int {
	if n.leaf() {
		return len(n.keys)
	}
	return len(n.children)
}

// tree is one version of the data.
type tree struct {
	root *node
	len  int
}

// writer modifies a tree in one generation.
type writer struct {
	gen     uint64
	maxSize int // entries or children per node before it splits
	t       tree
}

func (w *writer) mut(n *node) *node {
	if n.gen == w.gen {
		return n
	}
	c := &node{gen: w.gen, keys: slices.Clone(n.keys)}
	if n.leaf() {
		c.vals = slices.Clone(n.vals)
	} else {
		c.children = slices.Clone(n.children)
	}
	return c
}

// childIndex returns the child of internal node n that would hold key.
func childIndex(n *node, key []byte) int {
	return sort.Search(len(n.keys), func(i int) bool { return bytes.Compare(n.keys[i], key) > 0 })
}

// leafIndex returns the position of key in leaf n, and whether it's there.
func leafIndex(n *node, key []byte) (int, bool) {
	return slices.BinarySearchFunc(n.keys, key, bytes.Compare)
}

func get(n *node, key []byte) ([]byte, bool) {
	if n == nil {
		return nil, false
	}
	for !n.leaf() {
		n = n.children[childIndex(n, key)]
	}
	if i, ok := leafIndex(n, key); ok {
		return n.vals[i], true
	}
	return nil, false
}

func (w *writer) set(key, val []byte) {
	if w.t.root == nil {
		w.t.root = &node{gen: w.gen, keys: [][]byte{}, vals: [][]byte{}}
	}
	root := w.mut(w.t.root)
	if right, sep := w.insert(root, key, val); right != nil {
		root = &node{gen: w.gen, keys: [][]byte{sep}, children: []*node{root, right}}
	}
	w.t.root = root
}

// insert puts key into n, which must already be mutable. If n overflows it
// splits, returning the new right sibling and its smallest key.
func (w *writer) insert(n *node, key, val []byte) (*node, []byte) {
	if n.leaf() {
		i, ok := leafIndex(n, key)
		if ok {
			n.vals[i] = val
			return nil, nil
		}
		n.keys = slices.Insert(n.keys, i, key)
		n.vals = slices.Insert(n.vals, i, val)
		w.t.len++
	} else {
		i := childIndex(n, key)
		c := w.mut(n.children[i])
		n.children[i] = c
		right, sep := w.insert(c, key, val)
		if right == nil {
			return nil, nil
		}
		n.keys = slices.Insert(n.keys, i, sep)
		n.children = slices.Insert(n.children, i+1, right)
	}
	if n.size() <= w.maxSize {
		return nil, nil
	}
	return w.split(n)
}

func (w *writer) split(n *node) (*node, []byte) {
	mid := n.size() / 2
	r := &node{gen: w.gen}
	if n.leaf() {
		r.keys = slices.Clone(n.keys[mid:])
		r.vals = slices.Clone(n.vals[mid:])
		clear(n.keys[mid:])
		clear(n.vals[mid:])
		n.keys, n.vals = n.keys[:mid], n.vals[:mid]
		return r, r.keys[0]
	}
	// Children [mid:] move right; separator keys[mid-1] moves up.
	sep := n.keys[mid-1]
	r.keys = slices.Clone(n.keys[mid:])
	r.children = slices.Clone(n.children[mid:])
	clear(n.keys[mid-1:])
	clear(n.children[mid:])
	n.keys, n.children = n.keys[:mid-1], n.children[:mid]
	return r, sep
}

func (w *writer) delete(key []byte) {
	if w.t.root == nil {
		return
	}
	if _, ok := get(w.t.root, key); !ok {
		return // don't copy any nodes for a no-op
	}
	root := w.mut(w.t.root)
	w.remove(root, key)
	for !root.leaf() && len(root.children) == 1 {
		root = root.children[0]
	}
	w.t.root = root
}

// remove deletes key, which must exist, from mutable node n. Afterwards a
// child that became empty is dropped, and a small child is merged with a
// neighbour when the two fit in one node. Nodes may stay below half full;
// the height still never grows on deletion.
func (w *writer) remove(n *node, key []byte) {
	if n.leaf() {
		i, _ := leafIndex(n, key)
		n.keys = slices.Delete(n.keys, i, i+1)
		n.vals = slices.Delete(n.vals, i, i+1)
		w.t.len--
		return
	}
	i := childIndex(n, key)
	c := w.mut(n.children[i])
	n.children[i] = c
	w.remove(c, key)
	switch {
	case c.size() == 0:
		n.children = slices.Delete(n.children, i, i+1)
		if i > 0 {
			n.keys = slices.Delete(n.keys, i-1, i)
		} else if len(n.keys) > 0 {
			n.keys = slices.Delete(n.keys, 0, 1)
		}
	case c.size() < w.maxSize/4:
		if i > 0 && n.children[i-1].size()+c.size() <= w.maxSize {
			w.merge(n, i-1)
		} else if i+1 < len(n.children) && n.children[i+1].size()+c.size() <= w.maxSize {
			w.merge(n, i)
		}
	}
}

// merge joins children i and i+1 of n into child i.
func (w *writer) merge(n *node, i int) {
	l := w.mut(n.children[i])
	r := n.children[i+1]
	if l.leaf() {
		l.keys = append(l.keys, r.keys...)
		l.vals = append(l.vals, r.vals...)
	} else {
		l.keys = append(append(l.keys, n.keys[i]), r.keys...)
		l.children = append(l.children, r.children...)
	}
	n.children[i] = l
	n.children = slices.Delete(n.children, i+1, i+2)
	n.keys = slices.Delete(n.keys, i, i+1)
}

// --- iteration ---

type frame struct {
	n *node
	i int
}

// iterator walks one tree version between bounds, in either direction.
type iterator struct {
	lower, upper []byte
	reverse      bool
	stack        []frame // path from the root to the current leaf
	started      bool
	key, val     []byte
	closed       bool
}

func newIterator(root *node, lower, upper []byte, reverse bool) *iterator {
	it := &iterator{lower: lower, upper: upper, reverse: reverse}
	if root == nil {
		return it
	}
	// Descend to the first position in iteration order.
	n := root
	for {
		var i int
		switch {
		case !reverse && lower == nil:
			i = 0
		case !reverse && n.leaf():
			i, _ = leafIndex(n, lower)
		case !reverse:
			i = childIndex(n, lower)
		case upper == nil:
			i = n.size() - 1
		case n.leaf():
			j, _ := leafIndex(n, upper) // first key >= upper
			i = j - 1
		default:
			// The child holding the last key < upper.
			i = sort.Search(len(n.keys), func(k int) bool { return bytes.Compare(n.keys[k], upper) >= 0 })
		}
		it.stack = append(it.stack, frame{n, i})
		if n.leaf() {
			break
		}
		if i < 0 || i >= len(n.children) {
			break
		}
		n = n.children[i]
	}
	return it
}

// advance moves to the next leaf entry in order, ignoring bounds.
func (it *iterator) advance() bool {
	step := 1
	if it.reverse {
		step = -1
	}
	if it.started {
		it.stack[len(it.stack)-1].i += step
	}
	it.started = true
	for len(it.stack) > 0 {
		top := &it.stack[len(it.stack)-1]
		if top.i < 0 || top.i >= top.n.size() {
			// Exhausted this node: pop and step the parent.
			it.stack = it.stack[:len(it.stack)-1]
			if len(it.stack) > 0 {
				it.stack[len(it.stack)-1].i += step
			}
			continue
		}
		if top.n.leaf() {
			it.key, it.val = top.n.keys[top.i], top.n.vals[top.i]
			return true
		}
		c := top.n.children[top.i]
		i := 0
		if it.reverse {
			i = c.size() - 1
		}
		it.stack = append(it.stack, frame{c, i})
	}
	return false
}

func (it *iterator) Next() bool {
	if it.closed {
		return false
	}
	for it.advance() {
		if !it.reverse {
			if it.upper != nil && bytes.Compare(it.key, it.upper) >= 0 {
				break
			}
			if it.lower != nil && bytes.Compare(it.key, it.lower) < 0 {
				continue // can't happen after the initial seek; kept for safety
			}
			return true
		}
		if it.lower != nil && bytes.Compare(it.key, it.lower) < 0 {
			break
		}
		if it.upper != nil && bytes.Compare(it.key, it.upper) >= 0 {
			continue
		}
		return true
	}
	it.stack, it.key, it.val = nil, nil, nil
	return false
}

func (it *iterator) Key() []byte   { return it.key }
func (it *iterator) Value() []byte { return it.val }
func (it *iterator) Err() error    { return nil }
func (it *iterator) Close() error {
	it.closed, it.stack, it.key, it.val = true, nil, nil, nil
	return nil
}
