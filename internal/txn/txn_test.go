package txn

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sandgorgon/polydb/internal/storage"
	"github.com/sandgorgon/polydb/internal/storage/mem"
)

func commit(t *testing.T, tx *Txn) uint64 {
	t.Helper()
	seq, err := tx.Commit()
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	return seq
}

func get(t *testing.T, r interface {
	Get([]byte) ([]byte, bool, error)
}, k string) string {
	t.Helper()
	v, ok, err := r.Get([]byte(k))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return "<none>"
	}
	return string(v)
}

func TestReadYourWritesAndIsolation(t *testing.T) {
	m := NewManager(mem.New())
	setup := m.Begin(false)
	setup.Set([]byte("a"), []byte("1"))
	setup.Set([]byte("b"), []byte("2"))
	commit(t, setup)

	t1 := m.Begin(false)
	t1.Set([]byte("a"), []byte("t1"))
	t1.Delete([]byte("b"))
	if get(t, t1, "a") != "t1" || get(t, t1, "b") != "<none>" {
		t.Error("t1 doesn't see its own writes")
	}
	t2 := m.Begin(false)
	if get(t, t2, "a") != "1" {
		t.Error("t2 sees uncommitted writes")
	}
	commit(t, t1)
	if get(t, t2, "a") != "1" || get(t, t2, "b") != "2" {
		t.Error("t2's snapshot changed after t1 committed")
	}
	t2.Rollback()
	if get(t, m.Store(), "a") != "t1" {
		t.Error("t1's write not applied")
	}
}

func TestWriteWriteConflict(t *testing.T) {
	m := NewManager(mem.New())
	t1, t2 := m.Begin(false), m.Begin(false)
	t1.Set([]byte("x"), []byte("1"))
	t2.Set([]byte("x"), []byte("2"))
	commit(t, t1)
	if _, err := t2.Commit(); !errors.Is(err, ErrConflict) {
		t.Fatalf("second writer: %v", err)
	}
	// Disjoint writes don't conflict.
	t3, t4 := m.Begin(false), m.Begin(false)
	t3.Set([]byte("y"), nil)
	t4.Set([]byte("z"), nil)
	commit(t, t3)
	commit(t, t4)
	// A transaction that began after the commit doesn't conflict with it.
	t5 := m.Begin(false)
	t5.Set([]byte("x"), []byte("3"))
	commit(t, t5)
}

func TestRangeDeleteConflicts(t *testing.T) {
	m := NewManager(mem.New())
	t1, t2 := m.Begin(false), m.Begin(false)
	t1.DeleteRange([]byte("a"), []byte("m"))
	t2.Set([]byte("f"), nil)
	commit(t, t1)
	if _, err := t2.Commit(); !errors.Is(err, ErrConflict) {
		t.Errorf("write into deleted range: %v", err)
	}
	t3, t4 := m.Begin(false), m.Begin(false)
	t3.Set([]byte("f"), nil)
	t4.DeleteRange([]byte("a"), nil)
	commit(t, t3)
	if _, err := t4.Commit(); !errors.Is(err, ErrConflict) {
		t.Errorf("range over written key: %v", err)
	}
}

// TestGuardPreventsWriteSkew: two transactions each check that no "slot"
// key exists and then claim one under a different key. Without guards both
// would commit.
func TestGuardPreventsWriteSkew(t *testing.T) {
	m := NewManager(mem.New())
	claim := func(name string) *Txn {
		tx := m.Begin(false)
		prefix := []byte("slot/")
		it := tx.Iter(storage.IterOptions{Lower: prefix, Upper: storage.PrefixEnd(prefix)})
		if it.Next() {
			t.Fatal("slot already taken")
		}
		it.Close()
		tx.Guard(prefix, storage.PrefixEnd(prefix))
		tx.Set([]byte("slot/"+name), nil)
		return tx
	}
	a, b := claim("a"), claim("b")
	commit(t, a)
	if _, err := b.Commit(); !errors.Is(err, ErrConflict) {
		t.Fatalf("write skew allowed: %v", err)
	}
	// A guarded key written by an earlier commit doesn't matter.
	c := m.Begin(false)
	c.GuardKey([]byte("slot/a"))
	c.Set([]byte("other"), nil)
	commit(t, c)
}

func TestReadOnlyAndDone(t *testing.T) {
	m := NewManager(mem.New())
	ro := m.Begin(true)
	ro.Set([]byte("x"), nil)
	if _, err := ro.Commit(); !errors.Is(err, ErrReadOnly) {
		t.Errorf("read-only write: %v", err)
	}
	tx := m.Begin(false)
	commit(t, tx)
	if _, err := tx.Commit(); !errors.Is(err, ErrDone) {
		t.Errorf("double commit: %v", err)
	}
	if _, _, err := tx.Get([]byte("x")); !errors.Is(err, ErrDone) {
		t.Errorf("get after commit: %v", err)
	}
	tx.Rollback() // no-op
}

