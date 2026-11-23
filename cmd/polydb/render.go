package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/sandgorgon/polydb"
	"github.com/sandgorgon/polydb/value"
)

// maxCell is the widest a table cell is shown; longer values are cut.
const maxCell = 60

// print shows every result set of rows.
func (s *shell) print(rows polydb.Rows) error {
	for {
		cols := rows.Columns()
		var table [][]string
		for rows.Next() {
			cells := rows.Values()
			row := make([]string, len(cells))
			for i, c := range cells {
				row[i] = cellText(c)
			}
			table = append(table, row)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		res := rows.Result()
		switch {
		case len(cols) > 0:
			if s.format == "tsv" {
				s.tsv(cols, table)
			} else {
				s.table(cols, table)
				fmt.Fprintf(s.out, "(%d %s)\n", len(table), plural(len(table), "row", "rows"))
			}
		case res.RowsAffected > 0:
			fmt.Fprintf(s.out, "(%d changed)\n", res.RowsAffected)
		default:
			fmt.Fprintln(s.out, "ok")
		}
		for _, w := range res.Warnings {
			fmt.Fprintf(s.errOut, "warning: %s\n", w)
		}
		if !rows.NextResultSet() {
			return rows.Err()
		}
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func (s *shell) table(cols []polydb.Column, rows [][]string) {
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = utf8.RuneCountInString(c.Name)
	}
	for _, r := range rows {
		for i, c := range r {
			widths[i] = max(widths[i], utf8.RuneCountInString(c))
		}
	}
	line := func(cells []string) {
		var b strings.Builder
		for i, c := range cells {
			if i > 0 {
				b.WriteString(" │ ")
			}
			b.WriteString(c)
			if i < len(cells)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c)))
			}
		}
		fmt.Fprintln(s.out, b.String())
	}
	names := make([]string, len(cols))
	rules := make([]string, len(cols))
	for i, c := range cols {
		names[i], rules[i] = c.Name, strings.Repeat("─", widths[i])
	}
	line(names)
	fmt.Fprintln(s.out, strings.Join(rules, "─┼─"))
	for _, r := range rows {
		line(r)
	}
}

func (s *shell) tsv(cols []polydb.Column, rows [][]string) {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	fmt.Fprintln(s.out, strings.Join(names, "\t"))
	for _, r := range rows {
		fmt.Fprintln(s.out, strings.Join(r, "\t"))
	}
}

// cellText shows a cell compactly: text without quotes, other values in
// literal form, entities as Kind{field: value, …}.
func cellText(c any) string {
	var s string
	switch x := c.(type) {
	case nil:
		s = "null"
	case value.Text:
		s = x.Go()
	case value.Value:
		s = x.String()
	case *polydb.Entity:
		var fs []string
		for _, f := range x.Fields {
			if f.Value != nil {
				fs = append(fs, f.Name+": "+literal(f.Value))
			}
		}
		kind := x.Kind
		if kind == "" {
			kind = "entity"
		}
		s = kind + "{" + strings.Join(fs, ", ") + "}"
	case *polydb.Relation:
		s = x.Type
		if len(x.Props) > 0 {
			var ps []string
			for _, k := range sortedNames(x.Props) {
				ps = append(ps, k+": "+x.Props[k].String())
			}
			s += "{" + strings.Join(ps, ", ") + "}"
		}
	case *polydb.Path:
		var parts []string
		for i, n := range x.Nodes {
			if i > 0 {
				parts = append(parts, "~"+x.Rels[i-1].Type+"~>")
			}
			parts = append(parts, nodeName(n))
		}
		s = strings.Join(parts, " ")
	case *polydb.Blob:
		s = fmt.Sprintf("<%s, %d bytes>", x.Type, x.Length)
	default:
		s = fmt.Sprint(c)
	}
	s = strings.NewReplacer("\n", `\n`, "\t", `\t`, "\r", `\r`).Replace(s)
	if utf8.RuneCountInString(s) > maxCell {
		s = string([]rune(s)[:maxCell-1]) + "…"
	}
	return s
}

// literal shows a field value in literal form.
func literal(v any) string {
	switch x := v.(type) {
	case value.Value:
		return x.String()
	case *polydb.Blob:
		return fmt.Sprintf("<%s, %d bytes>", x.Type, x.Length)
	}
	return fmt.Sprint(v)
}

func nodeName(n any) string {
	e, ok := n.(*polydb.Entity)
	if !ok {
		return cellText(n)
	}
	if v, ok := e.Value("name").(value.Text); ok {
		return v.Go()
	}
	return e.Kind + " " + e.ID.String()
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// history keeps the shell's input history in ~/.polydb_history.
type history struct {
	path    string
	entries []string // oldest first
}

const historyMax = 1000

func loadHistory() *history {
	h := &history{}
	home, err := os.UserHomeDir()
	if err != nil {
		return h
	}
	h.path = filepath.Join(home, ".polydb_history")
	if data, err := os.ReadFile(h.path); err == nil {
		for _, l := range strings.Split(string(data), "\n") {
			if l != "" {
				h.entries = append(h.entries, l)
			}
		}
	}
	return h
}

func (h *history) Add(entry string) {
	if strings.TrimSpace(entry) == "" || (len(h.entries) > 0 && h.entries[len(h.entries)-1] == entry) {
		return
	}
	h.entries = append(h.entries, entry)
	if len(h.entries) > historyMax {
		h.entries = h.entries[len(h.entries)-historyMax:]
	}
	if h.path != "" {
		os.WriteFile(h.path, []byte(strings.Join(h.entries, "\n")+"\n"), 0o600)
	}
}

func (h *history) Len() int { return len(h.entries) }

func (h *history) At(i int) string { return h.entries[len(h.entries)-1-i] }
