package value

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Value encoding tags (spec/types.md §12.1).
const (
	tagNull     = 0x00
	tagFalse    = 0x01
	tagTrue     = 0x02
	tagInt      = 0x10
	tagBigInt   = 0x11
	tagFloat    = 0x12
	tagDecimal  = 0x13
	tagText     = 0x20
	tagBytes    = 0x21
	tagTime     = 0x30
	tagDate     = 0x31
	tagDuration = 0x32
	tagID       = 0x33
	tagRef      = 0x34
	tagImage    = 0x40
	tagDoc      = 0x41
	tagList     = 0x50
	tagArray    = 0x51
	tagSet      = 0x52
	tagMap      = 0x53
	tagPair     = 0x54
	tagFifo     = 0x58
	tagLifo     = 0x59
	tagRing     = 0x5A
	tagPriority = 0x5B
	tagRecord   = 0x60
	tagBlobRef  = 0x22 // BlobRef, in stored records only
	// 0x70 (collection reference) is for the storage layer's out-of-line
	// collections; 0x61 is reserved.
)

// canonicalNaN is the bit pattern every NaN is stored as.
var canonicalNaN = math.Float64bits(math.NaN())

// Encode returns the canonical value encoding of v.
func Encode(v Value) []byte { return AppendValue(nil, v) }

// AppendValue appends the canonical value encoding of v to dst. Equal values
// of the same type always encode to the same bytes.
//
// Layout (all integers big-endian; "uvarint" is unsigned LEB128 as in
// encoding/binary, always minimal; "str" is a uvarint length then bytes):
//
//	null, false, true   tag only
//	int                 zigzag uvarint
//	bigint              sign byte (0 or 1), str magnitude (no leading zeros; empty for 0)
//	float               8 bytes IEEE 754 (no -0; one NaN)
//	decimal             uvarint scale, then a bigint payload for the coefficient
//	text                str (valid NFC UTF-8)
//	bytes               str data, str media type
//	time                8 bytes signed seconds since 1970, 4 bytes nanoseconds
//	date                4 bytes signed days since 1970-01-01
//	duration            8 bytes signed nanoseconds
//	id, ref             16 bytes
//	image               str format, uvarint width, uvarint height, str data
//	doc                 str format name, str text
//	list, array, set    uvarint count, elements (set elements in sort order)
//	map                 uvarint count, key-value pairs in key sort order
//	pair                key, value
//	fifo, lifo          uvarint max (0 = unbounded), uvarint count, elements in pop order
//	ring                uvarint capacity, uvarint count, elements in pop order
//	priority            direction byte (0 high, 1 low), uvarint max, uvarint count,
//	                    then (zigzag priority, element) in pop order
//	record              uvarint count, then (str name, value) in declared order
//	blob reference      code byte (text, bytes or image), 32 bytes SHA-256,
//	                    uvarint length, str media type
//
// A value held in an any or union field is encoded as itself: every
// encoding already carries its type.
func AppendValue(dst []byte, v Value) []byte {
	switch x := v.(type) {
	case nil, nullValue:
		return append(dst, tagNull)
	case Bool:
		if x {
			return append(dst, tagTrue)
		}
		return append(dst, tagFalse)
	case Int:
		return binary.AppendUvarint(append(dst, tagInt), zigzag(int64(x)))
	case BigInt:
		return appendBig(append(dst, tagBigInt), x.big())
	case Float:
		return binary.BigEndian.AppendUint64(append(dst, tagFloat), math.Float64bits(x.f))
	case Decimal:
		dst = binary.AppendUvarint(append(dst, tagDecimal), uint64(x.scale))
		return appendBig(dst, x.c())
	case Text:
		return appendStr(append(dst, tagText), x.s)
	case Bytes:
		dst = appendStr(append(dst, tagBytes), string(x.b))
		return appendStr(dst, x.mediaType)
	case Time:
		dst = binary.BigEndian.AppendUint64(append(dst, tagTime), uint64(x.t.Unix()))
		return binary.BigEndian.AppendUint32(dst, uint32(x.t.Nanosecond()))
	case Date:
		return binary.BigEndian.AppendUint32(append(dst, tagDate), uint32(x.days))
	case Duration:
		return binary.BigEndian.AppendUint64(append(dst, tagDuration), uint64(x))
	case ID:
		return append(append(dst, tagID), x[:]...)
	case Ref:
		return append(append(dst, tagRef), x.Target[:]...)
	case BlobRef:
		dst = append(append(dst, tagBlobRef, byte(x.Of)), x.Hash[:]...)
		dst = binary.AppendUvarint(dst, uint64(x.Len))
		return appendStr(dst, x.Media)
	case Image:
		dst = appendStr(append(dst, tagImage), x.format)
		dst = binary.AppendUvarint(dst, uint64(x.width))
		dst = binary.AppendUvarint(dst, uint64(x.height))
		return appendStr(dst, string(x.data))
	case Doc:
		dst = appendStr(append(dst, tagDoc), x.format)
		return appendStr(dst, x.text)
	case List:
		return appendSeq(append(dst, tagList), x.elems)
	case Array:
		return appendSeq(append(dst, tagArray), x.elems)
	case Set:
		return appendSeq(append(dst, tagSet), x.elems)
	case Map:
		dst = binary.AppendUvarint(append(dst, tagMap), uint64(len(x.keys)))
		for i, k := range x.keys {
			dst = AppendValue(AppendValue(dst, k), x.vals[i])
		}
		return dst
	case Pair:
		return AppendValue(AppendValue(append(dst, tagPair), x.Key()), x.Value())
	case Record:
		dst = binary.AppendUvarint(append(dst, tagRecord), uint64(len(x.names)))
		for i, n := range x.names {
			dst = AppendValue(appendStr(dst, n), x.vals[i])
		}
		return dst
	case Queue:
		switch x.code {
		case CodeFifo:
			dst = append(dst, tagFifo)
		case CodeLifo:
			dst = append(dst, tagLifo)
		case CodeRing:
			dst = append(dst, tagRing)
		case CodePriority:
			dir := byte(0)
			if x.lowFirst {
				dir = 1
			}
			dst = append(dst, tagPriority, dir)
		}
		dst = binary.AppendUvarint(dst, uint64(x.limit))
		if x.code != CodePriority {
			return appendSeq(dst, x.items)
		}
		dst = binary.AppendUvarint(dst, uint64(len(x.items)))
		for i, it := range x.items {
			dst = AppendValue(binary.AppendUvarint(dst, zigzag(x.prios[i])), it)
		}
		return dst
	}
	panic(fmt.Sprintf("value: can't encode %T", v))
}

