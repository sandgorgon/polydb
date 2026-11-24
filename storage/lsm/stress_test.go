package lsm

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/storage"
	"github.com/sandgorgon/polydb/storage/vfs"
)

// TestConcurrentReadsDuringCompaction has writers rewrite some keys
// while flushes and compactions run, and readers check, in snapshots
// and in the live store, that keys nobody changes are always there.
func TestConcurrentReadsDuringCompaction(t *testing.T) {
	s := openOpts(t, tiny(vfs.NewMem()))
	defer s.Close()
	const n = 300
	for i := range n {
		var b storage.Batch
		b.Set(fmt.Appendf(nil, "e%04d/age", i), []byte("0"))
		b.Set(fmt.Appendf(nil, "e%04d/name", i), fmt.Appendf(nil, "name%d", i))
		if _, err := s.Apply(&b); err != nil {
			t.Fatal(err)
		}
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w), 1))
			for !stop.Load() {
				var b storage.Batch
				i := rng.IntN(n)
				b.Set(fmt.Appendf(nil, "e%04d/age", i), fmt.Appendf(nil, "%d", rng.IntN(1000)))
				if _, err := s.Apply(&b); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	var failures atomic.Int64
	for r := range 4 {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(r), 2))
			for !stop.Load() {
				i := rng.IntN(n)
				var rd storage.Reader = s
				var snap storage.Snapshot
				if rng.IntN(2) == 0 {
					snap = s.Snapshot()
					rd = snap
				}
				prefix := fmt.Appendf(nil, "e%04d/", i)
				it := rd.Iter(storage.IterOptions{Lower: prefix, Upper: storage.PrefixEnd(prefix)})
				var got []string
				for it.Next() {
					got = append(got, string(it.Key()))
				}
				err := it.Err()
				it.Close()
				if v, ok, gerr := rd.Get(fmt.Appendf(nil, "e%04d/name", i)); gerr != nil || !ok || string(v) != fmt.Sprintf("name%d", i) {
					if failures.Add(1) <= 3 {
						t.Errorf("Get e%04d/name: %q %v %v (snapshot %v)", i, v, ok, gerr, snap != nil)
					}
				}
				if snap != nil {
					snap.Release()
				}
				if err != nil || len(got) != 2 {
					if failures.Add(1) <= 3 {
						t.Errorf("scan e%04d/: %v %v (snapshot %v)", i, got, err, snap != nil)
					}
				}
			}
		})
	}
	time.Sleep(3 * time.Second)
	stop.Store(true)
	wg.Wait()
	t.Logf("%+v", s.Metrics())
	if f := failures.Load(); f > 0 {
		t.Errorf("%d failed reads", f)
	}
}
