package polydb

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"time"

	"github.com/sandgorgon/polydb/value"
)

// ToValue converts a Go value to a PolyDB value: nil, bool, integers,
// floats, string, []byte, time.Time, time.Duration, *big.Int, value.Value
// itself, slices (lists), and maps with string keys.
func ToValue(v any) (value.Value, error) {
	switch x := v.(type) {
	case nil:
		return value.Null, nil
	case value.Value:
		return x, nil
	case bool:
		return value.Bool(x), nil
	case int:
		return value.Int(x), nil
	case int8:
		return value.Int(x), nil
	case int16:
		return value.Int(x), nil
	case int32:
		return value.Int(x), nil
	case int64:
		return value.Int(x), nil
	case uint8:
		return value.Int(x), nil
	case uint16:
		return value.Int(x), nil
	case uint32:
		return value.Int(x), nil
	case uint:
		if x > math.MaxInt64 {
			return value.NewBigInt(new(big.Int).SetUint64(uint64(x)))
		}
		return value.Int(x), nil
	case uint64:
		if x > math.MaxInt64 {
			return value.NewBigInt(new(big.Int).SetUint64(x))
		}
		return value.Int(x), nil
	case float32:
		return value.NewFloat(float64(x)), nil
	case float64:
		return value.NewFloat(x), nil
	case string:
		return value.NewText(x)
	case []byte:
		return value.NewBytes(x, "")
	case time.Time:
		return value.NewTime(x)
	case time.Duration:
		return value.Duration(x), nil
	case *big.Int:
		return value.NewBigInt(x)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		vs := make([]value.Value, rv.Len())
		for i := range vs {
			var err error
			if vs[i], err = ToValue(rv.Index(i).Interface()); err != nil {
				return nil, err
			}
		}
		return value.NewList(vs...)
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			break
		}
		m := map[string]value.Value{}
		for _, k := range rv.MapKeys() {
			e, err := ToValue(rv.MapIndex(k).Interface())
			if err != nil {
				return nil, err
			}
			m[k.String()] = e
		}
		return value.MapOfText(m)
	case reflect.Pointer:
		if rv.IsNil() {
			return value.Null, nil
		}
		return ToValue(rv.Elem().Interface())
	}
	return nil, fmt.Errorf("polydb: can't convert %T to a PolyDB value", v)
}

// ParamValues converts parameters to values.
//
// A *Blob is read whole, unless an embedded database returned it; see
// SplitParams for blobs a server holds.
func ParamValues(p Params) (map[string]value.Value, error) {
	if len(p) == 0 {
		return nil, nil
	}
	out := make(map[string]value.Value, len(p))
	for k, v := range p {
		if b, ok := v.(*Blob); ok {
			if b.lazy != nil {
				out[k] = *b.lazy
				continue
			}
			c, err := b.Value(context.Background())
			if err != nil {
				return nil, fmt.Errorf("parameter $%s: %w", k, err)
			}
			out[k] = c
			continue
		}
		c, err := ToValue(v)
		if err != nil {
			return nil, fmt.Errorf("parameter $%s: %w", k, err)
		}
		out[k] = c
	}
	return out, nil
}

// SplitParams is ParamValues for a client: blobs with a lease token
// (read from results, or uploaded) are returned as tokens instead, so
// their content isn't sent again.
func SplitParams(p Params) (vals map[string]value.Value, tokens map[string][]byte, err error) {
	rest := Params{}
	for k, v := range p {
		if b, ok := v.(*Blob); ok && b.token != nil {
			if tokens == nil {
				tokens = map[string][]byte{}
			}
			tokens[k] = b.token
			continue
		}
		rest[k] = v
	}
	vals, err = ParamValues(rest)
	return vals, tokens, err
}

// Scan copies cells into dest, converting as needed:
//
//   - *any, *value.Value: the cell as is (a *Blob stays a *Blob for *any)
//   - *string: text (and other values as their literal form)
//   - *int, *int64: int; *float64: float or int; *bool: bool
//   - *[]byte: bytes, text or image data
//   - *time.Time: time or date; *time.Duration: duration
//   - *value.ID: id or ref; **Entity, **Relation, **Path, **Blob: those
//   - a pointer to a pointer (**string, **int64, …): nil for null
//
// Blob references are fetched with ctx when scanned into anything but
// *any or **Blob.
func Scan(ctx context.Context, cells []any, dest ...any) error {
	if len(dest) != len(cells) {
		return fmt.Errorf("polydb: Scan got %d destinations for %d columns", len(dest), len(cells))
	}
	for i, d := range dest {
		if err := scanOne(ctx, cells[i], d); err != nil {
			return fmt.Errorf("polydb: column %d: %w", i, err)
		}
	}
	return nil
}

func isNull(c any) bool {
	if c == nil {
		return true
	}
	v, ok := c.(value.Value)
	return ok && value.IsNull(v)
}

