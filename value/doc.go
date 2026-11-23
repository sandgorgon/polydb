package value

import (
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/text/unicode/norm"
)

// Format is a document format: the "key on how to interpret" a doc value
// (spec/types.md §6). It validates text and parses it into a tree of values.
type Format interface {
	// Name is the format's name, as written in doc<Name>.
	Name() string
	// Parse validates text and returns its parsed form.
	Parse(text string) (Value, error)
}

// Doc is text that is valid in a named format. The text is kept exactly as
// written (after NFC normalization).
type Doc struct {
	format string
	text   string
}

// NewDoc normalizes text to NFC, checks it is valid in format f and returns
// it as a Doc.
func NewDoc(f Format, text string) (Doc, error) {
	t, err := NewText(text)
	if err != nil {
		return Doc{}, err
	}
	if _, err := f.Parse(t.s); err != nil {
		return Doc{}, fmt.Errorf("invalid %s document: %w", f.Name(), err)
	}
	return Doc{f.Name(), t.s}, nil
}

func (Doc) Code() Code { return CodeDoc }

// Format returns the name of the document's format.
func (d Doc) Format() string { return d.format }

// Text returns the document text.
func (d Doc) Text() string { return d.text }

// String returns a cast expression that rebuilds the document.
func (d Doc) String() string {
	return "cast(" + QuoteText(d.text) + " as " + DocOf(d.format).String() + ")"
}

// --- format registry ---

var (
	formatsMu sync.RWMutex
	formats   = map[string]Format{}
)

// RegisterFormat adds a base format, which becomes available to every
// database. It fails if the name is invalid or already registered.
func RegisterFormat(f Format) error {
	if err := ValidateName("format", f.Name()); err != nil {
		return err
	}
	formatsMu.Lock()
	defer formatsMu.Unlock()
	if formats[f.Name()] != nil {
		return fmt.Errorf("format %q is already registered", f.Name())
	}
	formats[f.Name()] = f
	return nil
}

// LookupFormat returns the registered base format with the given name.
func LookupFormat(name string) (Format, bool) {
	formatsMu.RLock()
	defer formatsMu.RUnlock()
	f, ok := formats[name]
	return f, ok
}

func init() {
	for _, f := range []Format{
		formatFunc{"json", parseJSON},
		formatFunc{"xml", parseXML},
		formatFunc{"csv", func(s string) (Value, error) { return parseDelimited(s, ',') }},
		formatFunc{"tsv", func(s string) (Value, error) { return parseDelimited(s, '\t') }},
	} {
		if err := RegisterFormat(f); err != nil {
			panic(err)
		}
	}
}

type formatFunc struct {
	name  string
	parse func(string) (Value, error)
}

func (f formatFunc) Name() string                     { return f.name }
func (f formatFunc) Parse(text string) (Value, error) { return f.parse(text) }

// DerivedFormat is a format defined on top of a base format, whose parsed
// value must also conform to a shape (spec/types.md §6.1). Derived formats
// are defined per database in the catalog.
type DerivedFormat struct {
	name  string
	base  Format
	shape *Type
	table *TableSpec // set for table formats (NewTableFormat)
}

// NewDerivedFormat returns a format named name that accepts text valid in
// base whose parsed value conforms to shape.
func NewDerivedFormat(name string, base Format, shape *Type) (*DerivedFormat, error) {
	if err := ValidateName("format", name); err != nil {
		return nil, err
	}
	if err := shape.Validate(); err != nil {
		return nil, err
	}
	return &DerivedFormat{name: name, base: base, shape: shape}, nil
}

func (f *DerivedFormat) Name() string { return f.name }
func (f *DerivedFormat) Base() Format { return f.base }
func (f *DerivedFormat) Shape() *Type { return f.shape }

// Parse parses text with the base format and conforms the result to the
// shape, so for example JSON objects become records. On a json base, a
// number at a decimal position of the shape is read exactly as a decimal
// (2.50 stays 2.50) instead of as a float.
func (f *DerivedFormat) Parse(text string) (Value, error) {
	var v Value
	var err error
	if f.table != nil {
		if v, err = f.base.Parse(text); err != nil {
			return nil, err
		}
		return f.table.parse(v)
	}
	if f.base.Name() == "json" {
		if v, err = parseJSONMode(text, true); err == nil {
			v, err = settleNumbers(v, f.shape)
		}
	} else {
		v, err = f.base.Parse(text)
	}
	if err != nil {
		return nil, err
	}
	return Conform(v, f.shape)
}

