package value

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrQueueFull is returned when pushing to a fifo, lifo or priority queue
// that has reached its max size.
var ErrQueueFull = errors.New("queue is full")

// Queue is a queue value: fifo, lifo, ring or priority (spec/types.md §8).
// Queues are immutable like every value; Push and Pop return a new queue.
//
// Elements are kept in pop order, so the next element to pop is always
// first. Equality and ordering compare the elements in pop order (and the
// priorities, for priority queues); the size limit and direction come from
// the type and don't affect equality.
type Queue struct {
	code     Code
	lowFirst bool
	limit    int     // max size (fifo, lifo, priority; 0 = unbounded) or ring capacity
	items    []Value // in pop order
	prios    []int64 // priority queues only, parallel to items
	d        int
}

// NewQueue returns an empty queue of type t, which must be a queue type.
func NewQueue(t *Type) (Queue, error) {
	if !t.Code.IsQueue() {
		return Queue{}, fmt.Errorf("%s is not a queue type", t)
	}
	if t.Code == CodeRing && t.N < 1 {
		return Queue{}, errors.New("ring queue capacity must be at least 1")
	}
	return Queue{code: t.Code, lowFirst: t.LowFirst, limit: t.N, d: 2}, nil
}

// NewQueueOf returns a queue of type t holding items in pop order. For a
// priority queue, prios gives each item's priority and the items are put in
// pop order (stable, so equal priorities keep their given order); for other
// queues prios must be nil.
func NewQueueOf(t *Type, items []Value, prios []int64) (Queue, error) {
	q, err := NewQueue(t)
	if err != nil {
		return Queue{}, err
	}
	if (q.code == CodePriority) != (prios != nil) || (prios != nil && len(prios) != len(items)) {
		return Queue{}, errors.New("priorities must be given for exactly the items of a priority queue")
	}
	if q.limit > 0 && len(items) > q.limit {
		return Queue{}, fmt.Errorf("%d items exceed the queue's limit of %d", len(items), q.limit)
	}
	q.items = slices.Clone(items)
	if q.d, err = checkElems("queue", q.items); err != nil {
		return Queue{}, err
	}
	if prios != nil {
		idx := make([]int, len(items))
		for i := range idx {
			idx[i] = i
		}
		slices.SortStableFunc(idx, func(a, b int) int { return q.cmpPrio(prios[a], prios[b]) })
		sorted := make([]Value, len(idx))
		q.prios = make([]int64, len(idx))
		for i, j := range idx {
			sorted[i], q.prios[i] = q.items[j], prios[j]
		}
		q.items = sorted
	}
	return q, nil
}

// cmpPrio orders priorities in pop order: negative if a pops before b.
func (q Queue) cmpPrio(a, b int64) int {
	if q.lowFirst {
		return cmpInt64(a, b)
	}
	return cmpInt64(b, a)
}

func (q Queue) Code() Code     { return q.code }
func (q Queue) depth() int     { return max(q.d, 2) }
func (q Queue) Len() int       { return len(q.items) }
func (q Queue) LowFirst() bool { return q.lowFirst }

// Limit returns the max size (0 for unbounded) or, for a ring, its capacity.
func (q Queue) Limit() int { return q.limit }

// Push returns q with v added. priority is used only by priority queues.
// A full ring drops its oldest element; other full queues return
// ErrQueueFull.
func (q Queue) Push(v Value, priority int64) (Queue, error) {
	if v == nil {
		v = Null
	}
	if d := depthOf(v) + 1; d > MaxNesting {
		return q, fmt.Errorf("queue nested more than %d levels deep", MaxNesting)
	}
	full := q.limit > 0 && len(q.items) >= q.limit
	if full && q.code != CodeRing {
		return q, ErrQueueFull
	}
	if uint64(len(q.items)) >= MaxCollectionCount {
		return q, ErrQueueFull
	}
	n := q
	n.d = max(q.depth(), depthOf(v)+1)
	switch q.code {
	case CodeFifo:
		n.items = append(slices.Clip(q.items), v)
	case CodeRing:
		items := q.items
		if full {
			items = items[1:]
		}
		n.items = append(slices.Clip(items), v)
	case CodeLifo:
		n.items = append([]Value{v}, q.items...)
	case CodePriority:
		// After every element that pops before or with it, so equal
		// priorities pop oldest first.
		i := len(q.prios)
		for j, p := range q.prios {
			if q.cmpPrio(priority, p) < 0 {
				i = j
				break
			}
		}
		n.items = slices.Insert(slices.Clone(q.items), i, v)
		n.prios = slices.Insert(slices.Clone(q.prios), i, priority)
	}
	return n, nil
}

// Pop returns q without its next element, and that element. On an empty
// queue it returns q and null.
func (q Queue) Pop() (Queue, Value) {
	if len(q.items) == 0 {
		return q, Null
	}
	n := q
	n.items = q.items[1:]
	if q.code == CodePriority {
		n.prios = q.prios[1:]
	}
	return n, q.items[0]
}

// Peek returns the next element without removing it, or null if q is empty.
func (q Queue) Peek() Value {
	if len(q.items) == 0 {
		return Null
	}
	return q.items[0]
}

// Items returns the elements in pop order.
func (q Queue) Items() []Value { return slices.Clone(q.items) }

// Priorities returns the priorities of a priority queue's elements in pop
// order, or nil for other queues.
func (q Queue) Priorities() []int64 { return slices.Clone(q.prios) }

// String writes the elements in pop order as a list literal; a priority
// queue's elements are written as priority => element pairs.
func (q Queue) String() string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range q.items {
		if i > 0 {
			b.WriteString(", ")
		}
		if q.code == CodePriority {
			fmt.Fprintf(&b, "%d => ", q.prios[i])
		}
		writeOperand(&b, v)
	}
	b.WriteByte(']')
	return b.String()
}
