package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/sandgorgon/polydb/value"
)

// Message types (spec/protocol.md §6.1).
const (
	TypeHello          = 0x01
	TypeHelloOK        = 0x02
	TypeAuth           = 0x03
	TypeAuthContinue   = 0x04
	TypeAuthOK         = 0x05
	TypePing           = 0x06
	TypePong           = 0x07
	TypeGoAway         = 0x08
	TypeWindowUpdate   = 0x09
	TypeConnError      = 0x0A
	TypeQuery          = 0x20
	TypePrepare        = 0x21
	TypeExecute        = 0x22
	TypeCloseStatement = 0x23
	TypeBlobRead       = 0x24
	TypeCancel         = 0x25
	TypeResultHeader   = 0x40
	TypeRows           = 0x41
	TypeResultEnd      = 0x42
	TypeError          = 0x43
	TypePrepared       = 0x44
	TypeBlobData       = 0x45
	TypeNotice         = 0x46
)

// Message is a protocol message.
type Message interface {
	Type() byte
	encode(b *body)
	decode(b *body) error
}

// body holds a message's numbered fields (spec/protocol.md §4.1).
type body struct {
	fields map[uint64]any
	order  []uint64
}

func (b *body) set(n uint64, v any) {
	if b.fields == nil {
		b.fields = map[uint64]any{}
	}
	if _, ok := b.fields[n]; !ok {
		b.order = append(b.order, n)
	}
	b.fields[n] = v
}

// Encode returns a message's body.
func Encode(m Message) []byte {
	b := &body{}
	m.encode(b)
	slices.Sort(b.order)
	out := binary.AppendUvarint(nil, uint64(len(b.order)))
	for _, n := range b.order {
		out = binary.AppendUvarint(out, n)
		out = AppendAny(out, b.fields[n])
	}
	return out
}

// Decode decodes a message body of the given type. Unknown field numbers
// are ignored; an unknown message type is an error.
func Decode(typ byte, data []byte) (Message, error) {
	m := newMessage(typ)
	if m == nil {
		return nil, fmt.Errorf("wire: unknown message type 0x%02x", typ)
	}
	b := &body{fields: map[uint64]any{}}
	n, k := binary.Uvarint(data)
	if k <= 0 || n > uint64(len(data)) {
		return nil, protocolErr("bad field count")
	}
	i := k
	for range n {
		num, k := binary.Uvarint(data[i:])
		if k <= 0 {
			return nil, protocolErr("bad field number")
		}
		i += k
		v, k2, err := DecodeAny(data[i:])
		if err != nil {
			return nil, protocolErr("field %d: %v", num, err)
		}
		i += k2
		if _, dup := b.fields[num]; dup {
			return nil, protocolErr("field %d appears twice", num)
		}
		b.fields[num] = v
	}
	if i != len(data) {
		return nil, protocolErr("%d bytes after the last field", len(data)-i)
	}
	if err := m.decode(b); err != nil {
		return nil, protocolErr("%T: %v", m, err)
	}
	return m, nil
}

func newMessage(typ byte) Message {
	switch typ {
	case TypeHello:
		return &Hello{}
	case TypeHelloOK:
		return &HelloOK{}
	case TypeAuth:
		return &Auth{}
	case TypeAuthContinue:
		return &AuthContinue{}
	case TypeAuthOK:
		return &AuthOK{}
	case TypePing:
		return &Ping{}
	case TypePong:
		return &Pong{}
	case TypeGoAway:
		return &GoAway{}
	case TypeWindowUpdate:
		return &WindowUpdate{}
	case TypeConnError:
		return &ConnErrorMsg{}
	case TypeQuery:
		return &Query{}
	case TypePrepare:
		return &Prepare{}
	case TypeExecute:
		return &Execute{}
	case TypeCloseStatement:
		return &CloseStatement{}
	case TypeBlobRead:
		return &BlobRead{}
	case TypeCancel:
		return &Cancel{}
	case TypeResultHeader:
		return &ResultHeader{}
	case TypeRows:
		return &Rows{}
	case TypeResultEnd:
		return &ResultEnd{}
	case TypeError:
		return &Error{}
	case TypePrepared:
		return &Prepared{}
	case TypeBlobData:
		return &BlobData{}
	case TypeNotice:
		return &Notice{}
	}
	return nil
}

