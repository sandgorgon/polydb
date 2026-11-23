package lsm

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/storage"
	"github.com/sandgorgon/polydb/storage/vfs"
	"github.com/sandgorgon/polydb/storage/wal"
)

// fingerprint is a store's whole visible content.
func fingerprint(t *testing.T, r storage.Reader) string {
	t.Helper()
	it := r.Iter(storage.IterOptions{})
	defer it.Close()
	var b strings.Builder
	for it.Next() {
		fmt.Fprintf(&b, "%s=%s ", it.Key(), it.Value())
	}
	if err := it.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return b.String()
}

func (m model) fingerprint() string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(m)) {
		fmt.Fprintf(&b, "%s=%s ", k, m[k])
	}
	return b.String()
}

// randomBatch is a batch of sets, deletes and the odd range deletion over
// 100 keys.
func randomBatch(rng *rand.Rand, n int) *storage.Batch {
	var b storage.Batch
	for range 1 + rng.IntN(4) {
		k := fmt.Sprintf("k%02d", rng.IntN(100))
		switch x := rng.IntN(100); {
		case x < 70:
			b.Set([]byte(k), fmt.Appendf(nil, "%d%s", n, strings.Repeat(".", rng.IntN(40))))
		case x < 95:
			b.Delete([]byte(k))
		default:
			b.DeleteRange([]byte(k), fmt.Appendf(nil, "k%02d", rng.IntN(100)+5))
		}
	}
	return &b
}

// closeWithin closes a store, failing the test if Close hangs.
func closeWithin(t *testing.T, s *Store, what string) {
	t.Helper()
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("%s: Close hung", what)
	}
}

// runFaulty runs batches on a store over fs until one fails, and returns
// the state after each successful batch (states[0] is empty) and how
// many were acknowledged.
func runFaulty(t *testing.T, fs vfs.FS, durability wal.Durability, seed uint64, batches int) (states []string, acked int) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, 1))
	o := tiny(fs)
	o.Durability = durability
	m := model{}
	states = []string{""}
	s, err := Open(o)
	if err != nil {
		return states, 0
	}
	for i := range batches {
		b := randomBatch(rng, i)
		m.apply(b)
		states = append(states, m.fingerprint())
		if _, err := s.Apply(b); err != nil {
			break
		}
		acked++
		if i%97 == 0 {
			s.Flush() // errors show up in the next Apply
		}
	}
	closeWithin(t, s, "after the workload")
	return states, acked
}

// TestFaultInjection fails each sampled file-system operation, once or
// for good, then crashes. Recovery must give the state after the last
// acknowledged batch, or after the one that failed (its data may have
// reached the disk); in relaxed mode, the state after some earlier
// batch, never one with a hole.
func TestFaultInjection(t *testing.T) {
	for _, mode := range []struct {
		name       string
		durability wal.Durability
	}{{"durable", wal.Durable}, {"relaxed", wal.Relaxed}} {
		t.Run(mode.name, func(t *testing.T) {
			const batches = 300
			dry := vfs.NewFaulty(vfs.NewMem())
			runFaulty(t, dry, mode.durability, 1, batches)
			total := dry.Ops()
			rng := rand.New(rand.NewPCG(2, 2))
			var points []int64
			for n := int64(1); n <= min(total, 150); n++ {
				points = append(points, n)
			}
			for range 250 {
				points = append(points, 1+rng.Int64N(total))
			}
			t.Logf("%d operations in a clean run; testing %d failure points twice", total, len(points))
			hits := 0
			for _, at := range points {
				for _, sticky := range []bool{false, true} {
					mem := vfs.NewMem()
					fs := vfs.NewFaulty(mem)
					fs.FailAt(at, sticky)
					states, acked := runFaulty(t, fs, mode.durability, 1, batches)
					if len(fs.Failed()) > 0 {
						hits++
					}
					what := fmt.Sprintf("failing operation %d (sticky %v, %v)", at, sticky, fs.Failed())
					crashed := mem.Crash(rand.New(rand.NewPCG(uint64(at), 3)))
					o := tiny(crashed)
					o.Durability = mode.durability
					s, err := Open(o)
					if err != nil {
						t.Fatalf("%s: reopen: %v", what, err)
					}
					got := fingerprint(t, s)
					ok := false
					if mode.durability == wal.Durable {
						ok = got == states[acked] || (acked+1 < len(states) && got == states[acked+1])
					} else {
						ok = slices.Contains(states[:min(acked+2, len(states))], got)
					}
					if !ok {
						t.Fatalf("%s: %d batches acknowledged; recovered state matches no allowed state\n got %.400s\nwant %.400s",
							what, acked, got, states[acked])
					}
					var b storage.Batch
					b.Set([]byte("after"), []byte("recovery"))
					if _, err := s.Apply(&b); err != nil {
						t.Fatalf("%s: write after recovery: %v", what, err)
					}
					closeWithin(t, s, what)
				}
			}
			t.Logf("%d of %d runs reached their failure point", hits, 2*len(points))
			if hits < len(points) {
				t.Errorf("too few runs reached their failure point")
			}
		})
	}
}

