package client_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	auth9 "github.com/sandgorgon/9auth"
	"github.com/sandgorgon/polydb"
	"github.com/sandgorgon/polydb/client"
	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/security"
	"github.com/sandgorgon/polydb/server"
	"github.com/sandgorgon/polydb/value"
)

type testServer struct {
	eng         *engine.Engine
	addr        string
	fingerprint string
	sock        string
}

// startServer runs a server with a TLS identity on loopback, and a Unix
// socket where the current OS user logs in as admin.
func startServer(t *testing.T) *testServer {
	t.Helper()
	eng, err := engine.Open(engine.Options{PasswordIterations: security.MinIterations})
	if err != nil {
		t.Fatal(err)
	}
	eng.Security().SetPassword("admin", "secret")
	id, err := auth9.LoadFrom(filepath.Join(t.TempDir(), "identity"))
	if err != nil {
		t.Fatal(err)
	}
	peers := map[string]string{}
	if me, err := user.Current(); err == nil {
		peers[me.Username] = "admin"
	}
	srv, err := server.New(server.Config{Engine: eng, TLS: server.IdentityTLS(id), PeerUsers: peers})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(t.TempDir(), "polydb.sock")
	ul, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	go srv.Serve(ul)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		eng.Close()
	})
	return &testServer{eng: eng, addr: l.Addr().String(), fingerprint: id.Fingerprint(), sock: sock}
}

