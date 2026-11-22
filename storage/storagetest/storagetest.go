// Package storagetest holds conformance tests that every storage.Store
// implementation must pass.
package storagetest

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"

	"github.com/sandgorgon/polydb/storage"
)

// Run runs the conformance tests. newStore must return a new, empty store
// each time it is called.
func Run(t *testing.T, newStore func(t *testing.T) storage.Store) {
	tests := []struct {
		name string
		fn   func(*testing.T, storage.Store)
	}{
		{"Basic", testBasic},
		{"Seq", testSeq},
		{"Iterate", testIterate},
		{"DeleteRange", testDeleteRange},
		{"Snapshot", testSnapshot},
		{"BufferReuse", testBufferReuse},
		{"Close", testClose},
		{"Atomic", testAtomic},
		{"Random", testRandom},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			defer s.Close()
			tc.fn(t, s)
		})
	}
}

func apply(t *testing.T, s storage.Store, fn func(b *storage.Batch)) uint64 {
	t.Helper()
	var b storage.Batch
	fn(&b)
	seq, err := s.Apply(&b)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return seq
}

func set(t *testing.T, s storage.Store, kv ...string) {
	t.Helper()
	apply(t, s, func(b *storage.Batch) {
		for i := 0; i < len(kv); i += 2 {
			b.Set([]byte(kv[i]), []byte(kv[i+1]))
		}
	})
}

func wantGet(t *testing.T, r storage.Reader, key, want string, wantOK bool) {
	t.Helper()
	v, ok, err := r.Get([]byte(key))
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	if ok != wantOK || string(v) != want {
		t.Fatalf("Get(%q) = %q, %v; want %q, %v", key, v, ok, want, wantOK)
	}
}

// scan returns "k=v" entries from an iterator.
func scan(t *testing.T, r storage.Reader, opts storage.IterOptions) []string {
	t.Helper()
	it := r.Iter(opts)
	defer it.Close()
	var out []string
	for it.Next() {
		out = append(out, string(it.Key())+"="+string(it.Value()))
	}
	if err := it.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	return out
}

func testBasic(t *testing.T, s storage.Store) {
	wantGet(t, s, "a", "", false)
	set(t, s, "a", "1", "b", "2", "", "empty key", "e", "")
	wantGet(t, s, "a", "1", true)
	wantGet(t, s, "", "empty key", true)
	wantGet(t, s, "e", "", true) // an empty value exists
	set(t, s, "a", "3")
	wantGet(t, s, "a", "3", true)
	apply(t, s, func(b *storage.Batch) {
		b.Delete([]byte("a"))
		b.Delete([]byte("missing"))
	})
	wantGet(t, s, "a", "", false)
	// Operations in a batch apply in order.
	apply(t, s, func(b *storage.Batch) {
		b.Set([]byte("x"), []byte("1"))
		b.Delete([]byte("x"))
		b.Set([]byte("y"), []byte("1"))
		b.Set([]byte("y"), []byte("2"))
	})
	wantGet(t, s, "x", "", false)
	wantGet(t, s, "y", "2", true)
}

func testSeq(t *testing.T, s storage.Store) {
	if s.Seq() != 0 {
		t.Fatalf("new store Seq = %d", s.Seq())
	}
	for want := uint64(1); want <= 3; want++ {
		if seq := apply(t, s, func(*storage.Batch) {}); seq != want || s.Seq() != want {
			t.Fatalf("seq %d, Seq() %d, want %d", seq, s.Seq(), want)
		}
	}
}

