package value

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// The JSON form of values (spec/types.md §12.3), used by exports. It is
// exact: decoding gives back the value that was encoded.
//
// Values JSON has a type for are plain JSON: null, true and false, int
// (a number), text (a string), list (an array) and a map whose keys are
// all text not starting with "$" (an object). Every other value is an
// object with one key starting with "$" that names its type, plus, for
// some, plain keys with details:
//
//	{"$float": 1.5}             also "nan", "inf", "-inf"
//	{"$bigint": "12345678901234567890"}
//	{"$decimal": "19.99"}
//	{"$bytes": "<base64>", "media": "image/svg+xml"}
//	{"$image": "<base64>"}
//	{"$time": "2026-11-21T14:30:00Z"}
//	{"$date": "2026-11-21"}
//	{"$duration": "1h30m"}
//	{"$id": "<id>"}   {"$ref": "<id>"}
//	{"$doc": "<text>", "format": "invoice"}
//	{"$array": [...]}  {"$set": [...]}  {"$pair": [k, v]}
//	{"$map": [[k, v], ...]}             a map with other keys
//	{"$record": {"name": v, ...}}       fields in order
//	{"$fifo": [...], "limit": 10}       also $lifo; items in pop order
//	{"$ring": [...], "capacity": 5}
//	{"$priority": [[prio, item], ...], "low_first": true, "limit": 0}
//	{"$blob": "<sha256>", "type": "bytes", "len": 123, "media": "…"}
//	                            a large value written separately (exports)
//
// When decoding, a number is an int if it is whole and fits in 64 bits,
// and a float otherwise, as for json docs.