// FuzzRecovery turns bytes into a script of writes, flushes, compactions
// and crashes with torn writes, and checks every recovery against a
// model.
func FuzzRecovery(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 7, 4, 5, 6, 7})
	f.Add([]byte{1, 0x10, 0x18, 0x20, 6, 0x28, 0x30, 0x0f, 0x3e, 7, 0x40, 0x48, 0x57})
	f.Fuzz(func(t *testing.T, script []byte) {
		if len(script) == 0 || len(script) > 400 {
			return
		}
		durability := wal.Durable
		if script[0]&1 == 1 {
			durability = wal.Relaxed
		}
		mem := vfs.NewMem()
		o := tiny(mem)
		o.Durability = durability
		s, err := Open(o)
		if err != nil {
			t.Fatal(err)
		}
		m := model{}
		states := []model{{}}
		for i, c := range script[1:] {
			key := []byte(fmt.Sprintf("k%02d", int(c>>3)%32))
			var b storage.Batch
			switch c & 7 {
			case 0, 1, 2, 3:
				b.Set(key, fmt.Appendf(nil, "%d%s", i, strings.Repeat("x", int(c)%50)))
			case 4:
				b.Delete(key)
			case 5:
				b.DeleteRange(key, fmt.Appendf(nil, "k%02d", int(c>>3)%32+5))
			case 6:
				if c>>3&3 == 0 {
					err = s.CompactAll()
				} else {
					err = s.Flush()
				}
				if err != nil {
					t.Fatal(err)
				}
				continue
			case 7:
				crashed := mem.Crash(rand.New(rand.NewPCG(uint64(c), uint64(i))))
				s.Close()
				mem = crashed
				o.FS = mem
				if s, err = Open(o); err != nil {
					t.Fatalf("step %d: reopen: %v", i, err)
				}
				got := fingerprint(t, s)
				if durability == wal.Durable {
					if got != m.fingerprint() {
						t.Fatalf("step %d: after a crash\n got %s\nwant %s", i, got, m.fingerprint())
					}
				} else {
					j := slices.IndexFunc(states, func(st model) bool { return st.fingerprint() == got })
					if j < 0 {
						t.Fatalf("step %d: recovered state is no prefix of the batches: %s", i, got)
					}
					m = maps.Clone(states[j])
				}
				states = []model{maps.Clone(m)}
				continue
			}
			if _, err := s.Apply(&b); err != nil {
				t.Fatal(err)
			}
			m.apply(&b)
			states = append(states, maps.Clone(m))
		}
		if got := fingerprint(t, s); got != m.fingerprint() {
			t.Fatalf("at the end\n got %s\nwant %s", got, m.fingerprint())
		}
		s.Close()
	})
}