func zigzag(i int64) uint64   { return uint64(i<<1) ^ uint64(i>>63) }
func unzigzag(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }

func appendStr(dst []byte, s string) []byte {
	return append(binary.AppendUvarint(dst, uint64(len(s))), s...)
}

func appendBig(dst []byte, b *big.Int) []byte {
	sign := byte(0)
	if b.Sign() < 0 {
		sign = 1
	}
	mag := new(big.Int).Abs(b).Bytes()
	return append(binary.AppendUvarint(append(dst, sign), uint64(len(mag))), mag...)
}

func appendSeq(dst []byte, elems []Value) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(elems)))
	for _, e := range elems {
		dst = AppendValue(dst, e)
	}
	return dst
}

// Decode decodes a value that fills all of b.
//
// It rejects blob references, which only stored records hold: see
// DecodeStored.
func Decode(b []byte) (Value, error) { return decodeAll(b, false) }

// DecodeStored is Decode for the engine's own records, which may hold
// blob references.
func DecodeStored(b []byte) (Value, error) { return decodeAll(b, true) }

func decodeAll(b []byte, stored bool) (Value, error) {
	d := decoder{b: b, stored: stored}
	v, err := d.value(1)
	if err != nil {
		return nil, fmt.Errorf("decode at byte %d: %w", d.pos, err)
	}
	n := d.pos
	if n != len(b) {
		return nil, fmt.Errorf("decode: %d extra bytes after value", len(b)-n)
	}
	return v, nil
}