func testIterate(t *testing.T, s storage.Store) {
	set(t, s, "a", "1", "b", "2", "ba", "3", "bb", "4", "c", "5", "\xff", "6", "\xff\xff", "7")
	all := []string{"a=1", "b=2", "ba=3", "bb=4", "c=5", "\xff=6", "\xff\xff=7"}
	b := func(s string) []byte { return []byte(s) }
	cases := []struct {
		lower, upper []byte
		want         []string
	}{
		{nil, nil, all},
		{b("b"), nil, all[1:]},
		{nil, b("c"), all[:4]},
		{b("b"), b("bb"), all[1:3]},
		{b("az"), b("bz"), all[1:4]},
		{b("b"), storage.PrefixEnd(b("b")), all[1:4]},
		{b("\xff"), storage.PrefixEnd(b("\xff")), all[5:]},
		{b("d"), b("e"), nil},
		{b("c"), b("c"), nil},
		{b("c"), b("b"), nil},
		{b(""), b(""), nil},
	}
	for _, c := range cases {
		got := scan(t, s, storage.IterOptions{Lower: c.lower, Upper: c.upper})
		if !slices.Equal(got, c.want) {
			t.Errorf("[%q, %q): got %q, want %q", c.lower, c.upper, got, c.want)
		}
		rev := slices.Clone(c.want)
		slices.Reverse(rev)
		got = scan(t, s, storage.IterOptions{Lower: c.lower, Upper: c.upper, Reverse: true})
		if !slices.Equal(got, rev) {
			t.Errorf("reverse [%q, %q): got %q, want %q", c.lower, c.upper, got, rev)
		}
	}
	it := s.Iter(storage.IterOptions{})
	it.Close()
	if it.Next() {
		t.Error("Next after Close returned true")
	}
}

func testDeleteRange(t *testing.T, s storage.Store) {
	set(t, s, "a", "1", "b", "2", "ba", "3", "c", "4", "d", "5")
	apply(t, s, func(b *storage.Batch) {
		b.DeleteRange([]byte("b"), []byte("c"))
		b.Set([]byte("bz"), []byte("new")) // after the range delete, so kept
	})
	if got := scan(t, s, storage.IterOptions{}); !slices.Equal(got, []string{"a=1", "bz=new", "c=4", "d=5"}) {
		t.Errorf("got %q", got)
	}
	apply(t, s, func(b *storage.Batch) { b.DeleteRange([]byte("c"), nil) })
	if got := scan(t, s, storage.IterOptions{}); !slices.Equal(got, []string{"a=1", "bz=new"}) {
		t.Errorf("got %q", got)
	}
}

func testSnapshot(t *testing.T, s storage.Store) {
	set(t, s, "a", "1", "b", "2")
	snap := s.Snapshot()
	set(t, s, "a", "changed", "c", "3")
	apply(t, s, func(b *storage.Batch) { b.Delete([]byte("b")) })

	if snap.Seq() != 1 {
		t.Errorf("snapshot Seq = %d, want 1", snap.Seq())
	}
	wantGet(t, snap, "a", "1", true)
	wantGet(t, snap, "b", "2", true)
	wantGet(t, snap, "c", "", false)
	if got := scan(t, snap, storage.IterOptions{}); !slices.Equal(got, []string{"a=1", "b=2"}) {
		t.Errorf("snapshot scan %q", got)
	}
	wantGet(t, s, "a", "changed", true)

	snap.Release()
	if _, _, err := snap.Get([]byte("a")); !errors.Is(err, storage.ErrReleased) {
		t.Errorf("Get after Release: %v", err)
	}
}

func testBufferReuse(t *testing.T, s storage.Store) {
	k, v := []byte("key"), []byte("val")
	var b storage.Batch
	b.Set(k, v)
	copy(k, "xxx")
	copy(v, "yyy")
	if _, err := s.Apply(&b); err != nil {
		t.Fatal(err)
	}
	wantGet(t, s, "key", "val", true)
	b.Reset()
	if b.Len() != 0 || b.Size() != 0 {
		t.Error("Reset didn't empty the batch")
	}
}

