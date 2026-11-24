package transfer

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/query"
	"github.com/sandgorgon/polydb/value"
)

// Query results export as rows (jsonl, native, csv) or as a graph (dot,
// graph).
//
// Rows in jsonl and native are records like a database export's:
//
//	{"polydb": "rows", "format": 1, "columns": ["p.name", …]}
//	{"blob": …} {"data": …} …     large values, before their row
//	{"row": [cell, …]}            …
//	{"end": true, "rows": n}
//
// with whole entities as {"id", "kind", "fields"} maps and relations as
// {"id", "type", "from", "to", "props"}. csv writes a header and plain
// text cells: text as is, numbers, times and IDs in their usual forms,
// bytes in base64, collections as JSON.
//
// A graph holds the entities in the result (whole, referenced, or on a
// path) as nodes, and the relations in the result and those between its
// nodes as edges.

// Result formats besides the export formats.
const (
	CSV       Format = "csv"
	DOT       Format = "dot"
	GraphJSON Format = "graph"
)

// ExportResult writes a query result in format. tx reads the entities
// and relations a graph needs; it is the transaction the query ran in.
func ExportResult(ctx context.Context, tx *engine.Tx, res *query.Result, w io.Writer, format Format) (*Stats, error) {
	cw := &countWriter{w: w}
	st := &Stats{}
	var err error
	switch format {
	case JSONL, Native:
		err = exportRows(ctx, res, cw, format, st)
	case CSV:
		err = exportCSV(ctx, res, cw, st)
	case DOT, GraphJSON:
		err = exportGraph(ctx, tx, res, cw, format, st)
	default:
		err = fmt.Errorf("unknown result format %q (jsonl, native, csv, dot or graph)", format)
	}
	st.Bytes = cw.n
	return st, err
}

func exportRows(ctx context.Context, res *query.Result, w io.Writer, format Format, st *Stats) error {
	rw, err := newRecordWriter(w, format)
	if err != nil {
		return err
	}
	cols := make([]value.Value, len(res.Columns))
	for i, c := range res.Columns {
		cols[i] = value.MustText(c.Name)
	}
	cl, _ := value.NewList(cols...)
	if err := rw.write(rec(pair{"polydb", value.MustText("rows")}, pair{"format", value.Int(exportFormat)},
		pair{"columns", cl})); err != nil {
		return err
	}
	blobs := map[[32]byte]bool{}
	// large writes a lazy value's blob once, and gives its reference.
	large := func(v value.Value) (value.Value, error) {
		l, ok := v.(value.Lazy)
		if !ok {
			return v, nil
		}
		if !blobs[l.Ref.Hash] {
			if err := writeBlob(rw, l); err != nil {
				return nil, err
			}
			blobs[l.Ref.Hash] = true
			st.Blobs++
		}
		return l.Ref, nil
	}
	for i, row := range res.Rows {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		cells := make([]value.Value, len(row))
		for j, c := range row {
			v, err := cellValue(c, large)
			if err != nil {
				return err
			}
			cells[j] = v
		}
		l, err := value.NewList(cells...)
		if err != nil {
			return err
		}
		if err := rw.write(rec(pair{"row", l})); err != nil {
			return err
		}
		st.Entities++ // rows
	}
	if err := rw.write(rec(pair{"end", value.Bool(true)}, pair{"rows", value.Int(len(res.Rows))})); err != nil {
		return err
	}
	return rw.flush()
}

// cellValue converts a result cell to a value: whole entities,
// relations and paths become maps and lists. fix prepares field values.
func cellValue(c any, fix func(value.Value) (value.Value, error)) (value.Value, error) {
	switch x := c.(type) {
	case *query.Entity:
		var ks, vs []value.Value
		for _, f := range x.Fields {
			if f.Value == nil {
				continue
			}
			v, err := fix(f.Value)
			if err != nil {
				return nil, err
			}
			ks, vs = append(ks, value.MustText(f.Name)), append(vs, v)
		}
		fields, err := value.NewMap(ks, vs)
		if err != nil {
			return nil, err
		}
		var kind value.Value
		if x.Kind != "" {
			kind = value.MustText(x.Kind)
		}
		return rec(pair{"id", x.ID}, pair{"kind", kind}, pair{"fields", fields}), nil
	case *query.Relation:
		return rec(pair{"id", x.ID}, pair{"type", value.MustText(x.Type)}, pair{"from", x.From}, pair{"to", x.To},
			pair{"props", x.Props}), nil
	case *query.Path:
		var vs []value.Value
		for i, n := range x.Nodes {
			v, err := cellValue(n, fix)
			if err != nil {
				return nil, err
			}
			vs = append(vs, v)
			if i < len(x.Rels) {
				r, _ := cellValue(x.Rels[i], fix)
				vs = append(vs, r)
			}
		}
		return value.NewList(vs...)
	case value.Value:
		return fix(x)
	case nil:
		return value.Null, nil
	}
	return nil, fmt.Errorf("a result cell of type %T", c)
}