// AppendJSON appends v's JSON form to dst. A Lazy value can't be encoded
// here: exports write it separately and encode its BlobRef.
func AppendJSON(dst []byte, v Value) ([]byte, error) {
	switch x := v.(type) {
	case nil, nullValue:
		return append(dst, "null"...), nil
	case Bool:
		return strconv.AppendBool(dst, bool(x)), nil
	case Int:
		return strconv.AppendInt(dst, int64(x), 10), nil
	case Text:
		return appendJSONString(dst, x.s), nil
	case List:
		return appendJSONArray(dst, x.elems)
	case Map:
		natural := true
		for _, k := range x.keys {
			if t, ok := k.(Text); !ok || strings.HasPrefix(t.s, "$") {
				natural = false
				break
			}
		}
		if !natural {
			dst = append(dst, `{"$map":[`...)
			for i, k := range x.keys {
				if i > 0 {
					dst = append(dst, ',')
				}
				var err error
				if dst, err = appendJSONArray(dst, []Value{k, x.vals[i]}); err != nil {
					return dst, err
				}
			}
			return append(dst, "]}"...), nil
		}
		dst = append(dst, '{')
		for i, k := range x.keys {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = append(appendJSONString(dst, k.(Text).s), ':')
			var err error
			if dst, err = AppendJSON(dst, x.vals[i]); err != nil {
				return dst, err
			}
		}
		return append(dst, '}'), nil
	case Float:
		dst = append(dst, `{"$float":`...)
		switch f := x.f; {
		case math.IsNaN(f):
			dst = append(dst, `"nan"`...)
		case math.IsInf(f, 1):
			dst = append(dst, `"inf"`...)
		case math.IsInf(f, -1):
			dst = append(dst, `"-inf"`...)
		default:
			dst = strconv.AppendFloat(dst, f, 'g', -1, 64)
		}
		return append(dst, '}'), nil
	case BigInt:
		return tagged(dst, "$bigint", x.big().String()), nil
	case Decimal:
		return tagged(dst, "$decimal", x.Plain()), nil
	case Bytes:
		dst = tagged(dst, "$bytes", base64.StdEncoding.EncodeToString(x.b))
		if x.mediaType != "" {
			dst = append(appendJSONString(append(dst[:len(dst)-1], `,"media":`...), x.mediaType), '}')
		}
		return dst, nil
	case Image:
		return tagged(dst, "$image", base64.StdEncoding.EncodeToString(x.data)), nil
	case Time:
		return tagged(dst, "$time", x.t.Format(time.RFC3339Nano)), nil
	case Date:
		return tagged(dst, "$date", x.Go().Format("2006-01-02")), nil
	case Duration:
		return tagged(dst, "$duration", time.Duration(x).String()), nil
	case ID:
		return tagged(dst, "$id", x.Base32()), nil
	case Ref:
		return tagged(dst, "$ref", x.Target.Base32()), nil
	case Doc:
		dst = tagged(dst, "$doc", x.text)
		return append(appendJSONString(append(dst[:len(dst)-1], `,"format":`...), x.format), '}'), nil
	case Array:
		return taggedArray(dst, "$array", x.elems)
	case Set:
		return taggedArray(dst, "$set", x.elems)
	case Pair:
		return taggedArray(dst, "$pair", []Value{x.Key(), x.Value()})
	case Record:
		dst = append(dst, `{"$record":{`...)
		for i, n := range x.names {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = append(appendJSONString(dst, n), ':')
			var err error
			if dst, err = AppendJSON(dst, x.vals[i]); err != nil {
				return dst, err
			}
		}
		return append(dst, "}}"...), nil
	case Queue:
		switch x.code {
		case CodeFifo, CodeLifo:
			name := "$fifo"
			if x.code == CodeLifo {
				name = "$lifo"
			}
			dst, err := taggedArray(dst, name, x.items)
			if err != nil {
				return dst, err
			}
			return append(strconv.AppendInt(append(dst[:len(dst)-1], `,"limit":`...), int64(x.limit), 10), '}'), nil
		case CodeRing:
			dst, err := taggedArray(dst, "$ring", x.items)
			if err != nil {
				return dst, err
			}
			return append(strconv.AppendInt(append(dst[:len(dst)-1], `,"capacity":`...), int64(x.limit), 10), '}'), nil
		}
		dst = append(dst, `{"$priority":[`...)
		for i, it := range x.items {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = append(strconv.AppendInt(append(dst, '['), x.prios[i], 10), ',')
			var err error
			if dst, err = AppendJSON(dst, it); err != nil {
				return dst, err
			}
			dst = append(dst, ']')
		}
		dst = append(append(dst, `],"low_first":`...), strconv.FormatBool(x.lowFirst)...)
		return append(strconv.AppendInt(append(dst, `,"limit":`...), int64(x.limit), 10), '}'), nil
	case BlobRef:
		dst = tagged(dst, "$blob", hex.EncodeToString(x.Hash[:]))
		dst = append(appendJSONString(append(dst[:len(dst)-1], `,"type":`...), x.Of.String()), `,"len":`...)
		dst = strconv.AppendInt(dst, x.Len, 10)
		if x.Media != "" {
			dst = appendJSONString(append(dst, `,"media":`...), x.Media)
		}
		return append(dst, '}'), nil
	case Lazy:
		return dst, errors.New("value: a lazy value has no JSON form; encode its reference")
	}
	return dst, fmt.Errorf("value: no JSON form for %s", v.Code())
}

func tagged(dst []byte, tag, s string) []byte {
	return append(appendJSONString(append(append(append(dst, `{"`...), tag...), `":`...), s), '}')
}

func taggedArray(dst []byte, tag string, vs []Value) ([]byte, error) {
	dst, err := appendJSONArray(append(append(append(dst, `{"`...), tag...), `":`...), vs)
	return append(dst, '}'), err
}

func appendJSONArray(dst []byte, vs []Value) ([]byte, error) {
	dst = append(dst, '[')
	for i, e := range vs {
		if i > 0 {
			dst = append(dst, ',')
		}
		var err error
		if dst, err = AppendJSON(dst, e); err != nil {
			return dst, err
		}
	}
	return append(dst, ']'), nil
}

