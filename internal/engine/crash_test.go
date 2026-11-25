package engine

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/internal/model"
	"github.com/sandgorgon/polydb/internal/storage/lsm"
	"github.com/sandgorgon/polydb/internal/storage/vfs"
	"github.com/sandgorgon/polydb/value"
)

// Crash tests for Phase 6's on-disk work (plan.md step 9): each runs a
// workload on a file system that models a power cut (vfs.Mem.Crash),
// cuts it at a random moment, reopens what survived, and checks that
// every acknowledged change is there whole and nothing is half done.

// crashOptions are small settings, so that memtables flush, tables
// compact and blob segments fill and get rewritten within a short run.
func crashOptions(fs vfs.FS) Options {
	return Options{Dir: "/data", FS: fs, Now: func() time.Time { return clock }, HistorySweep: -1, StatsRefresh: -1,
		BlobThreshold: 4 << 10, BlobSegmentSize: 96 << 10,
		Storage: lsm.Options{MemtableSize: 64 << 10, TableSize: 32 << 10, LevelBaseSize: 128 << 10}}
}

// blobSize and blobContent make a value from a seed; seeds 60 apart
// share content, so some blobs are shared while most become garbage as
// they are overwritten.
func blobSize(seed int) int { return 4<<10 + (seed%7)*3000 }

func blobContent(seed int) []byte { return content(seed%60, blobSize(seed)) }

// op is a writer's change to one of its entities.
type op struct {
	create, del bool
	id          value.ID // zero for a create in flight
	seed        int
}

// crashWriters runs writers that create, update and delete entities with
// large values, and a collector, until stop. Under mu, acked holds each
// writer's entities (id → seed, -1 deleted) and inflight its change in
// progress.
type crashWriters struct {
	collected BlobResult // what the collector did (moved, segments)
	mu        sync.Mutex
	acked     []map[value.ID]int
	inflight  []*op
}

func runCrashWriters(t *testing.T, db *DB, writers int, stop *atomic.Bool, rng *rand.Rand) (*crashWriters, *sync.WaitGroup) {
	cw := &crashWriters{acked: make([]map[value.ID]int, writers), inflight: make([]*op, writers)}
	var wg sync.WaitGroup
	for w := range writers {
		cw.acked[w] = map[value.ID]int{}
		seedBase := rng.IntN(1000)
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(w), uint64(seedBase)))
			var mine []value.ID
			for i := 0; !stop.Load(); i++ {
				seed := seedBase + i
				o := &op{seed: seed}
				switch {
				case len(mine) == 0 || r.IntN(3) == 0:
					o.create = true
				case r.IntN(5) == 0:
					o.del, o.id = true, mine[r.IntN(len(mine))]
				default:
					o.id = mine[r.IntN(len(mine))]
				}
				cw.mu.Lock()
				cw.inflight[w] = o
				cw.mu.Unlock()
				var id value.ID
				err := db.Update(func(tx *Tx) error {
					switch {
					case o.create:
						var err error
						id, err = tx.Create("F", Fields{"w": value.Int(w), "seed": value.Int(seed),
							"data": must(value.NewBytes(blobContent(seed), ""))})
						return err
					case o.del:
						return tx.Delete(o.id)
					}
					return tx.Set(o.id, Fields{"seed": value.Int(seed), "data": must(value.NewBytes(blobContent(seed), ""))})
				})
				if err != nil {
					if IsConflict(err) {
						continue
					}
					return // the file system is gone (the crash) or failing
				}
				cw.mu.Lock()
				switch {
				case o.create:
					cw.acked[w][id] = seed
					mine = append(mine, id)
				case o.del:
					cw.acked[w][o.id] = -1
					for j, m := range mine {
						if m == o.id {
							mine = append(mine[:j], mine[j+1:]...)
							break
						}
					}
				default:
					cw.acked[w][o.id] = seed
				}
				cw.inflight[w] = nil
				cw.mu.Unlock()
			}
		})
	}
	wg.Go(func() {
		for !stop.Load() {
			r, err := db.CollectBlobs(context.Background())
			if err != nil && !IsConflict(err) {
				return
			}
			cw.mu.Lock()
			cw.collected.Blobs += r.Blobs
			cw.collected.Moved += r.Moved
			cw.collected.Segments += r.Segments
			cw.mu.Unlock()
			time.Sleep(time.Millisecond)
		}
	})
	return cw, &wg
}

func defineF(t *testing.T, db *DB) {
	t.Helper()
	update(t, db, func(tx *Tx) error {
		_, err := tx.DefineKind(model.Kind{Name: "F", Fields: []model.FieldDef{fd("w", "int"), fd("seed", "int"), fd("data", "bytes?")}})
		return err
	})
}

