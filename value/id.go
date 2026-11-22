package value

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"sync"
	"time"
)

// ID is a 128-bit PolyDB identifier (spec/types.md §4.11). Its layout,
// big-endian:
//
//	48 bits  milliseconds since 1970-01-01 UTC
//	16 bits  node ID (0 on a single machine)
//	16 bits  per-millisecond counter
//	48 bits  random
//
// IDs from one generator always increase, so they sort in creation order.
type ID [16]byte

func (ID) Code() Code { return CodeID }

// IsZero reports whether id is the zero ID, which is never generated.
func (id ID) IsZero() bool { return id == ID{} }

// Time returns the millisecond timestamp embedded in id.
func (id ID) Time() time.Time {
	ms := int64(id[0])<<40 | int64(id[1])<<32 | int64(id[2])<<24 |
		int64(id[3])<<16 | int64(id[4])<<8 | int64(id[5])
	return time.UnixMilli(ms).UTC()
}

// Node returns the node ID embedded in id.
func (id ID) Node() uint16 { return binary.BigEndian.Uint16(id[6:8]) }

// String returns "#" followed by the 26-character Crockford base32 form,
// which is the Rhizome literal syntax for an ID.
func (id ID) String() string { return "#" + id.Base32() }

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Base32 returns the 26-character Crockford base32 form of id.
func (id ID) Base32() string {
	// 128 bits are encoded as 130 bits (two leading zero bits, so the first
	// character is at most 7), 5 bits per character, most significant first.
	var out [26]byte
	hi := binary.BigEndian.Uint64(id[0:8])
	lo := binary.BigEndian.Uint64(id[8:16])
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}

var crockfordDecode = func() [256]int8 {
	var t [256]int8
	for i := range t {
		t[i] = -1
	}
	for i, c := range crockford {
		t[c] = int8(i)
		if c >= 'A' && c <= 'Z' {
			t[c+'a'-'A'] = int8(i)
		}
	}
	// Crockford's aliases for easily confused characters.
	for _, c := range "oO" {
		t[c] = 0
	}
	for _, c := range "iIlL" {
		t[c] = 1
	}
	return t
}()

// ParseID parses the 26-character base32 form of an ID, with or without a
// leading "#". Lowercase and Crockford's aliases (O for 0, I and L for 1)
// are accepted.
func ParseID(s string) (ID, error) {
	if len(s) > 0 && s[0] == '#' {
		s = s[1:]
	}
	if len(s) != 26 {
		return ID{}, fmt.Errorf("invalid id %q: want 26 base32 characters", s)
	}
	var hi, lo uint64
	for i := 0; i < 26; i++ {
		v := crockfordDecode[s[i]]
		if v < 0 {
			return ID{}, fmt.Errorf("invalid id %q: bad character %q", s, s[i])
		}
		if i == 0 && v > 7 { // 26×5 = 130 bits; the first character holds the top 3 of 128
			return ID{}, fmt.Errorf("invalid id %q: value too large", s)
		}
		hi = hi<<5 | lo>>59
		lo = lo<<5 | uint64(v)
	}
	var id ID
	binary.BigEndian.PutUint64(id[0:8], hi)
	binary.BigEndian.PutUint64(id[8:16], lo)
	return id, nil
}

// IDGenerator creates IDs for one node. It is safe for concurrent use.
type IDGenerator struct {
	node uint16
	now  func() time.Time

	mu      sync.Mutex
	lastMS  uint64
	counter uint16
}

// NewIDGenerator returns a generator for the given node ID (0 on a single
// machine).
func NewIDGenerator(node uint16) *IDGenerator {
	return &IDGenerator{node: node, now: time.Now}
}

// New returns a new ID, greater than every ID this generator returned
// before. If the clock goes backwards, or more than 65,536 IDs are made in
// one millisecond, the timestamp is carried forward so ordering holds.
func (g *IDGenerator) New() ID {
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		panic("polydb: reading random bytes: " + err.Error()) // crypto/rand never fails on supported platforms
	}

	g.mu.Lock()
	ms := uint64(g.now().UnixMilli())
	switch {
	case ms > g.lastMS:
		g.lastMS = ms
		g.counter = 0
	case g.counter == 0xFFFF:
		g.lastMS++
		g.counter = 0
	default:
		g.counter++
	}
	ms, counter := g.lastMS, g.counter
	g.mu.Unlock()

	var id ID
	id[0] = byte(ms >> 40)
	id[1] = byte(ms >> 32)
	id[2] = byte(ms >> 24)
	id[3] = byte(ms >> 16)
	id[4] = byte(ms >> 8)
	id[5] = byte(ms)
	binary.BigEndian.PutUint16(id[6:8], g.node)
	binary.BigEndian.PutUint16(id[8:10], counter)
	copy(id[10:], rnd[:])
	return id
}