// appendJSONString appends s as a JSON string, escaping what JSON
// requires and the characters that break HTML embedding or lines.
func appendJSONString(dst []byte, s string) []byte {
	b, _ := json.Marshal(s) // a string always marshals
	return append(dst, b...)
}

// --- decoding ---

// FormatLookup finds a doc format by name, for decoding docs.
type FormatLookup func(name string) (Format, bool)

// ParseJSON decodes one value from its JSON form.
func ParseJSON(data []byte, formats FormatLookup) (Value, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	v, err := DecodeJSON(d, formats)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("value: data after the JSON value")
	}
	return v, nil
}

// DecodeJSON decodes the next value from d, which must use numbers
// (Decoder.UseNumber). Object keys keep their order.
func DecodeJSON(d *json.Decoder, formats FormatLookup) (Value, error) {
	tok, err := d.Token()
	if err != nil {
		return nil, err
	}
	return decodeJSONFrom(d, tok, formats, 0)
}

func decodeJSONFrom(d *json.Decoder, tok json.Token, formats FormatLookup, depth int) (Value, error) {
	if depth > MaxNesting+2 {
		return nil, fmt.Errorf("value: JSON nested more than %d levels deep", MaxNesting)
	}
	switch t := tok.(type) {
	case nil:
		return Null, nil
	case bool:
		return Bool(t), nil
	case json.Number:
		return jsonNum(t)
	case string:
		return NewText(t)
	case json.Delim:
		if t == '[' {
			vs, err := jsonArray(d, formats, depth)
			if err != nil {
				return nil, err
			}
			return NewList(vs...)
		}
		if t == '{' {
			return jsonObject(d, formats, depth)
		}
	}
	return nil, fmt.Errorf("value: unexpected JSON %v", tok)
}

func jsonNum(n json.Number) (Value, error) {
	s := string(n)
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return Int(i), nil
	}
	if !strings.ContainsAny(s, ".eE") {
		return nil, fmt.Errorf("value: %s doesn't fit an int (use {\"$bigint\": \"%s\"})", s, s)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("value: number %s: %v", s, err)
	}
	if f == math.Trunc(f) && math.Abs(f) < 1<<63 {
		return Int(int64(f)), nil
	}
	return NewFloat(f), nil
}

func jsonArray(d *json.Decoder, formats FormatLookup, depth int) ([]Value, error) {
	var out []Value
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, err
		}
		if t, ok := tok.(json.Delim); ok && t == ']' {
			return out, nil
		}
		v, err := decodeJSONFrom(d, tok, formats, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
}

// jsonObject decodes an object: a map, or a tagged value.
func jsonObject(d *json.Decoder, formats FormatLookup, depth int) (Value, error) {
	var keys []string
	var vals []Value
	tag, tagIdx := "", -1
	var raw []json.RawMessage // tagged values keep their raw members
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, err
		}
		if t, ok := tok.(json.Delim); ok && t == '}' {
			break
		}
		k, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("value: object key %v", tok)
		}
		if strings.HasPrefix(k, "$") {
			if tag != "" {
				return nil, fmt.Errorf("value: object with two type tags, %s and %s", tag, k)
			}
			tag, tagIdx = k, len(keys)
		}
		var m json.RawMessage
		if err := d.Decode(&m); err != nil {
			return nil, err
		}
		keys = append(keys, k)
		raw = append(raw, m)
	}
	if tag == "" {
		for _, m := range raw {
			v, err := subJSON(m, formats, depth)
			if err != nil {
				return nil, err
			}
			vals = append(vals, v)
		}
		ks := make([]Value, len(keys))
		for i, k := range keys {
			t, err := NewText(k)
			if err != nil {
				return nil, err
			}
			ks[i] = t
		}
		return NewMap(ks, vals)
	}
	params := map[string]json.RawMessage{}
	for i, k := range keys {
		if i != tagIdx {
			params[k] = raw[i]
		}
	}
	return jsonTagged(tag, raw[tagIdx], params, formats, depth)
}