// DecodeValue decodes one value from the start of b and returns it with the
// number of bytes read. It accepts only canonical encodings of well-formed
// values, so AppendValue of the result reproduces exactly those bytes.
//
// Docs are checked for valid NFC text but not re-validated against their
// format, which may be a derived format defined in a database's catalog;
// writes validate docs before they are stored.
// DecodeListHead decodes only the first n elements of an encoded list
// (fewer if it is shorter), for reading a record's leading fields without
// decoding the rest.
func DecodeListHead(b []byte, n int) ([]Value, error) {
	d := decoder{b: b}
	tag, err := d.byte()
	if err != nil {
		return nil, err
	}
	if tag != tagList {
		return nil, fmt.Errorf("decode: not a list (tag %#x)", tag)
	}
	count, err := d.count()
	if err != nil {
		return nil, err
	}
	vs, err := d.values(min(n, count), 2)
	if err != nil {
		return nil, fmt.Errorf("decode at byte %d: %w", d.pos, err)
	}
	return vs, nil
}

func DecodeValue(b []byte) (Value, int, error) {
	d := decoder{b: b}
	v, err := d.value(1)
	if err != nil {
		return nil, 0, fmt.Errorf("decode at byte %d: %w", d.pos, err)
	}
	return v, d.pos, nil
}

type decoder struct {
	b      []byte
	pos    int
	stored bool // blob references allowed
}

var errShort = errors.New("unexpected end of data")

func (d *decoder) byte() (byte, error) {
	if d.pos >= len(d.b) {
		return 0, errShort
	}
	d.pos++
	return d.b[d.pos-1], nil
}

func (d *decoder) take(n uint64) ([]byte, error) {
	if n > uint64(len(d.b)-d.pos) {
		return nil, errShort
	}
	s := d.b[d.pos : d.pos+int(n)]
	d.pos += int(n)
	return s, nil
}

func (d *decoder) uvarint() (uint64, error) {
	x, n := binary.Uvarint(d.b[d.pos:])
	switch {
	case n == 0:
		return 0, errShort
	case n < 0:
		return 0, errors.New("varint overflows 64 bits")
	case n > 1 && d.b[d.pos+n-1] == 0:
		return 0, errors.New("varint is not minimal")
	}
	d.pos += n
	return x, nil
}

func (d *decoder) str(limit uint64) ([]byte, error) {
	n, err := d.uvarint()
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, fmt.Errorf("length %d exceeds limit %d", n, limit)
	}
	return d.take(n)
}

// text reads a str that must be valid NFC UTF-8.
func (d *decoder) text(limit uint64) (string, error) {
	b, err := d.str(limit)
	if err != nil {
		return "", err
	}
	s := string(b)
	if !utf8.ValidString(s) || !norm.NFC.IsNormalString(s) {
		return "", errors.New("text is not valid NFC UTF-8")
	}
	return s, nil
}

func (d *decoder) u32() (uint32, error) {
	b, err := d.take(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b), nil
}