// --- field helpers ---

var errFieldType = errors.New("field has the wrong type")

// get reads field n into out if it is present and not null; out keeps its
// value otherwise.
func get[T any](b *body, n uint64, out *T) error {
	_, err := lookup(b, n, out)
	return err
}

// lookup is get that also reports whether the field was set.
func lookup[T any](b *body, n uint64, out *T) (bool, error) {
	v, ok := b.fields[n]
	if !ok {
		return false, nil
	}
	if vv, isValue := v.(value.Value); isValue && value.IsNull(vv) {
		return false, nil // optional fields may be null
	}
	t, ok := v.(T)
	if !ok {
		return false, fmt.Errorf("field %d: %w", n, errFieldType)
	}
	*out = t
	return true, nil
}

func text(s string) value.Value { return value.MustText(s) }

func textOrNull(s string) value.Value {
	if s == "" {
		return value.Null
	}
	return value.MustText(s)
}

func getText(b *body, n uint64, out *string) error {
	var t value.Text
	ok, err := lookup(b, n, &t)
	if ok {
		*out = t.Go()
	}
	return err
}

func getInt(b *body, n uint64, out *int64) error {
	var i value.Int
	ok, err := lookup(b, n, &i)
	if ok {
		*out = int64(i)
	}
	return err
}

func getBool(b *body, n uint64, out *bool) error {
	var v value.Bool
	ok, err := lookup(b, n, &v)
	if ok {
		*out = bool(v)
	}
	return err
}

func getBytes(b *body, n uint64, out *[]byte) error {
	var v value.Bytes
	ok, err := lookup(b, n, &v)
	if ok {
		*out = v.Go()
	}
	return err
}

func bytesVal(p []byte) value.Value {
	v, err := value.NewBytes(p, "")
	if err != nil {
		panic(err)
	}
	return v
}

func textSet(ss []string) value.Value {
	vs := make([]value.Value, len(ss))
	for i, s := range ss {
		vs[i] = text(s)
	}
	s, err := value.NewSet(vs...)
	if err != nil {
		panic(err)
	}
	return s
}

func textList(ss []string) value.Value {
	vs := make([]value.Value, len(ss))
	for i, s := range ss {
		vs[i] = text(s)
	}
	l, _ := value.NewList(vs...)
	return l
}

// texts reads a list or set of text.
func texts(b *body, n uint64, out *[]string) error {
	v, ok := b.fields[n]
	if !ok {
		return nil
	}
	var vs []value.Value
	switch c := v.(type) {
	case value.List:
		vs = c.Values()
	case value.Set:
		vs = c.Values()
	default:
		if vv, ok := v.(value.Value); ok && value.IsNull(vv) {
			return nil
		}
		return fmt.Errorf("field %d: %w", n, errFieldType)
	}
	*out = nil
	for _, e := range vs {
		t, ok := e.(value.Text)
		if !ok {
			return fmt.Errorf("field %d: %w", n, errFieldType)
		}
		*out = append(*out, t.Go())
	}
	return nil
}

func paramsVal(p map[string]value.Value) value.Value {
	m, err := value.MapOfText(p)
	if err != nil {
		panic(err)
	}
	return m
}

func getParams(b *body, n uint64, out *map[string]value.Value) error {
	var m value.Map
	if err := get(b, n, &m); err != nil {
		return err
	}
	if m.Len() == 0 {
		return nil
	}
	*out = map[string]value.Value{}
	for k, v := range m.All() {
		t, ok := k.(value.Text)
		if !ok {
			return fmt.Errorf("field %d: keys must be text", n)
		}
		(*out)[t.Go()] = v
	}
	return nil
}

