package client_test

import (
	"bytes"
	"context"
	"io"
	"runtime"
	"runtime/metrics"
	"sync"
	"testing"
	"time"

	"github.com/sandgorgon/polydb"
	"github.com/sandgorgon/polydb/client"
	"github.com/sandgorgon/polydb/engine"
)

// BenchmarkLargeValue measures the memory one 64 MiB value costs, client
// and server together: uploading and storing it, fetching it, and
// updating another field of its entity. peak-MiB is the most live heap
// above what was live before, sampled every millisecond.
func BenchmarkLargeValue(b *testing.B) {
	ctx := context.Background()
	// On disk, so that stored content isn't in the heap being measured.
	ts := startServerWith(b, engine.Options{Dir: b.TempDir(), HistorySweep: -1, StatsRefresh: -1})
	db, err := client.Dial(ctx, "polydb://admin@"+ts.addr, client.WithPassword("secret"), client.WithServerFingerprint(ts.fingerprint))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ctx, `create database d; use d; define kind F { name: text, data: bytes? }`, nil); err != nil {
		b.Fatal(err)
	}
	c := db.(*client.Conn)
	data := make([]byte, 64<<20)
	for i := range data {
		data[i] = byte(i * 7 / 3)
	}
	up, err := c.Upload(ctx, bytes.NewReader(data), "bytes", "")
	if err != nil {
		b.Fatal(err)
	}
	if _, err := db.Exec(ctx, `create F { name: "a", data: $d }`, polydb.Params{"d": up}); err != nil {
		b.Fatal(err)
	}
	b.Run("upload+store", func(b *testing.B) {
		defer peakHeap(b)()
		for i := range b.N {
			data[0] = byte(i) // new content each time
			up, err := c.Upload(ctx, bytes.NewReader(data), "bytes", "")
			if err != nil {
				b.Fatal(err)
			}
			if _, err := db.Exec(ctx, `create F { name: "u", data: $d }`, polydb.Params{"d": up}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("fetch", func(b *testing.B) {
		defer peakHeap(b)()
		for range b.N {
			rows, err := db.Query(ctx, `find F f | where f.name = "a" | show f.data`, nil)
			if err != nil {
				b.Fatal(err)
			}
			var blob *polydb.Blob
			rows.Next()
			if err := rows.Scan(&blob); err != nil {
				b.Fatal(err)
			}
			rows.Close()
			if n, err := io.Copy(io.Discard, blob.Reader()); err != nil || n != int64(len(data)) {
				b.Fatal(n, err)
			}
		}
	})
	b.Run("update-other-field", func(b *testing.B) {
		defer peakHeap(b)()
		for range b.N {
			if _, err := db.Exec(ctx, `find F f | where f.name = "a" | update f set name = "a"`, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// peakHeap samples the live heap until the returned function reports the
// peak above the starting point.
func peakHeap(b *testing.B) func() {
	b.ReportAllocs()
	runtime.GC()
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	read := func() uint64 { metrics.Read(sample); return sample[0].Value.Uint64() }
	base, peak := read(), uint64(0)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
				if v := read(); v > peak {
					peak = v
				}
			}
		}
	})
	return func() {
		close(stop)
		wg.Wait()
		b.ReportMetric(float64(max(peak, base)-base)/(1<<20), "peak-MiB")
	}
}
