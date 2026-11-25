package session

import (
	"context"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/security"
	"github.com/sandgorgon/polydb/storage/vfs"
	"github.com/sandgorgon/polydb/value"
)

// An export to a file cut by a crash leaves the whole file or none.
func TestCrashExportFile(t *testing.T) {
	rng := rand.New(rand.NewPCG(8, 8))
	for iter := range 8 {
		mem := vfs.NewMem()
		eng, err := engine.Open(engine.Options{Dir: "/data", FS: mem, HistorySweep: -1, StatsRefresh: -1})
		if err != nil {
			t.Fatal(err)
		}
		s := New(eng, security.AdminUser)
		big := map[string]value.Value{"b": value.MustText(strings.Repeat("words ", 20000))}
		if _, err := s.Exec(context.Background(), `create database d; use d; define kind K { n: int, b: text? }`, nil); err != nil {
			t.Fatal(err)
		}
		for i := range 30 {
			s.Exec(context.Background(), `create K { n: `+itoa(i)+`, b: $b }`, big)
		}
		done := make(chan error, 1)
		go func() {
			_, err := s.Exec(context.Background(), `export database d to "d.jsonl"`, nil)
			done <- err
		}()
		time.Sleep(time.Duration(rng.IntN(4000)) * time.Microsecond)
		crashed := mem.Crash(rand.New(rand.NewPCG(uint64(iter), 5)))
		<-done
		s.Close()
		eng.Close()

		eng2, err := engine.Open(engine.Options{Dir: "/data", FS: crashed, HistorySweep: -1, StatsRefresh: -1})
		if err != nil {
			t.Fatal(err)
		}
		s2 := New(eng2, security.AdminUser)
		_, err = s2.Exec(context.Background(), `create database e; import into database e from "d.jsonl"`, nil)
		switch {
		case err == nil:
			if got := sessionRows(t, s2, `use e; find K k | show count()`); got != "30" {
				t.Fatalf("iteration %d: a whole export imported %s entities", iter, got)
			}
			t.Logf("iteration %d: the export was whole", iter)
		case strings.Contains(err.Error(), "no file"):
			t.Logf("iteration %d: the export wasn't there", iter)
		default:
			t.Fatalf("iteration %d: a cut export: %v", iter, err)
		}
		s2.Close()
		eng2.Close()
	}
}