// checkCrashed checks a recovered database against what the writers had
// acknowledged when the crash came, and that every large value reads
// back.
func checkCrashed(t *testing.T, db *DB, acked []map[value.ID]int, inflight []*op) {
	t.Helper()
	found := map[value.ID]bool{}
	err := db.View(func(tx *Tx) error {
		return tx.Entities("F", func(id value.ID) bool {
			e, err := tx.Get(id)
			if err != nil {
				t.Fatalf("entity %s: %v", id, err)
			}
			wf, _ := e.Field("w")
			sf, _ := e.Field("seed")
			df, _ := e.Field("data")
			w, seed := int(wf.Value.(value.Int)), int(sf.Value.(value.Int))
			data := loaded(t, df.Value).(value.Bytes).Go()
			if !bytes.Equal(data, blobContent(seed)) {
				t.Fatalf("entity %s: its large value doesn't match its seed %d", id, seed)
			}
			found[id] = true
			want, known := acked[w][id]
			fl := inflight[w]
			switch {
			case !known:
				if fl == nil || !fl.create || fl.seed != seed {
					t.Fatalf("entity %s of writer %d, seed %d, was never acknowledged", id, w, seed)
				}
			case want == -1:
				t.Fatalf("entity %s was deleted, and came back", id)
			case want != seed && !(fl != nil && fl.id == id && !fl.del && fl.seed == seed):
				t.Fatalf("entity %s has seed %d, acknowledged %d", id, seed, want)
			}
			return true
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	for w, m := range acked {
		for id, seed := range m {
			fl := inflight[w]
			if seed >= 0 && !found[id] && !(fl != nil && fl.del && fl.id == id) {
				t.Fatalf("acknowledged entity %s of writer %d is gone", id, w)
			}
		}
	}
}

// Large values written, rewritten, shared and collected survive a crash
// as acknowledged, and the store stays collectable.
func TestCrashBlobs(t *testing.T) {
	rng := rand.New(rand.NewPCG(9, 9))
	iters := 12
	if testing.Short() {
		iters = 3
	}
	for iter := range iters {
		mem := vfs.NewMem()
		eng := must(Open(crashOptions(mem)))
		db := must(eng.CreateDatabase("d"))
		defineF(t, db)
		var stop atomic.Bool
		cw, wg := runCrashWriters(t, db, 3, &stop, rng)
		time.Sleep(time.Duration(10+rng.IntN(60)) * time.Millisecond)
		cw.mu.Lock()
		acked := make([]map[value.ID]int, len(cw.acked))
		for i, m := range cw.acked {
			acked[i] = map[value.ID]int{}
			for k, v := range m {
				acked[i][k] = v
			}
		}
		inflight := append([]*op(nil), cw.inflight...)
		crashed := mem.Crash(rand.New(rand.NewPCG(uint64(iter), 1)))
		cw.mu.Unlock()
		stop.Store(true)
		wg.Wait()
		eng.Close()

		eng2, err := Open(crashOptions(crashed))
		if err != nil {
			t.Fatalf("iteration %d: reopen: %v", iter, err)
		}
		db2 := must(eng2.Database("d"))
		n := 0
		for _, m := range acked {
			n += len(m)
		}
		st := must(blobStatsOf(db2))
		t.Logf("iteration %d: %d entities acknowledged, collector freed %d blobs, moved %d chunks, deleted %d segments; after the crash %d blobs in %d segments",
			iter, n, cw.collected.Blobs, cw.collected.Moved, cw.collected.Segments, st.Blobs, st.Segments)
		checkCrashed(t, db2, acked, inflight)
		// Collect what the crash left over, and check again.
		if _, err := db2.CollectBlobs(context.Background()); err != nil {
			t.Fatalf("iteration %d: collect after the crash: %v", iter, err)
		}
		checkCrashed(t, db2, acked, inflight)
		if err := db2.VerifyIndexes(); err != nil {
			t.Fatal(err)
		}
		update(t, db2, func(tx *Tx) error {
			_, err := tx.Create("F", Fields{"w": value.Int(99), "seed": value.Int(1), "data": must(value.NewBytes(blobContent(1), ""))})
			return err
		})
		eng2.Close()
	}
}

// A background alter cut by a crash at any point finishes after the
// restart, with every entity converted.
func TestCrashAlter(t *testing.T) {
	rng := rand.New(rand.NewPCG(4, 4))
	iters := 8
	if testing.Short() {
		iters = 2
	}
	for iter := range iters {
		mem := vfs.NewMem()
		eng := must(Open(crashOptions(mem)))
		db := must(eng.CreateDatabase("d"))
		update(t, db, func(tx *Tx) error {
			_, err := tx.DefineKind(model.Kind{Name: "P", Fields: []model.FieldDef{fd("n", "int"), fd("note", "text?")}})
			return err
		})
		const n = 2500
		for i := 0; i < n; i += 500 {
			update(t, db, func(tx *Tx) error {
				for j := i; j < i+500; j++ {
					if _, err := tx.Create("P", Fields{"n": value.Int(j), "note": txt("note")}); err != nil {
						return err
					}
				}
				return nil
			})
		}
		update(t, db, func(tx *Tx) error {
			return tx.AlterKind("P", func(k *model.Kind) error {
				k.Fields = []model.FieldDef{fd("n", "decimal")}
				return nil
			})
		})
		time.Sleep(time.Duration(rng.IntN(40)) * time.Millisecond)
		crashed := mem.Crash(rand.New(rand.NewPCG(uint64(iter), 2)))
		eng.Close()

		eng2, err := Open(crashOptions(crashed))
		if err != nil {
			t.Fatalf("iteration %d: reopen: %v", iter, err)
		}
		db2 := must(eng2.Database("d"))
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		if err := db2.WaitAlters(ctx); err != nil {
			t.Fatal(err)
		}
		cancel()
		seen := 0
		err = db2.View(func(tx *Tx) error {
			return tx.Entities("P", func(id value.ID) bool {
				e, err := tx.Get(id)
				if err != nil {
					t.Fatal(err)
				}
				f, _ := e.Field("n")
				if f.Value.Code() != value.CodeDecimal {
					t.Fatalf("iteration %d: entity %s still has %s", iter, id, f.Value)
				}
				if _, ok := e.Field("note"); ok {
					t.Fatalf("iteration %d: the dropped field survived", iter)
				}
				seen++
				return true
			})
		})
		if err != nil || seen != n {
			t.Fatalf("iteration %d: %d entities, %v", iter, seen, err)
		}
		if err := db2.VerifyIndexes(); err != nil {
			t.Fatal(err)
		}
		update(t, db2, func(tx *Tx) error {
			_, err := tx.Create("P", Fields{"n": value.Int(1)})
			return err
		})
		eng2.Close()
	}
}

// A backup or restore cut by a crash leaves either a whole, restorable
// result or none, and the source database untouched.
func TestCrashBackupRestore(t *testing.T) {
	rng := rand.New(rand.NewPCG(6, 6))
	iters := 10
	if testing.Short() {
		iters = 3
	}
	for iter := range iters {
		mem := vfs.NewMem()
		eng := must(Open(crashOptions(mem)))
		db := must(eng.CreateDatabase("bank"))
		accounts(t, db, 300)
		restoring := iter%2 == 1
		if restoring {
			must(db.Backup(context.Background(), nil, "/bk"))
		}
		done := make(chan error, 1)
		go func() {
			if restoring {
				_, _, err := eng.RestoreDatabase(context.Background(), "copy", nil, "/bk")
				done <- err
			} else {
				_, err := db.Backup(context.Background(), nil, "/bk")
				done <- err
			}
		}()
		time.Sleep(time.Duration(rng.IntN(15)) * time.Millisecond)
		crashed := mem.Crash(rand.New(rand.NewPCG(uint64(iter), 3)))
		<-done
		eng.Close()

		eng2, err := Open(crashOptions(crashed))
		if err != nil {
			t.Fatalf("iteration %d: reopen: %v", iter, err)
		}
		if sum, n := total(t, must(eng2.Database("bank"))); sum != 300*100 || n != 300 {
			t.Fatalf("iteration %d: the source holds %d accounts and %d", iter, n, sum)
		}
		if restoring {
			// The restored database is whole, or not there at all.
			_, cerr := eng2.Database("copy")
			t.Logf("iteration %d: restore cut by the crash: copy there: %v", iter, cerr == nil)
			if cp, err := eng2.Database("copy"); err == nil {
				if sum, n := total(t, cp); sum != 300*100 || n != 300 {
					t.Fatalf("iteration %d: a half-restored database: %d accounts and %d", iter, n, sum)
				}
			}
			if names, _ := crashed.List("/data/databases"); len(names) > len(eng2.Databases()) {
				t.Fatalf("iteration %d: a restore's leftovers: %v", iter, names)
			}
		} else {
			// A finished backup restores whole; an unfinished one is refused.
			_, _, err := eng2.RestoreDatabase(context.Background(), "copy", nil, "/bk")
			t.Logf("iteration %d: backup cut by the crash: restore gives %v", iter, err)
			switch {
			case err == nil:
				if sum, n := total(t, must(eng2.Database("copy"))); sum != 300*100 || n != 300 {
					t.Fatalf("iteration %d: restored %d accounts and %d", iter, n, sum)
				}
			case CodeOf(err) != CodeNotFound:
				t.Fatalf("iteration %d: restoring a cut backup: %v", iter, err)
			}
		}
		eng2.Close()
	}
}

// An I/O error at any point of writes with large values, collection and
// a backup fails the operation and leaves a database that opens with
// every acknowledged change.
func TestFaultsBlobsAndBackup(t *testing.T) {
	var fl *op // the change in flight when the run failed
	work := func(fs vfs.FS) (acked map[value.ID]int, failed bool) {
		acked, fl = map[value.ID]int{}, nil
		eng, err := Open(crashOptions(fs))
		if err != nil {
			return acked, true
		}
		defer eng.Close()
		db, err := eng.CreateDatabase("d")
		if err != nil {
			return acked, true
		}
		err = db.Update(func(tx *Tx) error {
			_, err := tx.DefineKind(model.Kind{Name: "F", Fields: []model.FieldDef{fd("w", "int"), fd("seed", "int"), fd("data", "bytes?")}})
			return err
		})
		if err != nil {
			return acked, true
		}
		var ids []value.ID
		for i := range 40 {
			var id value.ID
			fl = &op{create: !(i%4 == 3 && len(ids) > 0), seed: i}
			if !fl.create {
				fl.id = ids[i%len(ids)]
			}
			err := db.Update(func(tx *Tx) error {
				if i%4 == 3 && len(ids) > 0 {
					id = ids[i%len(ids)]
					return tx.Set(id, Fields{"seed": value.Int(i), "data": must(value.NewBytes(blobContent(i), ""))})
				}
				var err error
				id, err = tx.Create("F", Fields{"w": value.Int(0), "seed": value.Int(i), "data": must(value.NewBytes(blobContent(i), ""))})
				return err
			})
			if err != nil {
				return acked, true
			}
			if _, ok := acked[id]; !ok {
				ids = append(ids, id)
			}
			acked[id] = i
			fl = nil
			if i%10 == 9 {
				if _, err := db.CollectBlobs(context.Background()); err != nil {
					return acked, true
				}
			}
		}
		if _, err := db.Backup(context.Background(), nil, "/bk"); err != nil {
			return acked, true
		}
		return acked, false
	}
	dry := vfs.NewFaulty(vfs.NewMem())
	work(dry)
	total := dry.Ops()
	rng := rand.New(rand.NewPCG(7, 7))
	runs := 60
	if testing.Short() {
		runs = 15
	}
	t.Logf("%d file operations in a clean run", total)
	for range runs {
		at := 1 + rng.Int64N(total)
		mem := vfs.NewMem()
		fs := vfs.NewFaulty(mem)
		fs.FailAt(at, rng.IntN(2) == 0)
		acked, failed := work(fs)
		what := fmt.Sprintf("failing operation %d of %d (%v)", at, total, fs.Failed())
		if !failed && len(fs.Failed()) > 0 {
			// The failure hit something retried or optional; fine.
			t.Logf("%s: the run succeeded", what)
		}
		crashed := mem.Crash(rand.New(rand.NewPCG(uint64(at), 4)))
		eng, err := Open(crashOptions(crashed))
		if err != nil {
			t.Fatalf("%s: reopen: %v", what, err)
		}
		db, err := eng.Database("d")
		if err != nil {
			if len(acked) > 0 {
				t.Fatalf("%s: database gone with %d acknowledged changes", what, len(acked))
			}
			eng.Close()
			continue
		}
		checkCrashed(t, db, []map[value.ID]int{acked}, []*op{fl})
		if _, err := db.CollectBlobs(context.Background()); err != nil {
			t.Fatalf("%s: collect after recovery: %v", what, err)
		}
		if _, _, err := eng.RestoreDatabase(context.Background(), "copy", nil, "/bk"); err != nil && CodeOf(err) != CodeNotFound {
			t.Fatalf("%s: restoring the backup: %v", what, err)
		}
		eng.Close()
	}
}

func blobStatsOf(db *DB) (BlobStats, error) {
	var st BlobStats
	err := db.View(func(tx *Tx) (err error) { st, err = tx.BlobStats(); return })
	return st, err
}