func (ts *testServer) dial(t *testing.T, path string, opts ...client.Option) polydb.DB {
	t.Helper()
	opts = append([]client.Option{client.WithPassword("secret"), client.WithServerFingerprint(ts.fingerprint)}, opts...)
	db, err := client.Dial(context.Background(), "polydb://admin@"+ts.addr+path, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// implementations returns the same database through the embedded
// engine and through the network client.
func implementations(t *testing.T) map[string]polydb.DB {
	ctx := context.Background()
	ts := startServer(t)
	admin := ts.dial(t, "")
	if _, err := admin.Exec(ctx, `create database shop`, nil); err != nil {
		t.Fatal(err)
	}
	emb, err := polydb.Embed(ctx, ts.eng, security.AdminUser, "shop")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { emb.Close() })
	return map[string]polydb.DB{"embedded": emb, "client": ts.dial(t, "/shop")}
}

func TestSharedInterface(t *testing.T) {
	ctx := context.Background()
	impls := implementations(t)
	for _, name := range []string{"embedded", "client"} {
		db := impls[name]
		kind := map[string]string{"embedded": "E", "client": "C"}[name]
		t.Run(name, func(t *testing.T) {
			res, err := db.Exec(ctx, `define kind `+kind+` { name: text, age: int?, joined: date default today() };
				create `+kind+` { name: "Ann", age: 34 }; create `+kind+` { name: "Bob" }`, nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.RowsAffected != 2 || len(res.CreatedIDs) != 2 {
				t.Errorf("exec result %+v", res)
			}

			// Parameters and scanning.
			rows, err := db.Query(ctx, `find `+kind+` p | where p.name = $n | show p.name, p.age, p.joined, p`,
				polydb.Params{"n": "Ann"})
			if err != nil {
				t.Fatal(err)
			}
			if cols := rows.Columns(); len(cols) != 4 || cols[1].Type != "int?" || cols[3].Type != "any" {
				t.Errorf("columns %v", cols)
			}
			if !rows.Next() {
				t.Fatalf("no row: %v", rows.Err())
			}
			var pname string
			var age int64
			var joined time.Time
			var e *polydb.Entity
			if err := rows.Scan(&pname, &age, &joined, &e); err != nil {
				t.Fatal(err)
			}
			if pname != "Ann" || age != 34 || joined.IsZero() || e.Kind != kind || e.Meta.Owner != "admin" {
				t.Errorf("scanned %q %d %v %+v", pname, age, joined, e)
			}
			if v, ok := e.Value("age").(value.Value); !ok || v.String() != "34" {
				t.Errorf("entity age %v", e.Value("age"))
			}
			if rows.Next() {
				t.Error("extra row")
			}
			rows.Close()

			// Nulls need a pointer to a pointer.
			rows, _ = db.Query(ctx, `find `+kind+` p | where p.name = "Bob" | show p.age`, nil)
			rows.Next()
			var agep *int64
			if err := rows.Scan(&agep); err != nil || agep != nil {
				t.Errorf("null: %v %v", agep, err)
			}
			if err := rows.Scan(&age); err == nil {
				t.Error("null scanned into *int64")
			}
			rows.Close()

			// Several result sets.
			rows, err = db.Query(ctx, `find `+kind+` p | show count(); find `+kind+` p | sort p.name | show p.name`, nil)
			if err != nil {
				t.Fatal(err)
			}
			var n int64
			rows.Next()
			rows.Scan(&n)
			var names []string
			if !rows.NextResultSet() {
				t.Fatalf("no second result set: %v", rows.Err())
			}
			for rows.Next() {
				var s string
				rows.Scan(&s)
				names = append(names, s)
			}
			if n != 2 || strings.Join(names, ",") != "Ann,Bob" || rows.NextResultSet() || rows.Err() != nil {
				t.Errorf("result sets: %d %v %v", n, names, rows.Err())
			}
			rows.Close()

			// Prepared statements.
			st, err := db.Prepare(ctx, `create `+kind+` { name: $n }`)
			if err != nil {
				t.Fatal(err)
			}
			for _, nm := range []string{"Cy", "Di"} {
				if _, err := st.Exec(ctx, polydb.Params{"n": nm}); err != nil {
					t.Fatal(err)
				}
			}
			st.Close()
			if _, err := st.Exec(ctx, polydb.Params{"n": "x"}); err == nil && name == "embedded" {
				t.Error("closed statement ran")
			}

			// Transactions.
			tx, err := db.Begin(ctx, polydb.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			tx.Exec(ctx, `create `+kind+` { name: "Tmp" }`, nil)
			if c := count(t, tx, kind); c < 5 {
				t.Errorf("inside the transaction: %d", c)
			}
			outside := count(t, db, kind)
			tx.Rollback(ctx)
			if c := count(t, db, kind); c != outside {
				t.Errorf("after rollback: %d, want %d", c, outside)
			}
			tx, _ = db.Begin(ctx, polydb.TxOptions{})
			if _, err := tx.Exec(ctx, `create `+kind+` { name: 1 }`, nil); polydb.CodeOf(err) != "type" {
				t.Errorf("bad create: %v", err)
			}
			if _, err := tx.Exec(ctx, `find `+kind+` p`, nil); polydb.CodeOf(err) != "txn_aborted" {
				t.Errorf("after failure: %v", err)
			}
			tx.Rollback(ctx)

			// RetryTx under contention.
			db.Exec(ctx, `create `+kind+` { name: "counter", age: 0 }`, nil)
			var wg sync.WaitGroup
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					err := polydb.RetryTx(ctx, db, func(tx polydb.Tx) error {
						_, err := tx.Exec(ctx, `find `+kind+` p | where p.name = "counter" | update p set age += 1`, nil)
						return err
					})
					if err != nil {
						t.Error(err)
					}
				}()
			}
			wg.Wait()
			rows, _ = db.Query(ctx, `find `+kind+` p | where p.name = "counter" | show p.age`, nil)
			rows.Next()
			rows.Scan(&n)
			rows.Close()
			if n != 8 {
				t.Errorf("counter %d after 8 retried increments", n)
			}

			// Errors.
			// The error of a later statement appears when Rows reaches it.
			rows, err = db.Query(ctx, "find "+kind+" p;\nfind Nope q", nil)
			if err == nil {
				for rows.NextResultSet() {
				}
				err = rows.Err()
			}
			var pe *polydb.Error
			if !errors.As(err, &pe) || pe.Code != "not_found" || pe.StatementIndex != 1 || pe.Line != 2 {
				t.Errorf("error: %#v", err)
			}
		})
	}
}

func count(t *testing.T, q polydb.Querier, kind string) int64 {
	t.Helper()
	rows, err := q.Query(context.Background(), `find `+kind+` p | show count()`, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var n int64
	rows.Next()
	if err := rows.Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestURLs(t *testing.T) {
	for raw, want := range map[string]client.Target{
		"polydb://ramon@db.example.com/sales":       {Network: "tcp", Address: "db.example.com:7377", Host: "db.example.com", User: "ramon", Database: "sales"},
		"polydb://ramon:pw@[::1]:9000/":             {Network: "tcp", Address: "[::1]:9000", Host: "::1", User: "ramon", Password: "pw"},
		"polydb+unix:///run/polydb.sock?db=sales":   {Network: "unix", Address: "/run/polydb.sock", Database: "sales"},
		"polydb+unix:///run/polydb.sock?user=ramon": {Network: "unix", Address: "/run/polydb.sock", User: "ramon"},
	} {
		got, err := client.ParseURL(raw)
		if err != nil || got != want {
			t.Errorf("%s: got %+v, %v", raw, got, err)
		}
	}
	for _, bad := range []string{"http://x", "polydb:///sales", "polydb+unix://"} {
		if _, err := client.ParseURL(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestTrustAndLogins(t *testing.T) {
	ctx := context.Background()
	ts := startServer(t)
	url := "polydb://admin@" + ts.addr
	pw := client.WithPassword("secret")

	if _, err := client.Dial(ctx, url, pw); err == nil || !strings.Contains(err.Error(), "no way to trust") {
		t.Errorf("no trust option: %v", err)
	}
	if _, err := client.Dial(ctx, url, pw, client.WithServerFingerprint(strings.Repeat("0", 64))); err == nil {
		t.Error("wrong pinned fingerprint accepted")
	}
	if _, err := client.Dial(ctx, url, client.WithPassword("wrong"), client.WithServerFingerprint(ts.fingerprint)); polydb.CodeOf(err) != "auth_failed" {
		t.Errorf("wrong password: %v", err)
	}

	// Trust on first use, then refuse a different identity.
	known := filepath.Join(t.TempDir(), "known-peers")
	var asked string
	db, err := client.Dial(ctx, url, pw, client.WithKnownPeers(known),
		client.WithConfirmPeer(func(addr, fp string) bool { asked = fp; return true }))
	if err != nil || asked != ts.fingerprint {
		t.Fatalf("first use: %v (asked %q)", err, asked)
	}
	db.Close()
	asked = ""
	db, err = client.Dial(ctx, url, pw, client.WithKnownPeers(known),
		client.WithConfirmPeer(func(addr, fp string) bool { asked = fp; return true }))
	if err != nil || asked != "" {
		t.Errorf("second use: %v (asked again: %v)", err, asked != "")
	}
	db.Close()
	data, _ := os.ReadFile(known)
	os.WriteFile(known, []byte(strings.Replace(string(data), ts.fingerprint, strings.Repeat("a", 64), 1)), 0o600)
	if _, err := client.Dial(ctx, url, pw, client.WithKnownPeers(known)); err == nil || !strings.Contains(err.Error(), "impostor") {
		t.Errorf("changed identity: %v", err)
	}

	// KEY login with a registered 9auth identity.
	id, _ := auth9.LoadFrom(filepath.Join(t.TempDir(), "me"))
	ts.eng.Security().AddKey("admin", id.Fingerprint(), "test")
	db, err = client.Dial(ctx, url, client.WithIdentity(id), client.WithServerFingerprint(ts.fingerprint))
	if err != nil {
		t.Fatalf("key login: %v", err)
	}
	if c := db.(*client.Conn); c.User() != "admin" {
		t.Errorf("key login user %s", c.User())
	}
	db.Close()

	// An identity that isn't registered falls back to the password.
	stranger, _ := auth9.LoadFrom(filepath.Join(t.TempDir(), "stranger"))
	db, err = client.Dial(ctx, url, client.WithIdentity(stranger), pw, client.WithServerFingerprint(ts.fingerprint))
	if err != nil {
		t.Fatalf("unregistered key with a password: %v", err)
	}
	db.Close()

	// PEER login on the Unix socket.
	db, err = client.Dial(ctx, "polydb+unix://"+ts.sock+"?user=admin")
	if err != nil {
		t.Fatalf("peer login: %v", err)
	}
	if _, err := db.Exec(ctx, `show databases`, nil); err != nil {
		t.Error(err)
	}
	db.Close()
}

func TestBlobsAndCancel(t *testing.T) {
	ctx := context.Background()
	ts := startServer(t)
	db := ts.dial(t, "", client.WithInlineLimit(1024), client.WithCompression())
	long := strings.Repeat("abcdefghij", 1000)
	if _, err := db.Exec(ctx, `create database d; use d; define kind Doc { body: text, data: bytes? };
		create Doc { body: $b, data: $d }`, polydb.Params{"b": long, "d": []byte(long)}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(ctx, `find Doc x | show x.body, x.body as again, x.data, x`, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows.Next()
	if _, ok := rows.Values()[0].(*polydb.Blob); !ok {
		t.Fatalf("not a blob: %T", rows.Values()[0])
	}
	var body string
	var blob *polydb.Blob
	var data []byte
	var e *polydb.Entity
	if err := rows.Scan(&body, &blob, &data, &e); err != nil {
		t.Fatal(err)
	}
	if body != long || string(data) != long || blob.Length != int64(len(long)) {
		t.Errorf("blob values: %d %d %d", len(body), len(data), blob.Length)
	}
	part := make([]byte, 5)
	if n, err := blob.ReadAt(part, 9995); n != 5 || string(part) != "fghij" || err != nil {
		t.Errorf("ReadAt: %d %q %v", n, part, err)
	}
	if _, ok := e.Value("body").(*polydb.Blob); !ok {
		t.Error("entity field isn't a blob")
	}
	rows.Close()

	// Cancelling the context cancels a waiting pop.
	db.Exec(ctx, `define kind W { jobs: queue.fifo<text> }; create W { }`, nil)
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	began := time.Now()
	_, err = db.Exec(cctx, `find W w | pop w.jobs wait 10s`, nil)
	if err == nil || time.Since(began) > 3*time.Second {
		t.Errorf("cancel: %v after %v", err, time.Since(began))
	}
	// The connection still works.
	if _, err := db.Exec(ctx, `find W w`, nil); err != nil {
		t.Errorf("after cancel: %v", err)
	}
}

func TestUploads(t *testing.T) {
	ctx := context.Background()
	ts := startServer(t)
	db := ts.dial(t, "")
	c := db.(*client.Conn)
	if _, err := c.Upload(ctx, strings.NewReader("abc"), "bytes", ""); polydb.CodeOf(err) != "not_found" {
		t.Fatalf("upload without a database: %v", err)
	}
	if _, err := db.Exec(ctx, `create database d; use d; define kind F { data: bytes?, body: text?, pic: image? }`, nil); err != nil {
		t.Fatal(err)
	}
	// Bigger than the server's in-memory pieces and the blob chunk size.
	data := make([]byte, 3<<20+77)
	for i := range data {
		data[i] = byte(i * 7 / 3)
	}
	up, err := c.Upload(ctx, bytes.NewReader(data), "bytes", "application/x-test")
	if err != nil {
		t.Fatal(err)
	}
	if up.Length != int64(len(data)) || up.MediaType != "application/x-test" {
		t.Fatalf("upload: %d %q", up.Length, up.MediaType)
	}
	// An upload reads back before it is used.
	part := make([]byte, 4)
	if _, err := up.ReadAt(part, 2<<20); err != nil || !bytes.Equal(part, data[2<<20:2<<20+4]) {
		t.Fatalf("reading the upload: %v", err)
	}
	if _, err := db.Exec(ctx, `create F { data: $d }`, polydb.Params{"d": up}); err != nil {
		t.Fatal(err)
	}
	var got *polydb.Blob
	rows, err := db.Query(ctx, `find F f | show f.data`, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows.Next()
	if err := rows.Scan(&got); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	all, err := got.Bytes(ctx)
	if err != nil || !bytes.Equal(all, data) || got.MediaType != "application/x-test" {
		t.Fatalf("stored value: %d bytes, %q, %v", len(all), got.MediaType, err)
	}
	// A blob from a result goes back as a parameter by its token.
	if _, err := db.Exec(ctx, `create F { data: $d }`, polydb.Params{"d": got}); err != nil {
		t.Fatal(err)
	}
	var n int64
	rows, err = db.Query(ctx, `find F f | where len(f.data) = $n | show count()`, polydb.Params{"n": len(data)})
	if err != nil {
		t.Fatal(err)
	}
	rows.Next()
	rows.Scan(&n)
	rows.Close()
	if n != 2 {
		t.Fatalf("%d copies", n)
	}

	// Text uploads are checked when used.
	txtUp, err := c.Upload(ctx, strings.NewReader(strings.Repeat("é", 20000)), "text", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `create F { body: $b }`, polydb.Params{"b": txtUp}); err != nil {
		t.Fatal(err)
	}
	bad, err := c.Upload(ctx, bytes.NewReader([]byte{0xff, 0xfe}), "text", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `create F { body: $b }`, polydb.Params{"b": bad}); polydb.CodeOf(err) != "type" {
		t.Fatalf("invalid text: %v", err)
	}
	notImage, _ := c.Upload(ctx, strings.NewReader("not an image"), "image", "")
	if _, err := db.Exec(ctx, `create F { pic: $p }`, polydb.Params{"p": notImage}); polydb.CodeOf(err) != "type" {
		t.Fatalf("invalid image: %v", err)
	}
	if _, err := c.Upload(ctx, strings.NewReader("x"), "list", ""); polydb.CodeOf(err) != "type" {
		t.Fatalf("upload of a list: %v", err)
	}
	// A token the server doesn't know.
	fake := polydb.NewLeasedBlob("bytes", 1, "", []byte("nope"), nil)
	if _, err := db.Exec(ctx, `create F { data: $d }`, polydb.Params{"d": fake}); polydb.CodeOf(err) != "lease_expired" {
		t.Fatalf("unknown token: %v", err)
	}
	// A reader that fails cancels the upload; the connection carries on.
	if _, err := c.Upload(ctx, io.MultiReader(bytes.NewReader(data[:1<<20]), iotest.ErrReader(errors.New("disk gone"))), "bytes", ""); err == nil {
		t.Fatal("failed reader uploaded")
	}
	if _, err := db.Exec(ctx, `find F f | show count()`, nil); err != nil {
		t.Fatalf("after a failed upload: %v", err)
	}
}