// settleNumbers turns the exact decimals of an exact JSON parse back into
// floats, except where t expects a decimal. Rounding the exact value to
// the nearest float gives the same float as parsing the text would.
func settleNumbers(v Value, t *Type) (Value, error) {
	var r *Type
	if t != nil {
		r = Required(t)
	}
	switch x := v.(type) {
	case Decimal:
		if wantsDecimal(r) {
			return x, nil
		}
		f, _ := x.Rat().Float64()
		return NewFloat(f), nil
	case List:
		var et *Type
		if r != nil && (r.Code == CodeList || r.Code == CodeArray || r.Code.IsQueue()) {
			et = r.Elem
		}
		elems := x.Values()
		for i, e := range elems {
			var err error
			if elems[i], err = settleNumbers(e, et); err != nil {
				return nil, err
			}
		}
		return NewList(elems...)
	case Map:
		keys := x.Keys()
		vals := make([]Value, len(keys))
		for i, k := range keys {
			var ft *Type
			if r != nil {
				switch r.Code {
				case CodeMap:
					ft = r.Elem
				case CodeRecord:
					if kt, ok := k.(Text); ok {
						for _, fd := range r.Fields {
							if fd.Name == kt.Go() {
								ft = fd.Type
							}
						}
					}
				}
			}
			v, _ := x.Get(k)
			var err error
			if vals[i], err = settleNumbers(orNull(v), ft); err != nil {
				return nil, err
			}
		}
		return NewMap(keys, vals)
	}
	return v, nil
}

// wantsDecimal reports whether a value of type t should keep a JSON
// number as a decimal.
func wantsDecimal(t *Type) bool {
	switch {
	case t == nil:
		return false
	case t.Code == CodeDecimal:
		return true
	case t.Code == CodeUnion:
		dec, flt := false, false
		for _, a := range t.Alts {
			switch Required(a).Code {
			case CodeDecimal:
				dec = true
			case CodeFloat:
				flt = true
			}
		}
		return dec && !flt
	}
	return false
}

// --- json ---

// parseJSON parses JSON: objects become map<text, any>, arrays list<any>,
// numbers int when whole and in range (else float), strings text. A
// repeated object key keeps its last value.
func parseJSON(s string) (Value, error) { return parseJSONMode(s, false) }

// parseJSONMode parses JSON. With exact set, numbers that aren't whole
// become exact decimals instead of floats.
func parseJSONMode(s string, exact bool) (Value, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	v, err := jsonValue(dec, exact)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after the JSON value")
	}
	return v, nil
}

