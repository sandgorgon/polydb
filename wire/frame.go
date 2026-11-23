// Package wire implements PolyDB's wire protocol (spec/protocol.md):
// the connection preface, frames, message bodies with numbered fields,
// the result-only values (entities, fields, relations, paths and blob
// references), message compression and flow-control windows. Both the
// server and the Go client build on it.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Protocol constants (spec/protocol.md §1–§4).
const (
	Major = 1 // protocol major version
	Minor = 0 // protocol minor version

	ALPN        = "polydb/1"
	DefaultPort = 7377

	HeaderSize          = 12
	DefaultMaxFrameSize = 1 << 20  // 1 MiB
	MaxFrameSizeLimit   = 16 << 20 // 16 MiB
	MaxMessageSize      = 64 << 20 // 64 MiB, reassembled

	DefaultStreamWindow = 1 << 20  // 1 MiB
	DefaultConnWindow   = 16 << 20 // 16 MiB
	DefaultMaxStreams   = 100

	DefaultInlineLimit = 16 << 10 // 16 KiB
	MinInlineLimit     = 1 << 10
	MaxInlineLimit     = MaxMessageSize

	CompressMin = 1 << 10 // messages smaller than this aren't compressed
)

// Preface is what a client sends first, followed by its highest major
// version as two big-endian bytes.
var Preface = []byte("PLYDB\r\n")

// WritePreface writes the connection preface.
func WritePreface(w io.Writer) error {
	b := append(append([]byte(nil), Preface...), 0, Major)
	_, err := w.Write(b)
	return err
}

// ReadPreface reads the connection preface and returns the client's
// highest major version.
func ReadPreface(r io.Reader) (int, error) {
	b := make([]byte, len(Preface)+2)
	if _, err := io.ReadFull(r, b); err != nil {
		return 0, err
	}
	if string(b[:len(Preface)]) != string(Preface) {
		return 0, ErrNotPolyDB
	}
	return int(binary.BigEndian.Uint16(b[len(Preface):])), nil
}

// ErrNotPolyDB means the peer didn't send the PolyDB preface.
var ErrNotPolyDB = errors.New("wire: not a PolyDB connection")

// Frame flags (spec/protocol.md §4).
const (
	FlagEndStream      = 0x01
	FlagContinued      = 0x02
	FlagCompressed     = 0x04
	FlagMustUnderstand = 0x08
)

// Frame is one frame.
type Frame struct {
	Type    byte
	Flags   byte
	Stream  uint32
	Payload []byte
}

// ConnError is a fatal connection error (spec/protocol.md §11.1).
type ConnError struct {
	Code string
	Msg  string
}

func (e *ConnError) Error() string { return "wire: " + e.Code + ": " + e.Msg }

func protocolErr(format string, args ...any) error {
	return &ConnError{Code: "protocol_error", Msg: fmt.Sprintf(format, args...)}
}

// ReadFrame reads one frame whose payload is at most maxSize bytes.
func ReadFrame(r io.Reader, maxSize int) (Frame, error) {
	var h [HeaderSize]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(h[0:4])
	if int64(n) > int64(maxSize) {
		return Frame{}, protocolErr("frame of %d bytes exceeds the %d byte limit", n, maxSize)
	}
	if h[6] != 0 || h[7] != 0 {
		return Frame{}, protocolErr("reserved frame bits are set")
	}
	f := Frame{Type: h[4], Flags: h[5], Stream: binary.BigEndian.Uint32(h[8:12]), Payload: make([]byte, n)}
	if _, err := io.ReadFull(r, f.Payload); err != nil {
		return Frame{}, err
	}
	return f, nil
}

// AppendFrame appends a frame's encoding to dst.
func AppendFrame(dst []byte, f Frame) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(f.Payload)))
	dst = append(dst, f.Type, f.Flags, 0, 0)
	dst = binary.BigEndian.AppendUint32(dst, f.Stream)
	return append(dst, f.Payload...)
}