func idOrNull(id *value.ID) value.Value {
	if id == nil {
		return value.Null
	}
	return *id
}

func getID(b *body, n uint64, out **value.ID) error {
	var id value.ID
	ok, err := lookup(b, n, &id)
	if ok {
		*out = &id
	}
	return err
}

// --- handshake and control ---

// Hello is the client's first message (spec/protocol.md §6.2).
type Hello struct {
	Versions      map[int]int // major → highest minor
	Features      []string
	ClientName    string
	ClientVersion string
	Database      string
	User          string
	InlineLimit   int64 // 0: no preference
}

func (*Hello) Type() byte { return TypeHello }

func (m *Hello) encode(b *body) {
	var ks, vs []value.Value
	for maj, mn := range m.Versions {
		ks, vs = append(ks, value.Int(maj)), append(vs, value.Int(mn))
	}
	vm, _ := value.NewMap(ks, vs)
	b.set(1, vm)
	b.set(2, textSet(m.Features))
	b.set(3, text(m.ClientName))
	b.set(4, text(m.ClientVersion))
	b.set(5, textOrNull(m.Database))
	b.set(6, textOrNull(m.User))
	if m.InlineLimit > 0 {
		b.set(7, value.Int(m.InlineLimit))
	}
}

func (m *Hello) decode(b *body) error {
	var vm value.Map
	if err := get(b, 1, &vm); err != nil {
		return err
	}
	m.Versions = map[int]int{}
	for k, v := range vm.All() {
		ki, ok1 := k.(value.Int)
		vi, ok2 := v.(value.Int)
		if !ok1 || !ok2 {
			return errors.New("versions must map ints to ints")
		}
		m.Versions[int(ki)] = int(vi)
	}
	return errors.Join(texts(b, 2, &m.Features), getText(b, 3, &m.ClientName), getText(b, 4, &m.ClientVersion),
		getText(b, 5, &m.Database), getText(b, 6, &m.User), getInt(b, 7, &m.InlineLimit))
}

// HelloOK is the server's reply to Hello.
type HelloOK struct {
	Major, Minor   int
	Features       []string
	ServerVersion  string
	AuthMechanisms []string
	MaxFrameSize   int64
	MaxStreams     int64
	StreamWindow   int64
	ConnWindow     int64
	InlineLimit    int64
}

func (*HelloOK) Type() byte { return TypeHelloOK }

func (m *HelloOK) encode(b *body) {
	p, _ := value.NewPair(value.Int(m.Major), value.Int(m.Minor))
	b.set(1, p)
	b.set(2, textSet(m.Features))
	b.set(3, text(m.ServerVersion))
	b.set(4, textList(m.AuthMechanisms))
	b.set(5, value.Int(m.MaxFrameSize))
	b.set(6, value.Int(m.MaxStreams))
	b.set(7, value.Int(m.StreamWindow))
	b.set(8, value.Int(m.ConnWindow))
	b.set(9, value.Int(m.InlineLimit))
}

func (m *HelloOK) decode(b *body) error {
	var p value.Pair
	if err := get(b, 1, &p); err != nil {
		return err
	}
	if ma, ok := p.Key().(value.Int); ok {
		m.Major = int(ma)
	}
	if mi, ok := p.Value().(value.Int); ok {
		m.Minor = int(mi)
	}
	return errors.Join(texts(b, 2, &m.Features), getText(b, 3, &m.ServerVersion), texts(b, 4, &m.AuthMechanisms),
		getInt(b, 5, &m.MaxFrameSize), getInt(b, 6, &m.MaxStreams), getInt(b, 7, &m.StreamWindow),
		getInt(b, 8, &m.ConnWindow), getInt(b, 9, &m.InlineLimit))
}