func TestCommitHookAndLogPruning(t *testing.T) {
	m := NewManager(mem.New())
	var got [][]byte
	m.OnCommit(func(_ uint64, keys [][]byte) { got = append(got, keys...) })
	tx := m.Begin(false)
	tx.Set([]byte("b"), nil)
	tx.Set([]byte("a"), nil)
	commit(t, tx)
	if len(got) != 2 || string(got[0]) != "a" {
		t.Errorf("hook keys %q", got)
	}
	if len(m.log) != 0 {
		t.Errorf("log not pruned with no active transactions: %d", len(m.log))
	}
	old := m.Begin(false)
	for range 3 {
		tx := m.Begin(false)
		tx.Set([]byte("k"), nil)
		commit(t, tx)
	}
	if len(m.log) != 3 {
		t.Errorf("log should keep commits an active transaction may conflict with: %d", len(m.log))
	}
	old.Rollback()
	if len(m.log) != 0 {
		t.Errorf("log not pruned after rollback: %d", len(m.log))
	}
}

// TestMergedIteration checks Get and Iter inside a transaction against a
// map model, with random sets, deletes and range deletes on top of a
// committed snapshot.
func TestMergedIteration(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	key := func() []byte { return fmt.Appendf(nil, "%02d", rng.IntN(60)) }
	for round := range 50 {
		m := NewManager(mem.New())
		model := map[string]string{}
		setup := m.Begin(false)
		for range 30 {
			k := key()
			setup.Set(k, []byte("s"))
			model[string(k)] = "s"
		}
		commit(t, setup)
		tx := m.Begin(false)
		for i := range 40 {
			k := key()
			switch rng.IntN(6) {
			case 0, 1, 2:
				v := fmt.Sprint("t", i)
				tx.Set(k, []byte(v))
				model[string(k)] = v
			case 3, 4:
				tx.Delete(k)
				delete(model, string(k))
			default:
				hi := key()
				if bytes.Compare(k, hi) > 0 {
					k, hi = hi, k
				}
				tx.DeleteRange(k, hi)
				for mk := range model {
					if storage.InRange([]byte(mk), k, hi) {
						delete(model, mk)
					}
				}
			}
		}
		keys := slices.Sorted(maps.Keys(model))
		for range 10 {
			lo, hi := key(), key()
			if bytes.Compare(lo, hi) > 0 {
				lo, hi = hi, lo
			}
			opts := storage.IterOptions{Lower: lo, Upper: hi, Reverse: rng.IntN(2) == 0}
			if rng.IntN(3) == 0 {
				opts.Lower = nil
			}
			var want, got []string
			for _, k := range keys {
				if storage.InRange([]byte(k), opts.Lower, opts.Upper) {
					want = append(want, k+"="+model[k])
				}
			}
			if opts.Reverse {
				slices.Reverse(want)
			}
			it := tx.Iter(opts)
			for it.Next() {
				got = append(got, string(it.Key())+"="+string(it.Value()))
			}
			it.Close()
			if !slices.Equal(got, want) {
				t.Fatalf("round %d %+v:\ngot  %q\nwant %q", round, opts, got, want)
			}
		}
		for k := range 60 {
			ks := fmt.Sprintf("%02d", k)
			want, ok := model[ks]
			if !ok {
				want = "<none>"
			}
			if g := get(t, tx, ks); g != want {
				t.Fatalf("Get(%s) = %s, want %s", ks, g, want)
			}
		}
		// After commit the store matches the model.
		commit(t, tx)
		for k := range 60 {
			ks := fmt.Sprintf("%02d", k)
			want, ok := model[ks]
			if !ok {
				want = "<none>"
			}
			if g := get(t, m.Store(), ks); g != want {
				t.Fatalf("after commit Get(%s) = %s, want %s", ks, g, want)
			}
		}
	}
}

// TestConcurrentCounter increments one counter from many goroutines with
// retries; snapshot isolation plus write-write conflicts must not lose
// updates.
func TestConcurrentCounter(t *testing.T) {
	m := NewManager(mem.New())
	var conflicts atomic.Int64
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				for {
					tx := m.Begin(false)
					v, _, _ := tx.Get([]byte("n"))
					n := 0
					fmt.Sscan(string(v), &n)
					tx.Set([]byte("n"), fmt.Append(nil, n+1))
					_, err := tx.Commit()
					if err == nil {
						break
					}
					if !errors.Is(err, ErrConflict) {
						t.Error(err)
						return
					}
					conflicts.Add(1)
				}
			}
		})
	}
	wg.Wait()
	if got := get(t, m.Store(), "n"); got != "400" {
		t.Errorf("counter = %s, want 400 (%d conflicts retried)", got, conflicts.Load())
	}
}
