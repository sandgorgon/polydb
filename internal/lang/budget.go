package lang

import "fmt"

// Budget is a statement's working memory, in bytes: the rows its steps
// hold (Hold) and the large values it loads (Load), checked against Max
// (spec/rhizome.md §4, Limits). One budget serves the executor and its
// evaluator. A nil *Budget, or Max ≤ 0, has no limit.
type Budget struct {
	Max          int64
	loaded, held int64
}

// NewBudget returns a budget of max bytes (≤ 0: no limit).
func NewBudget(max int64) *Budget { return &Budget{Max: max} }

// Used returns the bytes counted now.
func (b *Budget) Used() int64 {
	if b == nil {
		return 0
	}
	return b.loaded + b.held
}

// over reports whether n more bytes would pass Max.
func (b *Budget) over(n int64) bool { return b != nil && b.Max > 0 && b.loaded+b.held+n > b.Max }

// Load counts n bytes of a large value loaded for the rest of the
// statement, or fails if that would pass Max.
func (b *Budget) Load(n int64) error {
	if b == nil {
		return nil
	}
	if b.over(n) {
		return fmt.Errorf("the statement would hold more than its limit of %d bytes (max_memory)", b.Max)
	}
	b.loaded += n
	return nil
}

// Hold sets the bytes the statement's rows hold now, or fails if that
// would pass Max.
func (b *Budget) Hold(n int64) error {
	if b == nil {
		return nil
	}
	if b.Max > 0 && b.loaded+n > b.Max {
		return fmt.Errorf("the statement's rows would take more than its limit of %d bytes (max_memory); add a limit, a narrower where, or raise max_memory", b.Max)
	}
	b.held = n
	return nil
}