func testClose(t *testing.T, s storage.Store) {
	set(t, s, "a", "1")
	snap := s.Snapshot()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var b storage.Batch
	b.Set([]byte("b"), nil)
	if _, err := s.Apply(&b); !errors.Is(err, storage.ErrClosed) {
		t.Errorf("Apply after Close: %v", err)
	}
	if _, _, err := s.Get([]byte("a")); !errors.Is(err, storage.ErrClosed) {
		t.Errorf("Get after Close: %v", err)
	}
	if it := s.Iter(storage.IterOptions{}); it.Next() || !errors.Is(it.Err(), storage.ErrClosed) {
		t.Errorf("Iter after Close: %v", it.Err())
	}
	snap.Release()
}

// testAtomic checks that concurrent readers never see half a batch. Every
// batch writes the same counter to keys k0..k9; readers check all ten agree.
func testAtomic(t *testing.T, s storage.Store) {
	const keys, rounds = 10, 300
	write := func(n int) {
		var b storage.Batch
		for i := range keys {
			b.Set(fmt.Appendf(nil, "k%d", i), fmt.Appendf(nil, "%06d", n))
		}
		if _, err := s.Apply(&b); err != nil {
			t.Error(err)
		}
	}
	write(0)
	var wg sync.WaitGroup
	done := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				snap := s.Snapshot()
				vals := scan(t, snap, storage.IterOptions{})
				snap.Release()
				for _, v := range vals[1:] {
					if v[3:] != vals[0][3:] {
						t.Errorf("torn batch: %q", vals)
						return
					}
				}
			}
		})
	}
	for n := 1; n <= rounds; n++ {
		write(n)
	}
	close(done)
	wg.Wait()
}

// testRandom applies random batches to the store and to a map model, and
// checks gets, bounded scans and old snapshots against the model.
func testRandom(t *testing.T, s storage.Store) {
	rng := rand.New(rand.NewPCG(1, 2))
	key := func() []byte { return fmt.Appendf(nil, "%03d", rng.IntN(600)) }
	model := map[string]string{}
	type saved struct {
		snap  storage.Snapshot
		model map[string]string
	}
	var snaps []saved

	check := func(r storage.Reader, m map[string]string) {
		t.Helper()
		keys := slices.Sorted(maps.Keys(m))
		for range 5 {
			lo, hi := key(), key()
			if bytes.Compare(lo, hi) > 0 {
				lo, hi = hi, lo
			}
			opts := storage.IterOptions{Lower: lo, Upper: hi, Reverse: rng.IntN(2) == 0}
			switch rng.IntN(4) {
			case 0:
				opts.Lower = nil
			case 1:
				opts.Upper = nil
			}
			var want []string
			for _, k := range keys {
				if storage.InRange([]byte(k), opts.Lower, opts.Upper) {
					want = append(want, k+"="+m[k])
				}
			}
			if opts.Reverse {
				slices.Reverse(want)
			}
			if got := scan(t, r, opts); !slices.Equal(got, want) {
				t.Fatalf("scan %+v:\ngot  %q\nwant %q", opts, got, want)
			}
		}
		for range 20 {
			k := key()
			v, ok := m[string(k)]
			wantGet(t, r, string(k), v, ok)
		}
	}

	for round := range 400 {
		var b storage.Batch
		for range rng.IntN(40) + 1 {
			k := key()
			switch r := rng.IntN(20); {
			case r < 12:
				v := fmt.Sprintf("v%d", round)
				b.Set(k, []byte(v))
				model[string(k)] = v
			case r < 19:
				b.Delete(k)
				delete(model, string(k))
			default:
				end := fmt.Appendf(nil, "%03d", rng.IntN(600))
				b.DeleteRange(k, end)
				for mk := range model {
					if storage.InRange([]byte(mk), k, end) {
						delete(model, mk)
					}
				}
			}
		}
		if _, err := s.Apply(&b); err != nil {
			t.Fatal(err)
		}
		check(s, model)
		if round%25 == 0 {
			snaps = append(snaps, saved{s.Snapshot(), maps.Clone(model)})
		}
	}
	for _, sv := range snaps {
		check(sv.snap, sv.model)
		sv.snap.Release()
	}
}
