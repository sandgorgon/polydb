package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	auth9 "github.com/sandgorgon/9auth"
	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/security"
	"github.com/sandgorgon/polydb/server"
	"github.com/sandgorgon/polydb/storage/wal"
)

// serveConfig is what serve needs, parsed from flags.
type serveConfig struct {
	listen      string
	socket      string
	dataDir     string
	insecure    bool
	tlsCert     string
	tlsKey      string
	durability  string
	compression bool
	inlineLimit int64
	maxLoad     int64
	backupDir   string
	verbose     bool
	iterations  int
}

func serveCmd(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(errOut)
	c := serveConfig{}
	fs.StringVar(&c.listen, "listen", ":7377", "TCP address to listen on (empty: no TCP)")
	fs.StringVar(&c.dataDir, "data-dir", dataDir(), "directory for the server's data, identity and socket")
	fs.StringVar(&c.socket, "socket", "", "Unix socket path (default: <data-dir>/polydb.sock; \"none\" disables it)")
	fs.BoolVar(&c.insecure, "insecure", false, "serve TCP without TLS, from loopback addresses only (development)")
	fs.StringVar(&c.tlsCert, "tls-cert", "", "certificate file for CA mode (default: the server's 9auth identity)")
	fs.StringVar(&c.tlsKey, "tls-key", "", "key file for CA mode")
	fs.StringVar(&c.durability, "durability", "durable", "when commits are acknowledged: durable (on stable storage) or relaxed (written; synced in the background, for bulk loads)")
	fs.BoolVar(&c.compression, "compression", false, "allow deflate compression")
	fs.Int64Var(&c.inlineLimit, "inline-limit", 0, "largest value sent inline, in bytes (default 16384)")
	fs.Int64Var(&c.maxLoad, "max-load", 0, "bytes of large values one statement may load into memory, by default (default 268435456; -1 for no limit; sessions change it with set max_load)")
	fs.StringVar(&c.backupDir, "backup-dir", "", "directory that backup and restore statements write and read (default: <data-dir>/backups)")
	fs.BoolVar(&c.verbose, "v", false, "log connections")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(errOut, "polydb serve: unexpected arguments:", fs.Args())
		return 2
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, c, out, errOut, nil); err != nil {
		fmt.Fprintln(errOut, "polydb serve:", err)
		return 1
	}
	return 0
}

// serve runs the server until ctx ends. ready, if set, receives the
// listening addresses (for tests).
func serve(ctx context.Context, c serveConfig, out, errOut io.Writer, ready func(tcp, unix string)) error {
	if err := os.MkdirAll(c.dataDir, 0o700); err != nil {
		return err
	}
	level := slog.LevelWarn
	if c.verbose {
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewTextHandler(errOut, &slog.HandlerOptions{Level: level}))
	var durability wal.Durability
	switch c.durability {
	case "", "durable":
	case "relaxed":
		durability = wal.Relaxed
	default:
		return fmt.Errorf("--durability is durable or relaxed, not %q", c.durability)
	}
	eng, err := engine.Open(engine.Options{Dir: c.dataDir, Durability: durability, PasswordIterations: c.iterations,
		MaxLoad: c.maxLoad, BackupDir: c.backupDir})
	if err != nil {
		return err
	}
	defer eng.Close()
	// Until polydb init gives admin a password or key, only the OS user
	// running the server can log in, over the Unix socket (spec/protocol.md
	// §3.3).
	if u, _ := eng.Security().User(security.AdminUser); u.Creds == nil && len(u.Keys) == 0 {
		fmt.Fprintln(out, "not initialized: only this OS user can log in, over the Unix socket (run polydb init)")
	}
	if durability == wal.Relaxed {
		fmt.Fprintln(out, "durability relaxed: a crash can lose the last moments of commits")
	}

	cfg := server.Config{Engine: eng, Logger: logger, Compression: c.compression, InlineLimit: c.inlineLimit,
		PeerUsers: map[string]string{}}
	if me := osUser(); me != "" {
		cfg.PeerUsers[me] = security.AdminUser // the OS user running the server is admin over the socket
	}
	switch {
	case c.tlsCert != "" || c.tlsKey != "":
		if cfg.TLS, err = server.CATLS(c.tlsCert, c.tlsKey); err != nil {
			return err
		}
	case !c.insecure && c.listen != "":
		id, err := auth9.LoadFrom(filepath.Join(c.dataDir, "identity"))
		if err != nil {
			return err
		}
		cfg.TLS = server.IdentityTLS(id)
		fmt.Fprintf(out, "server fingerprint: %s\n", id.Fingerprint())
	}
	srv, err := server.New(cfg)
	if err != nil {
		return err
	}

	errc := make(chan error, 2)
	var tcpAddr, unixAddr string
	if c.listen != "" {
		l, err := net.Listen("tcp", c.listen)
		if err != nil {
			return err
		}
		tcpAddr = l.Addr().String()
		mode := "TLS"
		if cfg.TLS == nil {
			mode = "no TLS, loopback only"
		}
		fmt.Fprintf(out, "listening on %s (%s)\n", tcpAddr, mode)
		go func() { errc <- srv.Serve(l) }()
	}
	if c.socket != "none" {
		path := c.socket
		if path == "" {
			path = filepath.Join(c.dataDir, "polydb.sock")
		}
		if len(path) > 100 {
			return fmt.Errorf("the socket path %s is %d bytes, longer than Unix sockets allow (about 100); give a shorter --socket", path, len(path))
		}
		os.Remove(path) // a socket left by a server that didn't stop cleanly
		l, err := net.Listen("unix", path)
		if err != nil {
			return err
		}
		os.Chmod(path, 0o660)
		unixAddr = path
		fmt.Fprintf(out, "listening on %s\n", path)
		go func() { errc <- srv.Serve(l) }()
	}
	if tcpAddr == "" && unixAddr == "" {
		return errors.New("nothing to listen on: give --listen or --socket")
	}
	if ready != nil {
		ready(tcpAddr, unixAddr)
	}
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, server.ErrServerClosed) {
			return err
		}
	}
	fmt.Fprintln(out, "shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	if unixAddr != "" {
		os.Remove(unixAddr)
	}
	return nil
}