func exportCSV(ctx context.Context, res *query.Result, w io.Writer, st *Stats) error {
	cw := csv.NewWriter(w)
	head := make([]string, len(res.Columns))
	for i, c := range res.Columns {
		head[i] = c.Name
	}
	if err := cw.Write(head); err != nil {
		return err
	}
	for i, row := range res.Rows {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		rec := make([]string, len(row))
		for j, c := range row {
			s, err := plainText(c)
			if err != nil {
				return err
			}
			rec[j] = s
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
		st.Entities++
	}
	cw.Flush()
	return cw.Error()
}

// plainText renders a cell for csv.
func plainText(c any) (string, error) {
	switch x := c.(type) {
	case *query.Entity:
		return x.ID.Base32(), nil
	case *query.Relation:
		return x.ID.Base32(), nil
	case *query.Path:
		v, err := cellValue(x, func(v value.Value) (value.Value, error) { return value.Null, nil })
		if err != nil {
			return "", err
		}
		b, err := value.AppendJSON(nil, v)
		return string(b), err
	case nil:
		return "", nil
	}
	v, err := value.Load(c.(value.Value))
	if err != nil {
		return "", err
	}
	switch x := v.(type) {
	case value.Text:
		return x.Go(), nil
	case value.Int, value.Bool:
		return x.String(), nil
	case value.Float:
		return strconv.FormatFloat(x.Go(), 'g', -1, 64), nil
	case value.Decimal:
		return x.Plain(), nil
	case value.BigInt:
		return x.Go().String(), nil
	case value.Time:
		return x.Go().Format(time.RFC3339Nano), nil
	case value.Date:
		return x.Go().Format("2006-01-02"), nil
	case value.Duration:
		return time.Duration(x).String(), nil
	case value.ID:
		return x.Base32(), nil
	case value.Ref:
		return x.Target.Base32(), nil
	case value.Bytes:
		raw, _, _ := value.Large(x)
		return base64.StdEncoding.EncodeToString(raw), nil
	case value.Image:
		return base64.StdEncoding.EncodeToString(x.Data()), nil
	case value.Doc:
		return x.Text(), nil
	}
	if value.IsNull(v) {
		return "", nil
	}
	b, err := value.AppendJSON(nil, v)
	return string(b), err
}

// --- graphs ---

type graphNode struct {
	id     value.ID
	kind   string
	label  string
	fields value.Map
}

type graphEdge struct {
	id, from, to value.ID
	typ          string
	props        value.Map
}

func exportGraph(ctx context.Context, tx *engine.Tx, res *query.Result, w io.Writer, format Format, st *Stats) error {
	nodes := map[value.ID]*graphNode{}
	var order []value.ID
	edges := map[value.ID]*graphEdge{}
	var edgeOrder []value.ID
	addNode := func(id value.ID) error {
		if nodes[id] != nil {
			return nil
		}
		n, ok, err := tx.Exists(id)
		if err != nil || !ok || n != model.NodeEntity {
			return err
		}
		e, err := tx.Get(id)
		if err != nil {
			return err
		}
		gn := &graphNode{id: id}
		if e.Kind != nil {
			gn.kind = e.Kind.Name
		}
		var ks, vs []value.Value
		for _, f := range e.Fields {
			if f.Value == nil {
				continue
			}
			v := f.Value
			if l, ok := v.(value.Lazy); ok {
				v = l.Ref // a graph names large values, without their content
			}
			ks, vs = append(ks, value.MustText(f.Name)), append(vs, v)
		}
		if gn.fields, err = value.NewMap(ks, vs); err != nil {
			return err
		}
		gn.label = nodeLabel(gn)
		nodes[id] = gn
		order = append(order, id)
		return nil
	}
	addEdge := func(r *query.Relation) {
		if edges[r.ID] == nil {
			edges[r.ID] = &graphEdge{id: r.ID, from: r.From, to: r.To, typ: r.Type, props: r.Props}
			edgeOrder = append(edgeOrder, r.ID)
		}
	}
	var visit func(c any) error
	visit = func(c any) error {
		switch x := c.(type) {
		case *query.Entity:
			return addNode(x.ID)
		case *query.Relation:
			addEdge(x)
			if err := addNode(x.From); err != nil {
				return err
			}
			return addNode(x.To)
		case *query.Path:
			for _, n := range x.Nodes {
				if err := visit(n); err != nil {
					return err
				}
			}
			for _, r := range x.Rels {
				if err := visit(r); err != nil {
					return err
				}
			}
		case value.Ref:
			return addNode(x.Target)
		case value.ID:
			return addNode(x)
		}
		return nil
	}
	for i, row := range res.Rows {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		for _, c := range row {
			if err := visit(c); err != nil {
				return err
			}
		}
	}
	// The relations between the nodes.
	for _, id := range order {
		rels, err := tx.Relations(id, engine.Out, "")
		if err != nil {
			return err
		}
		for _, r := range rels {
			if nodes[r.To] == nil {
				continue
			}
			rt, _ := tx.Catalog().RelationTypeByID(r.Type)
			addEdge(&query.Relation{ID: r.ID, Type: rt.Name, From: r.From, To: r.To, Props: r.Props})
		}
	}
	st.Entities, st.Relations = int64(len(order)), int64(len(edgeOrder))
	bw := bufio.NewWriter(w)
	var err error
	if format == DOT {
		err = writeDOT(bw, nodes, order, edges, edgeOrder)
	} else {
		err = writeGraphJSON(bw, nodes, order, edges, edgeOrder)
	}
	if err != nil {
		return err
	}
	return bw.Flush()
}

// nodeLabel names a node: its kind and a name-like field, else its ID.
func nodeLabel(n *graphNode) string {
	for _, f := range []string{"name", "title", "label"} {
		if v, ok := n.fields.Get(value.MustText(f)); ok {
			if t, ok := v.(value.Text); ok {
				if n.kind == "" {
					return t.Go()
				}
				return n.kind + ": " + t.Go()
			}
		}
	}
	if n.kind == "" {
		return n.id.Base32()
	}
	return n.kind + " " + n.id.Base32()
}

func dotQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}