func subJSON(m json.RawMessage, formats FormatLookup, depth int) (Value, error) {
	d := json.NewDecoder(bytes.NewReader(m))
	d.UseNumber()
	tok, err := d.Token()
	if err != nil {
		return nil, err
	}
	return decodeJSONFrom(d, tok, formats, depth+1)
}

func jsonTagged(tag string, body json.RawMessage, params map[string]json.RawMessage, formats FormatLookup, depth int) (Value, error) {
	str := func() (string, error) {
		var s string
		if err := json.Unmarshal(body, &s); err != nil {
			return "", fmt.Errorf("value: %s takes a string", tag)
		}
		return s, nil
	}
	param := func(name string, out any) error {
		m, ok := params[name]
		if !ok {
			return nil
		}
		if err := json.Unmarshal(m, out); err != nil {
			return fmt.Errorf("value: %s: bad %q", tag, name)
		}
		return nil
	}
	list := func() ([]Value, error) {
		d := json.NewDecoder(bytes.NewReader(body))
		d.UseNumber()
		if tok, err := d.Token(); err != nil || tok != json.Delim('[') {
			return nil, fmt.Errorf("value: %s takes an array", tag)
		}
		return jsonArray(d, formats, depth)
	}
	for name := range params {
		allowed := map[string][]string{"$bytes": {"media"}, "$doc": {"format"}, "$fifo": {"limit"}, "$lifo": {"limit"},
			"$ring": {"capacity"}, "$priority": {"low_first", "limit"}, "$blob": {"type", "len", "media"}}[tag]
		if !contains(allowed, name) {
			return nil, fmt.Errorf("value: %s has no detail %q", tag, name)
		}
	}
	switch tag {
	case "$float":
		var s string
		if json.Unmarshal(body, &s) == nil {
			switch s {
			case "nan":
				return NewFloat(math.NaN()), nil
			case "inf":
				return NewFloat(math.Inf(1)), nil
			case "-inf":
				return NewFloat(math.Inf(-1)), nil
			}
			return nil, fmt.Errorf("value: $float %q", s)
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(string(body)), 64)
		if err != nil {
			return nil, fmt.Errorf("value: $float: %v", err)
		}
		return NewFloat(f), nil
	case "$bigint":
		s, err := str()
		if err != nil {
			return nil, err
		}
		b, ok := new(big.Int).SetString(s, 10)
		if !ok {
			return nil, fmt.Errorf("value: $bigint %q", s)
		}
		return NewBigInt(b)
	case "$decimal":
		s, err := str()
		if err != nil {
			return nil, err
		}
		return ParseDecimal(s)
	case "$bytes", "$image":
		s, err := str()
		if err != nil {
			return nil, err
		}
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("value: %s: %v", tag, err)
		}
		if tag == "$image" {
			return newImage(b)
		}
		var media string
		if err := param("media", &media); err != nil {
			return nil, err
		}
		return BytesOf(b, media), nil
	case "$time":
		s, err := str()
		if err != nil {
			return nil, err
		}
		return ParseTime(s)
	case "$date":
		s, err := str()
		if err != nil {
			return nil, err
		}
		return ParseDate(s)
	case "$duration":
		s, err := str()
		if err != nil {
			return nil, err
		}
		dur, err := time.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("value: $duration: %v", err)
		}
		return Duration(dur), nil
	case "$id", "$ref":
		s, err := str()
		if err != nil {
			return nil, err
		}
		id, err := ParseID(s)
		if err != nil {
			return nil, err
		}
		if tag == "$ref" {
			return Ref{Target: id}, nil
		}
		return id, nil
	case "$doc":
		s, err := str()
		if err != nil {
			return nil, err
		}
		var name string
		if err := param("format", &name); err != nil {
			return nil, err
		}
		var f Format
		ok := false
		if formats != nil {
			f, ok = formats(name)
		}
		if !ok {
			return nil, fmt.Errorf("value: $doc in unknown format %q", name)
		}
		return NewDoc(f, s)
	case "$array", "$set", "$pair":
		vs, err := list()
		if err != nil {
			return nil, err
		}
		switch tag {
		case "$array":
			return NewArray(vs...)
		case "$set":
			return NewSet(vs...)
		}
		if len(vs) != 2 {
			return nil, errors.New("value: $pair takes two values")
		}
		return NewPair(vs[0], vs[1])
	case "$map":
		vs, err := list()
		if err != nil {
			return nil, err
		}
		ks, mvs := make([]Value, len(vs)), make([]Value, len(vs))
		for i, e := range vs {
			l, ok := e.(List)
			if !ok || l.Len() != 2 {
				return nil, errors.New("value: $map takes [key, value] pairs")
			}
			ks[i], mvs[i] = l.At(0), l.At(1)
		}
		return NewMap(ks, mvs)
	case "$record":
		d := json.NewDecoder(bytes.NewReader(body))
		d.UseNumber()
		if tok, err := d.Token(); err != nil || tok != json.Delim('{') {
			return nil, errors.New("value: $record takes an object")
		}
		var names []string
		var vs []Value
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			if tok == json.Delim('}') {
				break
			}
			names = append(names, tok.(string))
			tok, err = d.Token()
			if err != nil {
				return nil, err
			}
			v, err := decodeJSONFrom(d, tok, formats, depth+1)
			if err != nil {
				return nil, err
			}
			vs = append(vs, v)
		}
		return NewRecord(names, vs)
	case "$fifo", "$lifo", "$ring":
		items, err := list()
		if err != nil {
			return nil, err
		}
		t := &Type{Code: map[string]Code{"$fifo": CodeFifo, "$lifo": CodeLifo, "$ring": CodeRing}[tag]}
		name := "limit"
		if tag == "$ring" {
			name = "capacity"
		}
		if err := param(name, &t.N); err != nil {
			return nil, err
		}
		return NewQueueOf(t, items, nil)
	case "$priority":
		entries, err := list()
		if err != nil {
			return nil, err
		}
		t := &Type{Code: CodePriority}
		if err := param("low_first", &t.LowFirst); err != nil {
			return nil, err
		}
		if err := param("limit", &t.N); err != nil {
			return nil, err
		}
		items, prios := make([]Value, len(entries)), make([]int64, len(entries))
		for i, e := range entries {
			l, ok := e.(List)
			p, isInt := Value(nil), false
			if ok && l.Len() == 2 {
				p = l.At(0)
				_, isInt = p.(Int)
			}
			if !isInt {
				return nil, errors.New("value: $priority takes [priority, item] pairs")
			}
			prios[i], items[i] = int64(p.(Int)), l.At(1)
		}
		return NewQueueOf(t, items, prios)
	case "$blob":
		s, err := str()
		if err != nil {
			return nil, err
		}
		h, err := hex.DecodeString(s)
		if err != nil || len(h) != 32 {
			return nil, fmt.Errorf("value: $blob %q is not a SHA-256", s)
		}
		r := BlobRef{Hash: [32]byte(h)}
		var typ string
		if err := param("type", &typ); err != nil {
			return nil, err
		}
		switch typ {
		case "text":
			r.Of = CodeText
		case "bytes":
			r.Of = CodeBytes
		case "image":
			r.Of = CodeImage
		default:
			return nil, fmt.Errorf("value: $blob of type %q", typ)
		}
		if err := param("len", &r.Len); err != nil {
			return nil, err
		}
		if err := param("media", &r.Media); err != nil {
			return nil, err
		}
		if r.Len < 0 {
			return nil, errors.New("value: $blob with a negative length")
		}
		return r, nil
	}
	return nil, fmt.Errorf("value: unknown type tag %s", tag)
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
