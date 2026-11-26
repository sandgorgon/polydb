package client

import (
	"context"
	"strings"
	"testing"

	"github.com/sandgorgon/polydb"
	"github.com/sandgorgon/polydb/internal/wire"
	"github.com/sandgorgon/polydb/value"
)

// Blobs the server holds go as their lease tokens; others as values.
func TestSplitParams(t *testing.T) {
	c := &Conn{}
	held := c.blob(&wire.BlobRef{Type: "bytes", Length: 3, Token: []byte("tok")})
	mine := polydb.NewBlob("bytes", 2, "", polydb.BlobFunc(func(context.Context, int64, int64) ([]byte, error) {
		return []byte("hi"), nil
	}))
	vals, tokens, err := splitParams(polydb.Params{"a": held, "b": mine, "n": 1})
	if err != nil {
		t.Fatal(err)
	}
	if string(tokens["a"]) != "tok" || len(tokens) != 1 {
		t.Errorf("tokens %q", tokens)
	}
	if b, ok := vals["b"].(value.Bytes); !ok || string(b.Go()) != "hi" || vals["n"] != value.Int(1) {
		t.Errorf("values %v", vals)
	}
}

func TestTraceRedacts(t *testing.T) {
	trace := traceText(&wire.Auth{Mechanism: "SCRAM-SHA-256", Data: []byte("n,,n=admin,r=secretnonce")})
	if strings.Contains(trace, "secretnonce") || !strings.Contains(trace, "redacted") {
		t.Errorf("trace leaks: %s", trace)
	}
}
