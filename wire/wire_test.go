package wire

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/value"
)

func tm(s string) value.Time {
	t, err := value.ParseTime(s)
	if err != nil {
		panic(err)
	}
	return t
}

func sampleEntity() *Entity {
	labels, _ := value.NewSet(value.MustText("vip"))
	props, _ := value.MapOfText(map[string]value.Value{"source": value.MustText("import")})
	return &Entity{
		ID: value.ID{1}, Kind: "Person",
		Meta: Meta{Created: tm("2026-11-22T12:00:00Z"), Updated: tm("2026-11-22T12:00:01Z"), Version: 3,
			Labels: labels, Owner: "admin", Props: props},
		Fields: []*Field{
			{ID: value.ID{2}, Name: "name", Type: "text", Doc: "Full name", Declared: true, Value: value.MustText("Ann")},
			{ID: value.ID{3}, Name: "age", Type: "int?", Declared: true}, // absent
			{ID: value.ID{4}, Name: "photo", Type: "image?", Value: &BlobRef{Type: "image", Hash: []byte{9, 9},
				Length: 1 << 20, MediaType: "image/png", Token: []byte("lease")}},
		},
	}
}

func sampleMessages() []Message {
	txn := value.ID{7}
	since, _ := value.MapOfText(map[string]value.Value{"since": value.Int(2019)})
	rel := &Relation{ID: value.ID{5}, Type: "knows", From: value.ID{1}, To: value.ID{6}, Props: since}
	return []Message{
		&Hello{Versions: map[int]int{1: 0}, Features: []string{"deflate", "queue-wait"}, ClientName: "polydb-go",
			ClientVersion: "0.1", Database: "sales", User: "ramon", InlineLimit: 4096},
		&HelloOK{Major: 1, Minor: 0, Features: []string{"queue-wait"}, ServerVersion: "0.1",
			AuthMechanisms: []string{"SCRAM-SHA-256-PLUS", "KEY"}, MaxFrameSize: 1 << 20, MaxStreams: 100,
			StreamWindow: 1 << 20, ConnWindow: 16 << 20, InlineLimit: 4096},
		&Auth{Mechanism: "SCRAM-SHA-256", Data: []byte("n,,n=ramon,r=abc")},
		&Auth{Mechanism: "KEY", User: "ramon"},
		&AuthContinue{Data: []byte("r=abcdef,s=c2FsdA==,i=4096")},
		&AuthOK{Data: []byte("v=sig"), SessionID: value.ID{8}, User: "ramon", Database: "sales"},
		&Ping{Data: []byte("12345678")},
		&Pong{Data: []byte("12345678")},
		&GoAway{LastStream: 41, Code: "shutting_down", Message: "bye"},
		&WindowUpdate{Increment: 65536},
		&ConnErrorMsg{Code: "auth_failed", Message: "login failed"},
		&Query{Text: "find Person p | where p.age > $min", Params: map[string]value.Value{"min": value.Int(30)},
			Txn: &txn, Options: map[string]value.Value{"timeout": value.Duration(time.Second)}},
		&Query{Text: "begin"},
		&Prepare{Text: "create Person { name: $n }"},
		&Execute{StatementID: 3, Params: map[string]value.Value{"n": value.MustText("Bob")}},
		&CloseStatement{StatementID: 3},
		&BlobRead{Ref: &BlobRef{Type: "bytes", Hash: []byte{1}, Length: 10, Token: []byte("t")}, Offset: 2, Length: 5},
		&Cancel{},
		&ResultHeader{StatementIndex: 0, Columns: []Column{{"p", "any"}, {"p.name", "text"}}},
		&Rows{Rows: [][]any{
			{sampleEntity(), value.MustText("Ann")},
			{rel, value.Null},
			{&Path{Nodes: []any{sampleEntity(), value.ID{9}}, Rels: []*Relation{rel}}, value.Int(1)},
			{[]any{sampleEntity()}, mustList(value.Int(1), value.Int(2))},
		}},
		&Rows{Rows: [][]any{{value.Int(1), value.Int(2)}, {value.Int(3), value.Int(4)}}},
		&ResultEnd{StatementIndex: 1, RowsReturned: 4, RowsAffected: 1, CreatedIDs: []value.ID{{1}, {2}}, Txn: &txn,
			TxnState: TxnOpen, Elapsed: 3 * time.Millisecond, Warnings: []string{"cross product"}, Plan: value.MustText("scan")},
		&Error{Code: "type", Message: "bad", Line: 2, Column: 5, StatementIndex: 1, Retryable: false,
			Details: map[string]value.Value{"suggestion": value.MustText("email")}},
		&Error{Code: "conflict", Message: "retry", StatementIndex: -1, Retryable: true},
		&Prepared{StatementID: 3, ParamTypes: map[string]string{"n": "text"}, Columns: []Column{{"id", "id"}}},
		&BlobData{Offset: 4, Data: []byte("data")},
		&Notice{Code: "progress", Message: "50%"},
	}
}