// Auth carries a login mechanism's client messages. The first names the
// mechanism, and for KEY the user.
type Auth struct {
	Mechanism string
	Data      []byte
	User      string
}

func (*Auth) Type() byte { return TypeAuth }

func (m *Auth) encode(b *body) {
	b.set(1, textOrNull(m.Mechanism))
	b.set(2, bytesVal(m.Data))
	if m.User != "" {
		b.set(3, text(m.User))
	}
}

func (m *Auth) decode(b *body) error {
	return errors.Join(getText(b, 1, &m.Mechanism), getBytes(b, 2, &m.Data), getText(b, 3, &m.User))
}

// AuthContinue carries a server challenge (SCRAM server-first).
type AuthContinue struct{ Data []byte }

func (*AuthContinue) Type() byte             { return TypeAuthContinue }
func (m *AuthContinue) encode(b *body)       { b.set(1, bytesVal(m.Data)) }
func (m *AuthContinue) decode(b *body) error { return getBytes(b, 1, &m.Data) }

// AuthOK ends a successful login.
type AuthOK struct {
	Data      []byte // SCRAM server-final
	SessionID value.ID
	User      string
	Database  string
}

func (*AuthOK) Type() byte { return TypeAuthOK }

func (m *AuthOK) encode(b *body) {
	b.set(1, bytesVal(m.Data))
	b.set(2, m.SessionID)
	b.set(3, text(m.User))
	b.set(4, textOrNull(m.Database))
}

func (m *AuthOK) decode(b *body) error {
	return errors.Join(getBytes(b, 1, &m.Data), get(b, 2, &m.SessionID), getText(b, 3, &m.User), getText(b, 4, &m.Database))
}

// Ping asks the peer to answer with Pong carrying the same data.
type Ping struct{ Data []byte }

func (*Ping) Type() byte             { return TypePing }
func (m *Ping) encode(b *body)       { b.set(1, bytesVal(m.Data)) }
func (m *Ping) decode(b *body) error { return getBytes(b, 1, &m.Data) }

// Pong answers a Ping.
type Pong struct{ Data []byte }

func (*Pong) Type() byte             { return TypePong }
func (m *Pong) encode(b *body)       { b.set(1, bytesVal(m.Data)) }
func (m *Pong) decode(b *body) error { return getBytes(b, 1, &m.Data) }

// GoAway announces that the sender is closing the connection.
type GoAway struct {
	LastStream int64
	Code       string
	Message    string
}

func (*GoAway) Type() byte { return TypeGoAway }

func (m *GoAway) encode(b *body) {
	b.set(1, value.Int(m.LastStream))
	b.set(2, text(m.Code))
	b.set(3, text(m.Message))
}

func (m *GoAway) decode(b *body) error {
	return errors.Join(getInt(b, 1, &m.LastStream), getText(b, 2, &m.Code), getText(b, 3, &m.Message))
}

// WindowUpdate grants the peer more flow-control window.
type WindowUpdate struct{ Increment int64 }

func (*WindowUpdate) Type() byte             { return TypeWindowUpdate }
func (m *WindowUpdate) encode(b *body)       { b.set(1, value.Int(m.Increment)) }
func (m *WindowUpdate) decode(b *body) error { return getInt(b, 1, &m.Increment) }

// ConnErrorMsg reports a fatal connection error before closing.
type ConnErrorMsg struct {
	Code    string
	Message string
}

func (*ConnErrorMsg) Type() byte { return TypeConnError }

func (m *ConnErrorMsg) encode(b *body) {
	b.set(1, text(m.Code))
	b.set(2, text(m.Message))
}

func (m *ConnErrorMsg) decode(b *body) error {
	return errors.Join(getText(b, 1, &m.Code), getText(b, 2, &m.Message))
}

// --- requests ---

// Query runs Rhizome text (spec/protocol.md §6.3).
type Query struct {
	Text    string
	Params  map[string]value.Value
	Txn     *value.ID
	Options map[string]value.Value
}

