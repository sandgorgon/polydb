package value

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Table formats are derived formats on a delimited base (csv or tsv) that
// read rows: with a header line, by column name, and with declared
// columns, as typed records (spec/types.md §6.3).

// Column is one declared column of a table format.
type Column struct {
	Name string
	// Type is the column's type: bool, int, bigint, float, decimal, text,
	// date, time, duration, id or any (read as text), optionally T?.
	Type *Type
	// Pos is the column's position, counting from 0, or -1 to find it by
	// name in the header (with a header) or by declared order (without).
	Pos int
	// Default is the value for an empty or missing cell; nil for none.
	Default Value
}

// TableSpec says how a table format reads rows.
type TableSpec struct {
	Header  bool     // the first line names the columns
	Columns []Column // none: rows are read whole
}

// columnTypes are the types a column may have: cells are text, and these
// are the types text converts to.
var columnTypes = map[Code]bool{
	CodeBool: true, CodeInt: true, CodeBigInt: true, CodeFloat: true, CodeDecimal: true,
	CodeText: true, CodeDate: true, CodeTime: true, CodeDuration: true, CodeID: true, CodeAny: true,
}

// Validate checks the columns: valid names and types, no name twice, no
// position read twice, and defaults of the column's type.
func (s TableSpec) Validate() error {
	names := map[string]bool{}
	pos := map[int]string{}
	for i, c := range s.Columns {
		if err := ValidateName("column", c.Name); err != nil {
			return err
		}
		if names[c.Name] {
			return fmt.Errorf("column %s is declared twice", c.Name)
		}
		names[c.Name] = true
		if c.Type == nil || !columnTypes[c.Type.Code] {
			return fmt.Errorf("column %s: a column holds bool, int, bigint, float, decimal, text, date, time, duration, id or any, not %v", c.Name, c.Type)
		}
		if c.Pos < -1 {
			return fmt.Errorf("column %s: position %d is negative", c.Name, c.Pos)
		}
		p := c.Pos
		if p < 0 && !s.Header {
			p = i
		}
		if p >= 0 {
			if other, dup := pos[p]; dup {
				return fmt.Errorf("columns %s and %s both read column %d", other, c.Name, p)
			}
			pos[p] = c.Name
		}
		if c.Default != nil {
			d, err := Conform(c.Default, c.Type)
			if err != nil || IsNull(d) {
				return fmt.Errorf("column %s: default %s isn't a %s", c.Name, c.Default, c.Type)
			}
		}
	}
	return nil
}

// Shape returns the type of the parsed value: a list of records with
// declared columns, of maps from column name to text with only a header,
// and of lists of text otherwise.
func (s TableSpec) Shape() *Type {
	switch {
	case len(s.Columns) > 0:
		fields := make([]RecordField, len(s.Columns))
		for i, c := range s.Columns {
			t := c.Type
			if t.Code == CodeAny {
				t = OptionalOf(TypeText)
			}
			fields[i] = RecordField{Name: c.Name, Type: t}
		}
		return ListOf(RecordOf(fields...))
	case s.Header:
		return ListOf(MapOf(TypeText, TypeText))
	}
	return ListOf(ListOf(TypeText))
}

// NewTableFormat returns a table format named name on a delimited base
// format, whose Parse gives rows as TableSpec.Shape describes.
func NewTableFormat(name string, base Format, spec TableSpec) (*DerivedFormat, error) {
	if err := ValidateName("format", name); err != nil {
		return nil, err
	}
	if err := spec.Validate(); err != nil {
		return nil, fmt.Errorf("format %s: %w", name, err)
	}
	cols := make([]Column, len(spec.Columns))
	for i, c := range spec.Columns {
		cols[i] = c
		if c.Default != nil {
			cols[i].Default, _ = Conform(c.Default, c.Type)
		}
	}
	spec.Columns = cols
	return &DerivedFormat{name: name, base: base, shape: spec.Shape(), table: &spec}, nil
}

// Table returns the format's table spec, or nil if it isn't a table
// format.
func (f *DerivedFormat) Table() *TableSpec { return f.table }

// parse reads rows from the base format's list of lists of text.
func (s *TableSpec) parse(v Value) (Value, error) {
	l, ok := v.(List)
	if !ok {
		return nil, fmt.Errorf("a table format needs a base that gives rows, not %s", v.Code())
	}
	rows := l.Values()
	cells := func(r Value) []string {
		out := []string{}
		if row, ok := r.(List); ok {
			for _, c := range row.Values() {
				if t, ok := c.(Text); ok {
					out = append(out, t.Go())
				}
			}
		}
		return out
	}
	var header []string
	if s.Header && len(rows) > 0 {
		header = cells(rows[0])
		for i, h := range header {
			if i == 0 {
				h = strings.TrimPrefix(h, "\ufeff") // a byte order mark from spreadsheet exports
			}
			header[i] = strings.TrimSpace(h)
		}
		rows = rows[1:]
	}
	if len(s.Columns) == 0 {
		if !s.Header {
			return l, nil
		}
		return s.rowsAsMaps(header, rows, cells)
	}
	idx, err := s.columnIndexes(header)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(s.Columns))
	for i, c := range s.Columns {
		names[i] = c.Name
	}
	out := make([]Value, len(rows))
	for r, row := range rows {
		cs := cells(row)
		vals := make([]Value, len(s.Columns))
		for i, c := range s.Columns {
			cell, present := "", false
			if idx[i] >= 0 && idx[i] < len(cs) {
				cell, present = cs[idx[i]], true
			}
			v, err := c.cell(cell, present)
			if err != nil {
				return nil, fmt.Errorf("row %d, column %s: %w", r+1, c.Name, err)
			}
			vals[i] = v
		}
		if out[r], err = NewRecord(names, vals); err != nil {
			return nil, err
		}
	}
	return NewList(out...)
}