func scanOne(ctx context.Context, cell any, dest any) error {
	switch d := dest.(type) {
	case *any:
		*d = cell
		return nil
	case **Entity:
		e, ok := cell.(*Entity)
		if !ok && !isNull(cell) {
			return mismatch(cell, "an entity")
		}
		*d = e
		return nil
	case **Relation:
		r, ok := cell.(*Relation)
		if !ok && !isNull(cell) {
			return mismatch(cell, "a relation")
		}
		*d = r
		return nil
	case **Path:
		p, ok := cell.(*Path)
		if !ok && !isNull(cell) {
			return mismatch(cell, "a path")
		}
		*d = p
		return nil
	case **Blob:
		switch c := cell.(type) {
		case *Blob:
			*d = c
		default:
			if isNull(cell) {
				*d = nil
				return nil
			}
			b, err := inlineBlob(cell)
			if err != nil {
				return err
			}
			*d = b
		}
		return nil
	}
	// Fetch blobs for everything else.
	if b, ok := cell.(*Blob); ok {
		v, err := b.Value(ctx)
		if err != nil {
			return err
		}
		cell = v
	}
	// Pointers to pointers take null as nil.
	rv := reflect.ValueOf(dest)
	if rv.Kind() == reflect.Pointer && rv.Elem().Kind() == reflect.Pointer {
		if isNull(cell) {
			rv.Elem().Set(reflect.Zero(rv.Elem().Type()))
			return nil
		}
		target := reflect.New(rv.Elem().Type().Elem())
		if err := scanOne(ctx, cell, target.Interface()); err != nil {
			return err
		}
		rv.Elem().Set(target)
		return nil
	}
	v, ok := cell.(value.Value)
	if !ok {
		return mismatch(cell, fmt.Sprintf("%T", dest))
	}
	if value.IsNull(v) {
		if dv, ok := dest.(*value.Value); ok {
			*dv = v
			return nil
		}
		return fmt.Errorf("null can't be scanned into %T; use a pointer to a pointer", dest)
	}
	switch d := dest.(type) {
	case *value.Value:
		*d = v
	case *string:
		if t, ok := v.(value.Text); ok {
			*d = t.Go()
		} else {
			*d = v.String()
		}
	case *int64:
		n, ok := v.(value.Int)
		if !ok {
			return mismatch(v, "an int")
		}
		*d = int64(n)
	case *int:
		n, ok := v.(value.Int)
		if !ok {
			return mismatch(v, "an int")
		}
		*d = int(n)
	case *float64:
		switch x := v.(type) {
		case value.Float:
			*d = x.Go()
		case value.Int:
			*d = float64(x)
		default:
			return mismatch(v, "a float")
		}
	case *bool:
		b, ok := v.(value.Bool)
		if !ok {
			return mismatch(v, "a bool")
		}
		*d = bool(b)
	case *[]byte:
		switch x := v.(type) {
		case value.Bytes:
			*d = x.Go()
		case value.Text:
			*d = []byte(x.Go())
		case value.Image:
			*d = x.Data()
		default:
			return mismatch(v, "bytes")
		}
	case *time.Time:
		switch x := v.(type) {
		case value.Time:
			*d = x.Go()
		case value.Date:
			*d = x.Go()
		default:
			return mismatch(v, "a time")
		}
	case *time.Duration:
		x, ok := v.(value.Duration)
		if !ok {
			return mismatch(v, "a duration")
		}
		*d = x.Go()
	case *value.ID:
		switch x := v.(type) {
		case value.ID:
			*d = x
		case value.Ref:
			*d = x.Target
		default:
			return mismatch(v, "an id")
		}
	default:
		return fmt.Errorf("can't scan into %T", dest)
	}
	return nil
}

func mismatch(cell any, want string) error {
	got := fmt.Sprintf("%T", cell)
	if v, ok := cell.(value.Value); ok {
		got = v.Code().String()
	}
	return fmt.Errorf("can't scan %s into %s", got, want)
}

// inlineBlob wraps a value sent inline as a Blob, so **Blob works for
// small values too.
func inlineBlob(cell any) (*Blob, error) {
	var data []byte
	var typ, media string
	switch x := cell.(type) {
	case value.Text:
		data, typ, media = []byte(x.Go()), "text", "text/plain; charset=utf-8"
	case value.Bytes:
		data, typ, media = x.Go(), "bytes", x.MediaType()
	case value.Image:
		data, typ, media = x.Data(), "image", "image/"+x.Format()
	default:
		return nil, mismatch(cell, "a blob")
	}
	return NewBlob(typ, int64(len(data)), media, func(_ context.Context, off, n int64) ([]byte, error) {
		end := min(off+n, int64(len(data)))
		if off > end {
			return nil, nil
		}
		return data[off:end], nil
	}), nil
}