func mustList(vs ...value.Value) value.List {
	l, err := value.NewList(vs...)
	if err != nil {
		panic(err)
	}
	return l
}

func TestMessageRoundTrip(t *testing.T) {
	for _, m := range sampleMessages() {
		b := Encode(m)
		got, err := Decode(m.Type(), b)
		if err != nil {
			t.Errorf("%T: %v", m, err)
			continue
		}
		if again := Encode(got); !bytes.Equal(again, b) {
			t.Errorf("%T: re-encoding differs", m)
		}
		if !reflect.DeepEqual(normalize(got), normalize(m)) {
			t.Errorf("%T:\n got %#v\nwant %#v", m, got, m)
		}
	}
}

// normalize makes nil and empty collections compare equal.
func normalize(m Message) Message {
	switch x := m.(type) {
	case *Query:
		c := *x
		if len(c.Params) == 0 {
			c.Params = nil
		}
		if len(c.Options) == 0 {
			c.Options = nil
		}
		return &c
	case *Execute:
		c := *x
		if len(c.Options) == 0 {
			c.Options = nil
		}
		return &c
	case *Error:
		c := *x
		if len(c.Details) == 0 {
			c.Details = nil
		}
		return &c
	case *Rows:
		// Value lists decode as value.List; compare by encoding instead.
		return &Rows{Rows: [][]any{{string(Encode(x))}}}
	}
	return m
}

func TestResultValues(t *testing.T) {
	e := sampleEntity()
	b := AppendAny(nil, e)
	v, n, err := DecodeAny(b)
	if err != nil || n != len(b) {
		t.Fatalf("decode: %v (%d of %d bytes)", err, n, len(b))
	}
	got := v.(*Entity)
	if got.Kind != "Person" || got.Meta.Owner != "admin" || got.Meta.Version != 3 || len(got.Fields) != 3 {
		t.Errorf("entity: %+v", got)
	}
	if got.Fields[1].Value != nil {
		t.Error("absent field decoded with a value")
	}
	if br, ok := got.Fields[2].Value.(*BlobRef); !ok || br.Length != 1<<20 || string(br.Token) != "lease" {
		t.Errorf("blob ref: %#v", got.Fields[2].Value)
	}
	// A list with only plain values decodes as a value.List.
	v, _, _ = DecodeAny(AppendAny(nil, []any{value.Int(1), value.MustText("x")}))
	if _, ok := v.(value.List); !ok {
		t.Errorf("plain list decoded as %T", v)
	}
	// Truncated input never panics.
	for i := range b {
		if _, _, err := DecodeAny(b[:i]); err == nil {
			t.Errorf("truncated at %d: no error", i)
		}
	}
}