func (d *decoder) u64() (uint64, error) {
	b, err := d.take(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(b), nil
}

func (d *decoder) count() (int, error) {
	n, err := d.uvarint()
	if err != nil {
		return 0, err
	}
	if n > MaxCollectionCount {
		return 0, fmt.Errorf("count %d exceeds %d", n, uint64(MaxCollectionCount))
	}
	// Every element takes at least one byte, which bounds allocations.
	if n > uint64(len(d.b)-d.pos) {
		return 0, errShort
	}
	return int(n), nil
}

func (d *decoder) big() (*big.Int, error) {
	sign, err := d.byte()
	if err != nil {
		return nil, err
	}
	mag, err := d.str(MaxBigIntBytes)
	if err != nil {
		return nil, err
	}
	switch {
	case sign > 1:
		return nil, errors.New("bad sign byte")
	case len(mag) > 0 && mag[0] == 0:
		return nil, errors.New("magnitude has leading zeros")
	case sign == 1 && len(mag) == 0:
		return nil, errors.New("negative zero")
	}
	b := new(big.Int).SetBytes(mag)
	if sign == 1 {
		b.Neg(b)
	}
	return b, nil
}

func (d *decoder) values(n, depth int) ([]Value, error) {
	vs := make([]Value, n)
	for i := range vs {
		v, err := d.value(depth)
		if err != nil {
			return nil, err
		}
		vs[i] = v
	}
	return vs, nil
}

func (d *decoder) seq(depth int) ([]Value, error) {
	n, err := d.count()
	if err != nil {
		return nil, err
	}
	return d.values(n, depth)
}

var (
	minDays = must2(NewDate(1, 1, 1)).days
	maxDays = must2(NewDate(9999, 12, 31)).days
)

func must2[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func (d *decoder) value(depth int) (Value, error) {
	if depth > MaxNesting {
		return nil, fmt.Errorf("nested more than %d levels deep", MaxNesting)
	}
	tag, err := d.byte()
	if err != nil {
		return nil, err
	}
	switch tag {
	case tagNull:
		return Null, nil
	case tagFalse:
		return Bool(false), nil
	case tagTrue:
		return Bool(true), nil
	case tagInt:
		u, err := d.uvarint()
		return Int(unzigzag(u)), err
	case tagBigInt:
		b, err := d.big()
		if err != nil {
			return nil, err
		}
		return BigInt{b}, nil
	case tagFloat:
		u, err := d.u64()
		if err != nil {
			return nil, err
		}
		f := math.Float64frombits(u)
		if (math.IsNaN(f) && u != canonicalNaN) || u == 1<<63 {
			return nil, errors.New("float is not canonical")
		}
		return Float{f}, nil
	case tagDecimal:
		scale, err := d.uvarint()
		if err != nil {
			return nil, err
		}
		coef, err := d.big()
		if err != nil {
			return nil, err
		}
		if scale > MaxDecimalDigits {
			return nil, fmt.Errorf("decimal scale %d out of range", scale)
		}
		return NewDecimal(coef, int(scale))
	case tagText:
		s, err := d.text(MaxTextBytes)
		return Text{s}, err
	case tagBytes:
		data, err := d.str(MaxBytesLen)
		if err != nil {
			return nil, err
		}
		mt, err := d.str(MaxTextBytes)
		if err != nil {
			return nil, err
		}
		return Bytes{append([]byte(nil), data...), string(mt)}, nil
	case tagTime:
		secs, err := d.u64()
		if err != nil {
			return nil, err
		}
		nanos, err := d.u32()
		if err != nil {
			return nil, err
		}
		if nanos >= 1e9 {
			return nil, errors.New("time nanoseconds out of range")
		}
		t, err := NewTime(time.Unix(int64(secs), int64(nanos)))
		return t, err
	case tagDate:
		u, err := d.u32()
		if err != nil {
			return nil, err
		}
		days := int32(u)
		if days < minDays || days > maxDays {
			return nil, errors.New("date outside years 0001-9999")
		}
		return Date{days}, nil
	case tagDuration:
		u, err := d.u64()
		return Duration(int64(u)), err
	case tagID, tagRef:
		b, err := d.take(16)
		if err != nil {
			return nil, err
		}
		id := ID(b)
		if tag == tagRef {
			return Ref{id}, nil
		}
		return id, nil
	case tagImage:
		return d.image()
	case tagBlobRef:
		if !d.stored {
			return nil, errors.New("blob reference outside a stored record")
		}
		return d.blobRef()
	case tagDoc:
		format, err := d.text(MaxNameLen * utf8.UTFMax)
		if err != nil {
			return nil, err
		}
		if err := ValidateName("format", format); err != nil {
			return nil, err
		}
		text, err := d.text(MaxTextBytes)
		if err != nil {
			return nil, err
		}
		return Doc{format, text}, nil
	case tagList:
		vs, err := d.seq(depth + 1)
		if err != nil {
			return nil, err
		}
		return NewList(vs...)
	case tagArray:
		vs, err := d.seq(depth + 1)
		if err != nil {
			return nil, err
		}
		return NewArray(vs...)
	case tagSet:
		vs, err := d.seq(depth + 1)
		if err != nil {
			return nil, err
		}
		if err := checkAscending(vs); err != nil {
			return nil, err
		}
		return NewSet(vs...)
	case tagMap:
		n, err := d.count()
		if err != nil {
			return nil, err
		}
		kv, err := d.values(2*n, depth+1)
		if err != nil {
			return nil, err
		}
		keys, vals := make([]Value, n), make([]Value, n)
		for i := range n {
			keys[i], vals[i] = kv[2*i], kv[2*i+1]
		}
		if err := checkAscending(keys); err != nil {
			return nil, err
		}
		return NewMap(keys, vals)
	case tagPair:
		kv, err := d.values(2, depth+1)
		if err != nil {
			return nil, err
		}
		return NewPair(kv[0], kv[1])
	case tagRecord:
		n, err := d.count()
		if err != nil {
			return nil, err
		}
		names, vals := make([]string, n), make([]Value, n)
		for i := range n {
			if names[i], err = d.text(MaxNameLen * utf8.UTFMax); err != nil {
				return nil, err
			}
			if vals[i], err = d.value(depth + 1); err != nil {
				return nil, err
			}
		}
		return NewRecord(names, vals)
	case tagFifo, tagLifo, tagRing, tagPriority:
		return d.queue(tag, depth)
	}
	return nil, fmt.Errorf("unknown tag 0x%02x", tag)
}

// checkAscending requires set elements or map keys to be strictly
// increasing, as canonical encodings write them.
func checkAscending(vs []Value) error {
	for i := 1; i < len(vs); i++ {
		if Compare(vs[i-1], vs[i]) >= 0 {
			return errors.New("elements are not in strictly increasing order")
		}
	}
	return nil
}

func (d *decoder) image() (Value, error) {
	format, err := d.text(16)
	if err != nil {
		return nil, err
	}
	w, err := d.uvarint()
	if err != nil {
		return nil, err
	}
	h, err := d.uvarint()
	if err != nil {
		return nil, err
	}
	data, err := d.str(MaxBytesLen)
	if err != nil {
		return nil, err
	}
	img, err := NewImage(data)
	if err != nil {
		return nil, err
	}
	if img.format != format || uint64(img.width) != w || uint64(img.height) != h {
		return nil, errors.New("image header doesn't match its data")
	}
	return img, nil
}

func (d *decoder) queue(tag byte, depth int) (Value, error) {
	t := &Type{Elem: TypeAny}
	switch tag {
	case tagFifo:
		t.Code = CodeFifo
	case tagLifo:
		t.Code = CodeLifo
	case tagRing:
		t.Code = CodeRing
	case tagPriority:
		t.Code = CodePriority
		dir, err := d.byte()
		if err != nil {
			return nil, err
		}
		if dir > 1 {
			return nil, errors.New("bad priority direction")
		}
		t.LowFirst = dir == 1
	}
	limit, err := d.uvarint()
	if err != nil {
		return nil, err
	}
	if limit > math.MaxInt32 {
		return nil, errors.New("queue limit out of range")
	}
	t.N = int(limit)
	n, err := d.count()
	if err != nil {
		return nil, err
	}
	if t.Code != CodePriority {
		items, err := d.values(n, depth+1)
		if err != nil {
			return nil, err
		}
		return NewQueueOf(t, items, nil)
	}
	items, prios := make([]Value, n), make([]int64, n)
	for i := range n {
		u, err := d.uvarint()
		if err != nil {
			return nil, err
		}
		prios[i] = unzigzag(u)
		if items[i], err = d.value(depth + 1); err != nil {
			return nil, err
		}
	}
	q, err := NewQueueOf(t, items, prios)
	if err != nil {
		return nil, err
	}
	// NewQueueOf sorts stably; the input must already have been in pop order.
	for i := 1; i < n; i++ {
		if q.cmpPrio(prios[i-1], prios[i]) > 0 {
			return nil, errors.New("priority queue elements are not in pop order")
		}
	}
	return q, nil
}

func (d *decoder) blobRef() (Value, error) {
	of, err := d.byte()
	if err != nil {
		return nil, err
	}
	if c := Code(of); c != CodeText && c != CodeBytes && c != CodeImage {
		return nil, fmt.Errorf("blob reference to a %s", c)
	}
	h, err := d.take(32)
	if err != nil {
		return nil, err
	}
	n, err := d.uvarint()
	if err != nil {
		return nil, err
	}
	if n > MaxBytesLen {
		return nil, fmt.Errorf("blob reference of %d bytes", n)
	}
	mt, err := d.str(MaxTextBytes)
	if err != nil {
		return nil, err
	}
	r := BlobRef{Of: Code(of), Len: int64(n), Media: string(mt)}
	copy(r.Hash[:], h)
	return r, nil
}