func (s *TableSpec) rowsAsMaps(header []string, rows []Value, cells func(Value) []string) (Value, error) {
	keys := make([]Value, len(header))
	for i, h := range header {
		t, err := NewText(h)
		if err != nil {
			return nil, err
		}
		keys[i] = t
	}
	out := make([]Value, len(rows))
	for r, row := range rows {
		cs := cells(row)
		vals := make([]Value, len(header))
		for i := range header {
			vals[i] = Text{}
			if i < len(cs) {
				vals[i] = MustText(cs[i])
			}
		}
		m, err := NewMap(keys, vals)
		if err != nil {
			return nil, fmt.Errorf("header: %w", err)
		}
		out[r] = m
	}
	return NewList(out...)
}

// columnIndexes finds each declared column's position: by at, by name in
// the header, or by declared order. A column that isn't there is -1,
// which only optional and defaulted columns allow.
func (s *TableSpec) columnIndexes(header []string) ([]int, error) {
	idx := make([]int, len(s.Columns))
	for i, c := range s.Columns {
		switch {
		case c.Pos >= 0:
			idx[i] = c.Pos
		case s.Header:
			idx[i] = -1
			for j, h := range header {
				if h != c.Name {
					continue
				}
				if idx[i] >= 0 {
					return nil, fmt.Errorf("column %s appears more than once in the header; pick one with at", c.Name)
				}
				idx[i] = j
			}
			if idx[i] < 0 && header != nil && c.Default == nil && !c.Type.Nullable() {
				return nil, fmt.Errorf("the header has no column %s", c.Name)
			}
		default:
			idx[i] = i
		}
	}
	return idx, nil
}

// cell converts one cell. An empty or missing cell gives the default,
// null for an optional column, "" for a required text column, and is an
// error otherwise.
func (c *Column) cell(s string, present bool) (Value, error) {
	if s == "" {
		switch {
		case c.Default != nil:
			return c.Default, nil
		case c.Type.Nullable():
			return Null, nil
		case c.Type.Code == CodeText && present:
			return Text{}, nil
		case !present:
			return nil, fmt.Errorf("the row has no such column")
		}
		return nil, fmt.Errorf("the cell is empty")
	}
	v, err := ParseText(s, c.Type)
	if err != nil {
		return nil, err
	}
	return Conform(v, c.Type)
}

// ParseText reads text as a value of a scalar type: a number, bool, date,
// time, duration, id or text (any reads as text). Surrounding spaces are
// ignored except for text.
func ParseText(s string, t *Type) (Value, error) {
	code := t.Code
	if code == CodeText || code == CodeAny {
		return NewText(s)
	}
	s = strings.TrimSpace(s)
	bad := func(err error) error {
		if err == nil {
			return fmt.Errorf("%q isn't a %s", s, Required(t))
		}
		return fmt.Errorf("%q isn't a %s: %v", s, Required(t), err)
	}
	switch code {
	case CodeBool:
		switch strings.ToLower(s) {
		case "true":
			return Bool(true), nil
		case "false":
			return Bool(false), nil
		}
		return nil, bad(nil)
	case CodeInt:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, bad(nil)
		}
		return Int(n), nil
	case CodeBigInt:
		b, err := ParseBigInt(s)
		if err != nil {
			return nil, bad(nil)
		}
		return b, nil
	case CodeFloat:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, bad(nil)
		}
		return NewFloat(f), nil
	case CodeDecimal:
		d, err := ParseDecimal(s)
		if err != nil {
			return nil, bad(nil)
		}
		return d, nil
	case CodeDate:
		d, err := ParseDate(s)
		if err != nil {
			return nil, bad(nil)
		}
		return d, nil
	case CodeTime:
		v, err := ParseTime(s)
		if err != nil {
			return nil, bad(nil)
		}
		return v, nil
	case CodeDuration:
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, bad(nil)
		}
		return Duration(d), nil
	case CodeID:
		id, err := ParseID(s)
		if err != nil {
			return nil, bad(nil)
		}
		return id, nil
	}
	return nil, fmt.Errorf("text can't be read as %s", t)
}