func (*Query) Type() byte { return TypeQuery }

func (m *Query) encode(b *body) {
	b.set(1, text(m.Text))
	b.set(2, paramsVal(m.Params))
	b.set(3, idOrNull(m.Txn))
	b.set(4, paramsVal(m.Options))
}

func (m *Query) decode(b *body) error {
	return errors.Join(getText(b, 1, &m.Text), getParams(b, 2, &m.Params), getID(b, 3, &m.Txn), getParams(b, 4, &m.Options))
}

// Prepare prepares one statement for repeated execution.
type Prepare struct{ Text string }

func (*Prepare) Type() byte             { return TypePrepare }
func (m *Prepare) encode(b *body)       { b.set(1, text(m.Text)) }
func (m *Prepare) decode(b *body) error { return getText(b, 1, &m.Text) }

// Execute runs a prepared statement.
type Execute struct {
	StatementID int64
	Params      map[string]value.Value
	Txn         *value.ID
	Options     map[string]value.Value
}

func (*Execute) Type() byte { return TypeExecute }

func (m *Execute) encode(b *body) {
	b.set(1, value.Int(m.StatementID))
	b.set(2, paramsVal(m.Params))
	b.set(3, idOrNull(m.Txn))
	b.set(4, paramsVal(m.Options))
}

func (m *Execute) decode(b *body) error {
	return errors.Join(getInt(b, 1, &m.StatementID), getParams(b, 2, &m.Params), getID(b, 3, &m.Txn), getParams(b, 4, &m.Options))
}

// CloseStatement frees a prepared statement.
type CloseStatement struct{ StatementID int64 }

func (*CloseStatement) Type() byte             { return TypeCloseStatement }
func (m *CloseStatement) encode(b *body)       { b.set(1, value.Int(m.StatementID)) }
func (m *CloseStatement) decode(b *body) error { return getInt(b, 1, &m.StatementID) }

// BlobRead fetches part of a value sent as a blob reference.
type BlobRead struct {
	Ref    *BlobRef
	Offset int64
	Length int64 // 0: to the end
}

func (*BlobRead) Type() byte { return TypeBlobRead }

func (m *BlobRead) encode(b *body) {
	b.set(1, m.Ref)
	b.set(2, value.Int(m.Offset))
	b.set(3, value.Int(m.Length))
}

func (m *BlobRead) decode(b *body) error {
	return errors.Join(get(b, 1, &m.Ref), getInt(b, 2, &m.Offset), getInt(b, 3, &m.Length))
}

// Cancel stops the request on its stream.
type Cancel struct{}

func (*Cancel) Type() byte         { return TypeCancel }
func (*Cancel) encode(*body)       {}
func (*Cancel) decode(*body) error { return nil }

// --- responses ---

// Column is a result column: its name and type expression.
type Column struct {
	Name string
	Type string
}

func columnsVal(cs []Column) value.Value {
	vs := make([]value.Value, len(cs))
	for i, c := range cs {
		r, _ := value.NewRecord([]string{"name", "type"}, []value.Value{text(c.Name), text(c.Type)})
		vs[i] = r
	}
	l, _ := value.NewList(vs...)
	return l
}

func getColumns(b *body, n uint64, out *[]Column) error {
	var l value.List
	if err := get(b, n, &l); err != nil {
		return err
	}
	*out = nil
	for _, v := range l.Values() {
		r, ok := v.(value.Record)
		if !ok {
			return fmt.Errorf("field %d: columns must be records", n)
		}
		nm, _ := r.Get("name")
		ty, _ := r.Get("type")
		nt, ok1 := nm.(value.Text)
		tt, ok2 := ty.(value.Text)
		if !ok1 || !ok2 {
			return fmt.Errorf("field %d: columns need a name and type", n)
		}
		*out = append(*out, Column{nt.Go(), tt.Go()})
	}
	return nil
}

// ResultHeader starts a statement's result.
type ResultHeader struct {
	StatementIndex int64
	Columns        []Column
}