func TestDecodeErrors(t *testing.T) {
	good := Encode(&Ping{Data: []byte("x")})
	for name, c := range map[string]struct {
		typ  byte
		data []byte
	}{
		"unknown type":  {0x7E, good},
		"trailing data": {TypePing, append(good, 0)},
		"wrong type":    {TypePing, Encode(&WindowUpdate{Increment: 1})}, // field 1 is an int, not bytes
		"duplicate":     {TypePing, append([]byte{2}, append(good[1:], good[1:]...)...)},
		"bad count":     {TypePing, []byte{0xff}},
	} {
		if _, err := Decode(c.typ, c.data); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Unknown field numbers are ignored.
	b := Encode(&Ping{Data: []byte("x")})
	b[0]++
	b = append(b, 99, 0x10, 0x02) // field 99 = int 1
	if m, err := Decode(TypePing, b); err != nil || string(m.(*Ping).Data) != "x" {
		t.Errorf("unknown field: %v, %v", m, err)
	}
}

func TestFraming(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePreface(&buf); err != nil {
		t.Fatal(err)
	}
	if major, err := ReadPreface(&buf); err != nil || major != Major {
		t.Fatalf("preface: %d, %v", major, err)
	}
	if _, err := ReadPreface(strings.NewReader("GET / HTTP/1.1\r\n")); !errors.Is(err, ErrNotPolyDB) {
		t.Errorf("http preface: %v", err)
	}

	// A large message split across frames, interleaved with another
	// stream, and compressed.
	w := NewWriter(&buf)
	w.SetMaxFrameSize(1024)
	big := &Query{Text: strings.Repeat("find Person p | show p.name; ", 2000)}
	frames := 0
	var parts bytes.Buffer
	pw := NewWriter(&parts)
	pw.SetMaxFrameSize(1024)
	if err := pw.WriteMessage(3, big, true); err != nil {
		t.Fatal(err)
	}
	// Interleave a ping on stream 0 after the first frame of stream 3.
	first, _ := ReadFrame(bytes.NewReader(parts.Bytes()), 1<<20)
	buf.Write(AppendFrame(nil, first))
	w.WriteMessage(0, &Ping{Data: []byte("p")}, false)
	rest := parts.Bytes()[HeaderSize+len(first.Payload):]
	buf.Write(rest)
	r := NewReader(&buf)
	in, err := r.ReadMessage()
	if err != nil || in.Stream != 0 {
		t.Fatalf("ping: %+v, %v", in, err)
	}
	in, err = r.ReadMessage()
	if err != nil || in.Stream != 3 || !in.EndStream || in.Msg.(*Query).Text != big.Text {
		t.Fatalf("big: %v", err)
	}
	frames = len(Encode(big))/1024 + 1
	if frames < 50 {
		t.Errorf("expected many frames, got %d", frames)
	}

	// Compression.
	buf.Reset()
	w = NewWriter(&buf)
	w.SetCompression(true)
	w.WriteMessage(5, big, true)
	wireLen := buf.Len()
	if wireLen > len(Encode(big))/4 {
		t.Errorf("compressed %d bytes to %d", len(Encode(big)), wireLen)
	}
	in, err = NewReader(&buf).ReadMessage()
	if err != nil || in.Msg.(*Query).Text != big.Text || in.Size != wireLen-HeaderSize {
		t.Errorf("compressed read: %v (size %d)", err, in.Size)
	}

	// Unknown frame types are skipped unless marked must-understand.
	buf.Reset()
	buf.Write(AppendFrame(nil, Frame{Type: 0x7E, Payload: []byte("future")}))
	NewWriter(&buf).WriteMessage(0, &Pong{}, false)
	if in, err := NewReader(&buf).ReadMessage(); err != nil || in.Msg.Type() != TypePong {
		t.Errorf("skip unknown: %v", err)
	}
	buf.Reset()
	buf.Write(AppendFrame(nil, Frame{Type: 0x7E, Flags: FlagMustUnderstand}))
	if _, err := NewReader(&buf).ReadMessage(); err == nil {
		t.Error("must-understand frame accepted")
	}

	// Oversized frames are refused.
	buf.Reset()
	buf.Write(AppendFrame(nil, Frame{Type: TypePing, Payload: make([]byte, 2<<20)}))
	if _, err := NewReader(&buf).ReadMessage(); err == nil {
		t.Error("oversized frame accepted")
	}

	// A decompression bomb is refused.
	zeros := make([]byte, MaxMessageSize+10)
	bomb, _ := deflate(zeros)
	buf.Reset()
	r = NewReader(&buf)
	r.SetMaxFrameSize(MaxFrameSizeLimit)
	buf.Write(AppendFrame(nil, Frame{Type: TypePing, Flags: FlagCompressed, Payload: bomb}))
	if _, err := r.ReadMessage(); err == nil || !strings.Contains(err.Error(), "expands") {
		t.Errorf("bomb: %v", err)
	}
}

func TestFlowControl(t *testing.T) {
	ctx := context.Background()
	stream, conn := NewWindow(100), NewWindow(60)
	n, err := TakeBoth(ctx, stream, conn, 80)
	if err != nil || n != 60 || stream.Available() != 40 || conn.Available() != 0 {
		t.Fatalf("TakeBoth: %d %v (stream %d, conn %d)", n, err, stream.Available(), conn.Available())
	}
	done := make(chan int)
	go func() {
		n, _ := TakeBoth(ctx, stream, conn, 30)
		done <- n
	}()
	select {
	case <-done:
		t.Fatal("took from an empty window")
	case <-time.After(20 * time.Millisecond):
	}
	if err := conn.Add(25); err != nil {
		t.Fatal(err)
	}
	if n := <-done; n != 25 {
		t.Errorf("after update took %d", n)
	}
	if err := conn.Add(1 << 31); err == nil {
		t.Error("overflow accepted")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := conn.Take(cctx, 1); err == nil {
		t.Error("cancelled take succeeded")
	}
	conn.Close(errors.New("closed"))
	if _, err := conn.Take(ctx, 1); err == nil {
		t.Error("take on closed window succeeded")
	}

	c := NewCredit(100)
	if err := c.Receive(60); err != nil {
		t.Fatal(err)
	}
	if inc := c.Consume(40); inc != 0 {
		t.Errorf("early update %d", inc)
	}
	if inc := c.Consume(20); inc != 60 {
		t.Errorf("update %d", inc)
	}
	if err := c.Receive(100); err != nil {
		t.Errorf("within window: %v", err)
	}
	if err := c.Receive(1); err == nil {
		t.Error("overrun accepted")
	}
}

// FuzzReadMessage feeds arbitrary bytes to the reader: it must not panic,
// and whatever it decodes must re-encode to something that decodes the
// same.
func FuzzReadMessage(f *testing.F) {
	for _, m := range sampleMessages() {
		var buf bytes.Buffer
		NewWriter(&buf).WriteMessage(1, m, true)
		f.Add(buf.Bytes())
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		r := NewReader(bytes.NewReader(data))
		for range 8 {
			in, err := r.ReadMessage()
			if err != nil {
				return
			}
			b := Encode(in.Msg)
			again, err := Decode(in.Msg.Type(), b)
			if err != nil {
				t.Fatalf("%T re-encoded doesn't decode: %v", in.Msg, err)
			}
			if !bytes.Equal(Encode(again), b) {
				t.Fatalf("%T: encoding not stable", in.Msg)
			}
		}
	})
}
