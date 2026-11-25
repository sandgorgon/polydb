package transfer

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/sandgorgon/polydb/value"
)

// An export is a sequence of records, each a map with text keys, one of
// which (recordKinds) says what it is. JSON Lines writes each as one line in the
// values' JSON form (value.AppendJSON); the native format writes a magic
// line, then each record as a length and its binary value encoding
// (spec/types.md §12.1). Both stream: large values are written in
// pieces (blob and data records), never whole.

// Format is an export's file format.
type Format string

const (
	JSONL  Format = "jsonl"
	Native Format = "native"
)

// nativeMagic starts a native export.
const nativeMagic = "POLYDB-EXPORT-1\n"

// maxRecord bounds a record's size when reading: values inline are
// bounded by the 64 MiB message size, large values come in pieces.
const maxRecord = 256 << 20

// pair is one key and value of a record; a nil value leaves the key out.
type pair struct {
	k string
	v value.Value
}

// rec builds a record.
func rec(ps ...pair) value.Map {
	var ks, vs []value.Value
	for _, p := range ps {
		if p.v == nil {
			continue
		}
		ks, vs = append(ks, value.MustText(p.k)), append(vs, p.v)
	}
	m, err := value.NewMap(ks, vs)
	if err != nil {
		panic(err) // keys are distinct constants
	}
	return m
}

type recordWriter struct {
	w      *bufio.Writer
	format Format
	buf    []byte
}

func newRecordWriter(w io.Writer, format Format) (*recordWriter, error) {
	rw := &recordWriter{w: bufio.NewWriterSize(w, 1<<20), format: format}
	switch format {
	case JSONL:
	case Native:
		if _, err := rw.w.WriteString(nativeMagic); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown export format %q (jsonl or native)", format)
	}
	return rw, nil
}

func (rw *recordWriter) write(m value.Map) error {
	var err error
	switch rw.format {
	case JSONL:
		if rw.buf, err = value.AppendJSON(rw.buf[:0], m); err != nil {
			return err
		}
		rw.buf = append(rw.buf, '\n')
	default:
		body := value.AppendValue(nil, m)
		rw.buf = append(binary.AppendUvarint(rw.buf[:0], uint64(len(body))), body...)
	}
	_, err = rw.w.Write(rw.buf)
	if cap(rw.buf) > 4<<20 {
		rw.buf = nil
	}
	return err
}

func (rw *recordWriter) flush() error { return rw.w.Flush() }

// recordReader reads an export's records, in either format.
type recordReader struct {
	r       *bufio.Reader
	native  bool
	dec     *json.Decoder
	formats value.FormatLookup
}

func newRecordReader(r io.Reader) (*recordReader, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	rr := &recordReader{r: br}
	head, err := br.Peek(len(nativeMagic))
	if err == nil && string(head) == nativeMagic {
		br.Discard(len(nativeMagic))
		rr.native = true
		return rr, nil
	}
	rr.dec = json.NewDecoder(br)
	rr.dec.UseNumber()
	return rr, nil
}

// next returns the next record, or io.EOF after the last.
func (rr *recordReader) next() (value.Map, error) {
	var v value.Value
	if rr.native {
		n, err := binary.ReadUvarint(rr.r)
		if err == io.EOF {
			return value.Map{}, io.EOF
		}
		if err != nil {
			return value.Map{}, err
		}
		if n > maxRecord {
			return value.Map{}, fmt.Errorf("a record of %d bytes is too large", n)
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(rr.r, body); err != nil {
			return value.Map{}, unexpected(err)
		}
		if v, err = value.DecodeStored(body); err != nil {
			return value.Map{}, err
		}
	} else {
		var err error
		if v, err = value.DecodeJSON(rr.dec, rr.formats); err != nil {
			if err == io.EOF {
				return value.Map{}, io.EOF
			}
			return value.Map{}, err
		}
	}
	m, ok := v.(value.Map)
	if !ok {
		return value.Map{}, errors.New("a record isn't an object")
	}
	if kindOf(m) == "" {
		return value.Map{}, fmt.Errorf("a record of no known kind: %.80s", m)
	}
	return m, nil
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// recordKinds are the keys that say what a record is; a record has one.
var recordKinds = []string{"polydb", "define", "blob", "data", "entity", "relation", "user", "role", "grant", "end"}

// kindOf returns what a record is, or "" if it has no kind key.
func kindOf(m value.Map) string {
	for _, k := range recordKinds {
		if _, ok := m.Get(value.MustText(k)); ok {
			return k
		}
	}
	return ""
}

// field reads a record's key, nil if absent.
func field(m value.Map, k string) value.Value {
	v, ok := m.Get(value.MustText(k))
	if !ok || value.IsNull(v) {
		return nil
	}
	return v
}

func textField(m value.Map, k string) (string, error) {
	v := field(m, k)
	if v == nil {
		return "", nil
	}
	t, ok := v.(value.Text)
	if !ok {
		return "", fmt.Errorf("%s is %s, not text", k, v.Code())
	}
	return t.Go(), nil
}

func idField(m value.Map, k string) (value.ID, error) {
	id, ok := field(m, k).(value.ID)
	if !ok {
		return value.ID{}, fmt.Errorf("%s isn't an id", k)
	}
	return id, nil
}

func intField(m value.Map, k string) (int64, error) {
	v := field(m, k)
	if v == nil {
		return 0, nil
	}
	i, ok := v.(value.Int)
	if !ok {
		return 0, fmt.Errorf("%s isn't an int", k)
	}
	return int64(i), nil
}

func mapField(m value.Map, k string) (value.Map, error) {
	v := field(m, k)
	if v == nil {
		return value.Map{}, nil
	}
	mm, ok := v.(value.Map)
	if !ok {
		return value.Map{}, fmt.Errorf("%s isn't a map", k)
	}
	return mm, nil
}