func writeDOT(w *bufio.Writer, nodes map[value.ID]*graphNode, order []value.ID, edges map[value.ID]*graphEdge, edgeOrder []value.ID) error {
	w.WriteString("digraph polydb {\n")
	for _, id := range order {
		n := nodes[id]
		fmt.Fprintf(w, "  %s [label=%s];\n", dotQuote(id.Base32()), dotQuote(n.label))
	}
	for _, id := range edgeOrder {
		e := edges[id]
		if nodes[e.from] == nil || nodes[e.to] == nil {
			continue // a field endpoint
		}
		fmt.Fprintf(w, "  %s -> %s [label=%s];\n", dotQuote(e.from.Base32()), dotQuote(e.to.Base32()), dotQuote(e.typ))
	}
	_, err := w.WriteString("}\n")
	return err
}

func writeGraphJSON(w *bufio.Writer, nodes map[value.ID]*graphNode, order []value.ID, edges map[value.ID]*graphEdge, edgeOrder []value.ID) error {
	var ns, es []value.Value
	for _, id := range order {
		n := nodes[id]
		var kind value.Value
		if n.kind != "" {
			kind = value.MustText(n.kind)
		}
		ns = append(ns, rec(pair{"id", value.MustText(id.Base32())}, pair{"kind", kind},
			pair{"label", value.MustText(n.label)}, pair{"fields", n.fields}))
	}
	for _, id := range edgeOrder {
		e := edges[id]
		es = append(es, rec(pair{"id", value.MustText(id.Base32())}, pair{"type", value.MustText(e.typ)},
			pair{"from", value.MustText(e.from.Base32())}, pair{"to", value.MustText(e.to.Base32())}, pair{"props", e.props}))
	}
	nl, err := value.NewList(ns...)
	if err != nil {
		return err
	}
	el, err := value.NewList(es...)
	if err != nil {
		return err
	}
	b, err := value.AppendJSON(nil, rec(pair{"nodes", nl}, pair{"edges", el}))
	if err != nil {
		return err
	}
	w.Write(b)
	return w.WriteByte('\n')
}