func (*ResultHeader) Type() byte { return TypeResultHeader }

func (m *ResultHeader) encode(b *body) {
	b.set(1, value.Int(m.StatementIndex))
	b.set(2, columnsVal(m.Columns))
}

func (m *ResultHeader) decode(b *body) error {
	return errors.Join(getInt(b, 1, &m.StatementIndex), getColumns(b, 2, &m.Columns))
}

// Rows carries a batch of result rows. Cells are value.Value or the
// result-only types (*Entity, *Field, *Relation, *Path, *BlobRef).
type Rows struct {
	Rows [][]any
}

func (*Rows) Type() byte { return TypeRows }

func (m *Rows) encode(b *body) {
	rows := make([]any, len(m.Rows))
	for i, r := range m.Rows {
		rows[i] = r
	}
	b.set(1, rows)
}

func (m *Rows) decode(b *body) error {
	v, ok := b.fields[1]
	if !ok {
		return nil
	}
	m.Rows = nil
	switch rows := v.(type) {
	case []any:
		for _, r := range rows {
			cells, err := rowCells(r)
			if err != nil {
				return err
			}
			m.Rows = append(m.Rows, cells)
		}
	case value.List:
		for _, r := range rows.Values() {
			cells, err := rowCells(r)
			if err != nil {
				return err
			}
			m.Rows = append(m.Rows, cells)
		}
	default:
		return errors.New("rows must be a list")
	}
	return nil
}

func rowCells(r any) ([]any, error) {
	switch c := r.(type) {
	case []any:
		return c, nil
	case value.List:
		out := make([]any, c.Len())
		for i, v := range c.Values() {
			out[i] = v
		}
		return out, nil
	}
	return nil, errors.New("a row must be a list")
}

// TxnState is a transaction's state after a statement.
type TxnState string

const (
	TxnNone       TxnState = "none"
	TxnOpen       TxnState = "open"
	TxnFailed     TxnState = "failed"
	TxnCommitted  TxnState = "committed"
	TxnRolledBack TxnState = "rolled_back"
)

// ResultEnd ends a statement's result.
type ResultEnd struct {
	StatementIndex int64
	RowsReturned   int64
	RowsAffected   int64
	CreatedIDs     []value.ID
	Txn            *value.ID
	TxnState       TxnState
	Elapsed        time.Duration
	Warnings       []string
	Plan           value.Value // explain plans; nil if none
}

func (*ResultEnd) Type() byte { return TypeResultEnd }

func (m *ResultEnd) encode(b *body) {
	b.set(1, value.Int(m.StatementIndex))
	b.set(2, value.Int(m.RowsReturned))
	b.set(3, value.Int(m.RowsAffected))
	ids := make([]value.Value, len(m.CreatedIDs))
	for i, id := range m.CreatedIDs {
		ids[i] = id
	}
	l, _ := value.NewList(ids...)
	b.set(4, l)
	b.set(5, idOrNull(m.Txn))
	state := m.TxnState
	if state == "" {
		state = TxnNone
	}
	b.set(6, text(string(state)))
	b.set(7, value.Duration(m.Elapsed))
	b.set(8, textList(m.Warnings))
	if m.Plan != nil {
		b.set(9, m.Plan)
	}
}

func (m *ResultEnd) decode(b *body) error {
	var ids value.List
	var state string
	var d value.Duration
	err := errors.Join(getInt(b, 1, &m.StatementIndex), getInt(b, 2, &m.RowsReturned), getInt(b, 3, &m.RowsAffected),
		get(b, 4, &ids), getID(b, 5, &m.Txn), getText(b, 6, &state), get(b, 7, &d), texts(b, 8, &m.Warnings))
	if err != nil {
		return err
	}
	for _, v := range ids.Values() {
		id, ok := v.(value.ID)
		if !ok {
			return errors.New("created_ids must be ids")
		}
		m.CreatedIDs = append(m.CreatedIDs, id)
	}
	m.TxnState, m.Elapsed = TxnState(state), time.Duration(d)
	if p, ok := b.fields[9].(value.Value); ok {
		m.Plan = p
	}
	return nil
}

