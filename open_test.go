package polydb_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sandgorgon/polydb"
)

func TestOpenOptions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := polydb.Open(dir, &polydb.Options{Database: "shop", MaxMemory: 64 << 10, StatementTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	// Only one program, or one Open, at a time.
	if _, err := polydb.Open(dir, nil); err == nil {
		t.Error("a second Open of the same directory")
	}
	rows, err := db.Query(ctx, `show limits`, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var name string
		var v any
		rows.Scan(&name, &v, new(string))
		got = append(got, name)
		if name == "max_memory" && fmt.Sprint(v) != "65536" {
			t.Errorf("max_memory %v", v)
		}
	}
	rows.Close()
	if len(got) == 0 {
		t.Fatal("show limits gave nothing")
	}
	if _, err := db.Exec(ctx, `define kind P { s: text }`, nil); err != nil {
		t.Fatal(err)
	}
	// The limit applies.
	if _, err := db.Exec(ctx, `create P { s: "x" }; create P { s: "y" }`, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Query(ctx, `find P a, P b, P c, P d, P e, P f, P g, P h, P i, P j, P k, P l | show a.s ++ b.s ++ c.s ++ d.s ++ e.s ++ f.s ++ g.s ++ h.s ++ i.s ++ j.s ++ k.s ++ l.s as s | sort s`, nil); polydb.CodeOf(err) != "limit" {
		t.Errorf("past max_memory: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopened, the named database is there and used.
	db, err = polydb.Open(dir, &polydb.Options{Database: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ctx, `find P p | show count()`, nil); err != nil {
		t.Errorf("reopened: %v", err)
	}
	if _, err := polydb.Open("", nil); err == nil {
		t.Error("Open with no directory")
	}
}

func TestEmbeddedUpload(t *testing.T) {
	ctx := context.Background()
	db, err := polydb.OpenMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ctx, `define kind F { b: bytes, t: text? }`, nil); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte{1, 2, 3, 4, 5}, 200_000)
	if _, err := db.Upload(ctx, bytes.NewReader(data), "video", ""); polydb.CodeOf(err) != "type" {
		t.Errorf("an unknown type: %v", err)
	}
	if _, err := db.Upload(ctx, bytes.NewReader([]byte{0xff, 0xfe}), "text", ""); polydb.CodeOf(err) != "type" {
		t.Errorf("text that isn't UTF-8: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := db.Upload(cancelled, bytes.NewReader(data), "bytes", ""); polydb.CodeOf(err) != "cancelled" {
		t.Errorf("cancelled: %v", err)
	}
	// Uploaded, then stored inside a transaction.
	b, err := db.Upload(ctx, bytes.NewReader(data), "bytes", "application/x-test")
	if err != nil {
		t.Fatal(err)
	}
	if b.Length != int64(len(data)) || b.Type != "bytes" {
		t.Errorf("blob %+v", b)
	}
	tx, err := db.Begin(ctx, polydb.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `create F { b: $b }`, polydb.Params{"b": b}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(ctx, `find F f | show f.b`, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no row")
	}
	var got *polydb.Blob
	if err := rows.Scan(&got); err != nil {
		t.Fatal(err)
	}
	back, err := io.ReadAll(got.Reader())
	if err != nil || !bytes.Equal(back, data) || got.MediaType != "application/x-test" {
		t.Errorf("read back %d bytes, %v, media %q", len(back), err, got.MediaType)
	}
	// A failing reader fails the upload.
	if _, err := db.Upload(ctx, io.MultiReader(strings.NewReader("abc"), errReader{}), "bytes", ""); err == nil {
		t.Error("a failing reader")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("broken") }