func jsonValue(dec *json.Decoder, exact bool) (Value, error) {
	tok, err := dec.Token()
	if err != nil {
		if err == io.EOF {
			err = errors.New("unexpected end of JSON")
		}
		return nil, err
	}
	switch t := tok.(type) {
	case nil:
		return Null, nil
	case bool:
		return Bool(t), nil
	case string:
		return NewText(t)
	case json.Number:
		if exact {
			if d, ok := jsonDecimal(string(t)); ok {
				return d, nil
			}
		}
		return jsonNumber(string(t))
	case json.Delim:
		switch t {
		case '[':
			var elems []Value
			for dec.More() {
				v, err := jsonValue(dec, exact)
				if err != nil {
					return nil, err
				}
				elems = append(elems, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return NewList(elems...)
		case '{':
			var keys []string
			vals := map[string]Value{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k := norm.NFC.String(kt.(string))
				v, err := jsonValue(dec, exact)
				if err != nil {
					return nil, err
				}
				if _, dup := vals[k]; !dup {
					keys = append(keys, k)
				}
				vals[k] = v
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			mk := make([]Value, len(keys))
			mv := make([]Value, len(keys))
			for i, k := range keys {
				mk[i], mv[i] = Text{k}, vals[k]
			}
			return NewMap(mk, mv)
		}
	}
	return nil, fmt.Errorf("unexpected JSON token %v", tok)
}

// jsonNumber converts a JSON number to an int if it is whole and fits in 64
// bits, and to a float otherwise.
func jsonNumber(s string) (Value, error) {
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return Int(i), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("JSON number %s is out of range", s)
	}
	// Forms like 1.0 or 1e3 are whole too. Check exactly, but only when the
	// exponent is small, since big.Rat would expand a huge exponent.
	if f == math.Trunc(f) && math.Abs(f) <= 1<<63 && smallExponent(s) {
		if r, ok := new(big.Rat).SetString(s); ok && r.IsInt() && r.Num().IsInt64() {
			return Int(r.Num().Int64()), nil
		}
	}
	return NewFloat(f), nil
}

// jsonDecimal reads a JSON number that isn't a whole int as an exact
// decimal. It reports false for whole numbers, which stay ints, and for
// numbers too large for a decimal.
func jsonDecimal(s string) (Decimal, bool) {
	if v, err := jsonNumber(s); err == nil && v.Code() == CodeInt {
		return Decimal{}, false
	}
	mant, exp := s, 0
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		if !smallExponent(s) {
			return Decimal{}, false
		}
		e, err := strconv.Atoi(strings.TrimPrefix(s[i+1:], "+"))
		if err != nil {
			return Decimal{}, false
		}
		mant, exp = s[:i], e
	}
	d, err := ParseDecimal(mant)
	if err != nil {
		return Decimal{}, false
	}
	scale := d.Scale() - exp
	coef := d.Coefficient()
	if scale < 0 {
		coef.Mul(coef, pow10(-scale))
		scale = 0
	}
	if d, err = NewDecimal(coef, scale); err != nil {
		return Decimal{}, false
	}
	return d, true
}

func smallExponent(s string) bool {
	i := strings.IndexAny(s, "eE")
	if i < 0 {
		return true
	}
	e, err := strconv.Atoi(s[i+1:])
	return err == nil && e >= -400 && e <= 400
}

// --- xml ---

// XMLElementType is the type of a parsed XML element (spec/types.md §6.2).
var XMLElementType = MustParseType("record{name: text, attrs: map<text, text>, children: list<any>}")

// parseXML parses an XML document into its root element. Each element is a
// record{name, attrs, children}; text nodes are text. Whitespace-only text,
// comments, processing instructions and directives are dropped. Namespaced
// names are written {uri}local; namespace declarations keep their xmlns:p
// form.
func parseXML(s string) (Value, error) {
	dec := xml.NewDecoder(strings.NewReader(s))
	type frame struct {
		name     string
		attrs    Map
		children []Value
		text     strings.Builder
	}
	var stack []*frame
	var root Value
	flush := func(f *frame) error {
		if t := f.text.String(); strings.TrimSpace(t) != "" {
			tv, err := NewText(t)
			if err != nil {
				return err
			}
			f.children = append(f.children, tv)
		}
		f.text.Reset()
		return nil
	}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if len(stack) == 0 && root != nil {
				return nil, errors.New("more than one root element")
			}
			if len(stack) > 0 {
				if err := flush(stack[len(stack)-1]); err != nil {
					return nil, err
				}
			}
			keys := make([]Value, len(t.Attr))
			vals := make([]Value, len(t.Attr))
			for i, a := range t.Attr {
				if keys[i], err = NewText(xmlName(a.Name, true)); err != nil {
					return nil, err
				}
				if vals[i], err = NewText(a.Value); err != nil {
					return nil, err
				}
			}
			attrs, err := NewMap(keys, vals)
			if err != nil {
				return nil, err
			}
			stack = append(stack, &frame{name: xmlName(t.Name, false), attrs: attrs})
		case xml.EndElement:
			f := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if err := flush(f); err != nil {
				return nil, err
			}
			children, err := NewList(f.children...)
			if err != nil {
				return nil, err
			}
			name, err := NewText(f.name)
			if err != nil {
				return nil, err
			}
			el, err := NewRecord([]string{"name", "attrs", "children"}, []Value{name, f.attrs, children})
			if err != nil {
				return nil, err
			}
			if len(stack) == 0 {
				root = el
			} else {
				stack[len(stack)-1].children = append(stack[len(stack)-1].children, el)
			}
		case xml.CharData:
			if len(stack) == 0 {
				if strings.TrimSpace(string(t)) != "" {
					return nil, errors.New("text outside the root element")
				}
				continue
			}
			stack[len(stack)-1].text.Write(t)
		}
	}
	if root == nil {
		return nil, errors.New("no root element")
	}
	return root, nil
}

func xmlName(n xml.Name, attr bool) string {
	switch {
	case n.Space == "":
		return n.Local
	case attr && n.Space == "xmlns":
		return "xmlns:" + n.Local
	}
	return "{" + n.Space + "}" + n.Local
}

// --- csv / tsv ---

// parseDelimited parses CSV (or TSV with a tab separator) into
// list<list<text>>. Every row must have the same number of fields.
func parseDelimited(s string, sep rune) (Value, error) {
	r := csv.NewReader(strings.NewReader(s))
	r.Comma = sep
	var rows []Value
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		fields := make([]Value, len(rec))
		for i, f := range rec {
			if fields[i], err = NewText(f); err != nil {
				return nil, err
			}
		}
		row, err := NewList(fields...)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return NewList(rows...)
}