// Error ends a request with a stream error (spec/protocol.md §6.4).
type Error struct {
	Code           string
	Message        string
	Line, Column   int64 // 0 if no position
	StatementIndex int64 // -1 if not tied to a statement
	Retryable      bool
	Details        map[string]value.Value
}

func (*Error) Type() byte { return TypeError }

func (e *Error) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%d:%d: %s: %s", e.Line, e.Column, e.Code, e.Message)
	}
	return e.Code + ": " + e.Message
}

func (m *Error) encode(b *body) {
	b.set(1, text(m.Code))
	b.set(2, text(m.Message))
	if m.Line > 0 {
		p, _ := value.NewPair(value.Int(m.Line), value.Int(m.Column))
		b.set(3, p)
	} else {
		b.set(3, value.Null)
	}
	if m.StatementIndex >= 0 {
		b.set(4, value.Int(m.StatementIndex))
	} else {
		b.set(4, value.Null)
	}
	b.set(5, value.Bool(m.Retryable))
	b.set(6, paramsVal(m.Details))
}

func (m *Error) decode(b *body) error {
	var pos value.Pair
	m.StatementIndex = -1
	err := errors.Join(getText(b, 1, &m.Code), getText(b, 2, &m.Message), get(b, 3, &pos),
		getInt(b, 4, &m.StatementIndex), getBool(b, 5, &m.Retryable), getParams(b, 6, &m.Details))
	if err != nil {
		return err
	}
	if l, ok := pos.Key().(value.Int); ok {
		m.Line = int64(l)
	}
	if c, ok := pos.Value().(value.Int); ok {
		m.Column = int64(c)
	}
	return nil
}

// Prepared answers Prepare.
type Prepared struct {
	StatementID int64
	ParamTypes  map[string]string // parameter → type expression
	Columns     []Column          // nil if not known in advance
}

func (*Prepared) Type() byte { return TypePrepared }

func (m *Prepared) encode(b *body) {
	pt := map[string]value.Value{}
	for k, v := range m.ParamTypes {
		pt[k] = text(v)
	}
	b.set(1, value.Int(m.StatementID))
	b.set(2, paramsVal(pt))
	if m.Columns != nil {
		b.set(3, columnsVal(m.Columns))
	}
}

func (m *Prepared) decode(b *body) error {
	var pt map[string]value.Value
	if err := errors.Join(getInt(b, 1, &m.StatementID), getParams(b, 2, &pt), getColumns(b, 3, &m.Columns)); err != nil {
		return err
	}
	if len(pt) > 0 {
		m.ParamTypes = map[string]string{}
	}
	for k, v := range pt {
		t, ok := v.(value.Text)
		if !ok {
			return errors.New("param_types must be text")
		}
		m.ParamTypes[k] = t.Go()
	}
	return nil
}

// BlobData carries part of a blob.
type BlobData struct {
	Offset int64
	Data   []byte
}

func (*BlobData) Type() byte { return TypeBlobData }

func (m *BlobData) encode(b *body) {
	b.set(1, value.Int(m.Offset))
	b.set(2, bytesVal(m.Data))
}

func (m *BlobData) decode(b *body) error {
	return errors.Join(getInt(b, 1, &m.Offset), getBytes(b, 2, &m.Data))
}

// Notice reports a warning or progress during a request.
type Notice struct {
	Code    string
	Message string
}

func (*Notice) Type() byte { return TypeNotice }

func (m *Notice) encode(b *body) {
	b.set(1, text(m.Message))
	b.set(2, textOrNull(m.Code))
}

func (m *Notice) decode(b *body) error {
	return errors.Join(getText(b, 1, &m.Message), getText(b, 2, &m.Code))
}
