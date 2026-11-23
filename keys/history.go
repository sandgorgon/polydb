package keys

import (
	"encoding/binary"
	"time"

	"github.com/sandgorgon/polydb/value"
)

// Opt-in field history (spec/rhizome.md §10):
//
//	0x40 history  0x40 entity len name time    a field's value from time on
//	                                           (len: uvarint length of name;
//	                                           time: Unix seconds with the
//	                                           sign bit flipped, then
//	                                           nanoseconds, so keys sort by
//	                                           time across years 1–9999)
//	0x41 histkind 0x41 kind entity             entities of a kind with
//	                                           history, deleted ones included
//	0x42 life     0x42 entity                  such an entity's kind, and when
//	                                           it was created and deleted
//
// The time is the writing transaction's time. A field's versions are
// contiguous, oldest first.
const (
	PrefixHistKind = 0x41
	PrefixLife     = 0x42
)

// HistoryEntity returns the prefix of all of an entity's versions.
func HistoryEntity(entity value.ID) []byte { return key(PrefixHistory, entity) }

// HistoryPrefix returns the prefix of a field's versions.
func HistoryPrefix(entity value.ID, name string) []byte {
	k := binary.AppendUvarint(key(PrefixHistory, entity), uint64(len(name)))
	return append(k, name...)
}

// History returns the key of a field's version that starts at t.
func History(entity value.ID, name string, t value.Time) []byte {
	g := t.Go()
	k := binary.BigEndian.AppendUint64(HistoryPrefix(entity, name), uint64(g.Unix())^1<<63)
	return binary.BigEndian.AppendUint32(k, uint32(g.Nanosecond()))
}

// HistoryTime returns the time in a version key.
func HistoryTime(k []byte) (value.Time, error) {
	if len(k) < 1+idLen+1+12 || k[0] != PrefixHistory {
		return value.Time{}, badKey("history", k)
	}
	sec := int64(binary.BigEndian.Uint64(k[len(k)-12:]) ^ 1<<63)
	ns := int64(binary.BigEndian.Uint32(k[len(k)-4:]))
	t, err := value.NewTime(time.Unix(sec, ns))
	if err != nil {
		return value.Time{}, badKey("history", k)
	}
	return t, nil
}

// HistKind returns the key recording that entity, of a kind with history,
// exists or existed.
func HistKind(kind, entity value.ID) []byte { return key(PrefixHistKind, kind, entity) }

// HistKinds returns the prefix of a kind's entities, deleted ones included.
func HistKinds(kind value.ID) []byte { return key(PrefixHistKind, kind) }

// Life returns the key of an entity's life record.
func Life(entity value.ID) []byte { return key(PrefixLife, entity) }

// EncodeVersion encodes a field version; nil is an absent value.
func EncodeVersion(v value.Value) []byte {
	if v == nil {
		return list(value.Bool(false), value.Null)
	}
	return list(value.Bool(true), v)
}

// DecodeVersion decodes a field version; nil is an absent value.
func DecodeVersion(b []byte) (value.Value, error) {
	vs, err := fields(b, "version", 2)
	if err != nil {
		return nil, err
	}
	r := &reader{what: "version", vs: vs}
	present := get[value.Bool](r)
	v := r.next()
	if r.err != nil {
		return nil, r.err
	}
	if !present {
		return nil, nil
	}
	return v, nil
}

// LifeRecord is when an entity of a kind with history was created and, if
// it was, deleted.
type LifeRecord struct {
	Kind    value.ID
	Created value.Time
	Deleted *value.Time
}

// EncodeLife encodes a life record.
func EncodeLife(l LifeRecord) []byte {
	var del value.Value = value.Null
	if l.Deleted != nil {
		del = *l.Deleted
	}
	return list(l.Kind, l.Created, del)
}

// DecodeLife decodes a life record.
func DecodeLife(b []byte) (LifeRecord, error) {
	vs, err := fields(b, "life", 3)
	if err != nil {
		return LifeRecord{}, err
	}
	r := &reader{what: "life", vs: vs}
	l := LifeRecord{Kind: get[value.ID](r), Created: get[value.Time](r)}
	if d := r.next(); !value.IsNull(d) {
		t, ok := d.(value.Time)
		if !ok && r.err == nil {
			r.err = badKey("life", b)
		}
		l.Deleted = &t
	}
	return l, r.err
}
