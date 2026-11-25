package query

import (
	"github.com/sandgorgon/polydb/internal/lang"
	"github.com/sandgorgon/polydb/value"
)

// A statement's rows count against its memory budget (Context.MaxMemory,
// lang.Budget): after its source and each step, the rows it then holds
// are estimated, and while a source binds rows each one is charged a
// little, so a runaway pattern (a cross product, say) stops before it
// fills memory rather than after.

// Estimates, in bytes, of what values and rows take in memory.
const (
	rowBytes   = 48 // a row and its map
	varBytes   = 40 // one variable's entry
	scalarSize = 16
)

// boundCheckEvery is how many rows a source binds between checks.
const boundCheckEvery = 1024

// hold estimates what rows hold and checks it against the budget.
func (x *executor) hold(n lang.Node, rows []prow) error {
	x.bound = 0
	if x.budget == nil || x.budget.Max <= 0 {
		return nil
	}
	var size int64
	for _, r := range rows {
		size += rowSize(r.vars)
		for _, g := range r.group {
			size += rowSize(g)
		}
	}
	if err := x.budget.Hold(size); err != nil {
		return errAt(n, "limit", "%v", err)
	}
	return nil
}

// bindRow charges one row a source bound, checking now and then.
func (x *executor) bindRow(n lang.Node, vars int) error {
	if x.budget == nil || x.budget.Max <= 0 {
		return nil
	}
	x.bound++
	if x.bound%boundCheckEvery != 0 {
		return nil
	}
	if err := x.budget.Hold(x.bound * (rowBytes + int64(vars)*varBytes)); err != nil {
		return errAt(n, "limit", "%v", err)
	}
	return nil
}

func rowSize(row lang.Row) int64 {
	n := int64(rowBytes)
	for k, v := range row {
		n += varBytes + int64(len(k)) + valueSize(v)
	}
	return n
}

// valueSize estimates a value's size in memory. A lazy value counts only
// its reference: its content counts when it is loaded.
func valueSize(v value.Value) int64 {
	switch x := v.(type) {
	case value.Text:
		return scalarSize + int64(len(x.Go()))
	case value.Bytes, value.Image:
		return scalarSize + int64(value.LargeLen(x))
	case value.Lazy:
		return 64
	case value.List:
		n := int64(24)
		for _, e := range x.All() {
			n += valueSize(e)
		}
		return n
	case value.Array:
		n := int64(24)
		for _, e := range x.All() {
			n += valueSize(e)
		}
		return n
	case value.Set:
		n := int64(24)
		for e := range x.All() {
			n += valueSize(e)
		}
		return n
	case value.Map:
		n := int64(48)
		for k, e := range x.All() {
			n += valueSize(k) + valueSize(e)
		}
		return n
	case value.Record:
		n := int64(48)
		for name, e := range x.All() {
			n += int64(len(name)) + valueSize(e)
		}
		return n
	case value.Doc:
		return scalarSize + int64(len(x.Text()))
	case value.Queue:
		n := int64(48)
		for _, e := range x.Items() {
			n += valueSize(e)
		}
		return n
	}
	return scalarSize
}
