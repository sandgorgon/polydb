package wire

import (
	"bytes"
	"compress/flate"
	"io"
	"sync"
)

// Writer writes messages as frames (spec/protocol.md §4). It is safe for
// concurrent use; frames of different messages may interleave, which the
// protocol allows.
type Writer struct {
	mu       sync.Mutex
	w        io.Writer
	maxFrame int
	compress bool
}

// NewWriter returns a writer with the default frame size and no
// compression.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w, maxFrame: DefaultMaxFrameSize}
}

// SetMaxFrameSize sets the largest frame payload to send.
func (w *Writer) SetMaxFrameSize(n int) {
	w.mu.Lock()
	w.maxFrame = min(max(n, 1<<10), MaxFrameSizeLimit)
	w.mu.Unlock()
}

// SetCompression turns message compression on or off (it needs the
// deflate feature, spec/protocol.md §10).
func (w *Writer) SetCompression(on bool) {
	w.mu.Lock()
	w.compress = on
	w.mu.Unlock()
}

// Acquire is called before each frame of a flow-controlled message with
// the payload size the writer would like to send; it returns how many
// bytes may be sent (at least 1), waiting for window if needed.
type Acquire func(want int) (int, error)

// WriteMessage writes m on a stream, with END_STREAM on its last frame if
// end is set.
func (w *Writer) WriteMessage(stream uint32, m Message, end bool) error {
	return w.WriteBody(stream, m.Type(), Encode(m), end, nil)
}

// WriteBody writes an encoded message body, splitting it into frames.
// acquire, if not nil, limits each frame by flow-control window.
func (w *Writer) WriteBody(stream uint32, typ byte, body []byte, end bool, acquire Acquire) error {
	if len(body) > MaxMessageSize {
		return protocolErr("message of %d bytes exceeds the %d byte limit", len(body), MaxMessageSize)
	}
	w.mu.Lock()
	maxFrame, compress := w.maxFrame, w.compress
	w.mu.Unlock()
	var flags byte
	if compress && len(body) >= CompressMin && typ != TypeBlobData {
		if c, ok := deflate(body); ok {
			body, flags = c, FlagCompressed
		}
	}
	for first := true; first || len(body) > 0; first = false {
		n := min(len(body), maxFrame)
		if acquire != nil && n > 0 {
			got, err := acquire(n)
			if err != nil {
				return err
			}
			n = min(n, got)
		}
		f := Frame{Type: typ, Flags: flags, Stream: stream, Payload: body[:n]}
		body = body[n:]
		if len(body) > 0 {
			f.Flags |= FlagContinued
		} else if end {
			f.Flags |= FlagEndStream
		}
		w.mu.Lock()
		_, err := w.w.Write(AppendFrame(nil, f))
		w.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

// deflate compresses b, reporting false if that doesn't make it smaller.
func deflate(b []byte) ([]byte, bool) {
	var buf bytes.Buffer
	zw, _ := flate.NewWriter(&buf, flate.DefaultCompression)
	zw.Write(b)
	zw.Close()
	if buf.Len() >= len(b) {
		return nil, false
	}
	return buf.Bytes(), true
}

// inflate decompresses b, failing if the result would exceed the message
// size limit (a decompression bomb).
func inflate(b []byte) ([]byte, error) {
	zr := flate.NewReader(bytes.NewReader(b))
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, MaxMessageSize+1))
	if err != nil {
		return nil, protocolErr("bad compressed message: %v", err)
	}
	if len(out) > MaxMessageSize {
		return nil, protocolErr("compressed message expands beyond %d bytes", MaxMessageSize)
	}
	return out, nil
}

// Incoming is a message read from the connection.
type Incoming struct {
	Stream    uint32
	Msg       Message
	EndStream bool
	// Size is the number of payload bytes the message took on the wire,
	// which flow control counts for ROWS and BLOB_DATA.
	Size int
}

// Reader reads frames and reassembles messages. It isn't safe for
// concurrent use.
type Reader struct {
	r        io.Reader
	maxFrame int
	partial  map[uint32]*partial
	pending  int // bytes held in partial messages
}

type partial struct {
	typ   byte
	flags byte
	data  []byte
}

// NewReader returns a reader accepting frames up to the default size.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: r, maxFrame: DefaultMaxFrameSize, partial: map[uint32]*partial{}}
}

// SetMaxFrameSize sets the largest frame payload to accept.
func (r *Reader) SetMaxFrameSize(n int) { r.maxFrame = min(max(n, 1<<10), MaxFrameSizeLimit) }

// ReadMessage reads frames until a whole message has arrived on some
// stream. Unknown frame types are skipped, unless they carry
// MUST_UNDERSTAND.
func (r *Reader) ReadMessage() (Incoming, error) {
	for {
		f, err := ReadFrame(r.r, r.maxFrame)
		if err != nil {
			return Incoming{}, err
		}
		if newMessage(f.Type) == nil {
			if f.Flags&FlagMustUnderstand != 0 {
				return Incoming{}, protocolErr("unknown frame type 0x%02x marked must-understand", f.Type)
			}
			continue
		}
		p := r.partial[f.Stream]
		if p == nil {
			p = &partial{typ: f.Type, flags: f.Flags & FlagCompressed}
		} else if p.typ != f.Type || p.flags != f.Flags&FlagCompressed {
			return Incoming{}, protocolErr("stream %d: a continued message changed type or compression", f.Stream)
		}
		p.data = append(p.data, f.Payload...)
		r.pending += len(f.Payload)
		if len(p.data) > MaxMessageSize || r.pending > 4*MaxMessageSize {
			return Incoming{}, protocolErr("message exceeds %d bytes", MaxMessageSize)
		}
		if f.Flags&FlagContinued != 0 {
			r.partial[f.Stream] = p
			continue
		}
		delete(r.partial, f.Stream)
		r.pending -= len(p.data)
		data := p.data
		if p.flags&FlagCompressed != 0 {
			if data, err = inflate(data); err != nil {
				return Incoming{}, err
			}
		}
		m, err := Decode(p.typ, data)
		if err != nil {
			return Incoming{}, err
		}
		return Incoming{Stream: f.Stream, Msg: m, EndStream: f.Flags&FlagEndStream != 0, Size: len(p.data)}, nil
	}
}
