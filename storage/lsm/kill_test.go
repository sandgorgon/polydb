//go:build linux || darwin

package lsm

import (
	"bufio"
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/sandgorgon/polydb/storage"
	"github.com/sandgorgon/polydb/storage/vfs"
)

// TestKill runs a writer in a child process and kills it with SIGKILL at
// random moments, mid-commit, mid-flush or mid-compaction, reopening the
// same directory each time. Every batch the child acknowledged must
// survive, whole. (A killed process loses nothing the kernel already
// has; losing what it doesn't is what the crash-simulating file system
// tests cover.)
func TestKill(t *testing.T) {
	if testing.Short() {
		t.Skip("starts child processes")
	}
	dir := t.TempDir()
	rng := rand.New(rand.NewPCG(5, 5))
	acked := map[string]bool{}
	for round := range 12 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestKillChild$", "-test.count=1")
		cmd.Env = append(os.Environ(), "POLYDB_KILL_DIR="+dir, fmt.Sprintf("POLYDB_KILL_ROUND=%d", round))
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		target := 200 + rng.IntN(2500)
		n := 0
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			tag, ok := strings.CutPrefix(sc.Text(), "ack ")
			if !ok {
				continue
			}
			acked[tag] = true
			if n++; n == target {
				cmd.Process.Kill()
			}
		}
		cmd.Wait()
		if n < target {
			t.Fatalf("round %d: the child stopped after %d batches:\n%s", round, n, stderr.String())
		}

		o := tiny(vfs.OS)
		o.Dir = dir
		s, err := Open(o)
		if err != nil {
			t.Fatalf("round %d: reopen: %v", round, err)
		}
		counts := map[string]int{}
		it := s.Iter(storage.IterOptions{})
		for it.Next() {
			counts[strings.TrimRight(string(it.Value()), "-")]++
		}
		if err := it.Err(); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		it.Close()
		for tag := range acked {
			if counts[tag] != 3 {
				t.Fatalf("round %d: acknowledged batch %s has %d of 3 keys", round, tag, counts[tag])
			}
		}
		for tag, c := range counts {
			if c != 3 {
				t.Fatalf("round %d: batch %s is partial (%d of 3 keys)", round, tag, c)
			}
		}
		met := s.Metrics()
		s.Close()
		t.Logf("round %d: killed after %d batches; %d acknowledged in all; levels %v", round, n, len(acked), met.Levels)
	}
}

// TestKillChild is TestKill's child process: it writes until killed,
// printing each acknowledged batch.
func TestKillChild(t *testing.T) {
	dir := os.Getenv("POLYDB_KILL_DIR")
	if dir == "" {
		t.Skip("run by TestKill")
	}
	o := tiny(vfs.OS)
	o.Dir = dir
	s, err := Open(o)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var mu sync.Mutex
	w := bufio.NewWriter(os.Stdout)
	round := os.Getenv("POLYDB_KILL_ROUND")
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Go(func() {
			for i := 0; ; i++ {
				tag := fmt.Sprintf("r%s-g%d-%d", round, g, i)
				var b storage.Batch
				for k := range 3 {
					// A random prefix spreads keys over the key space, so
					// compactions mix rounds; the tag keeps them unique.
					b.Set(fmt.Appendf(nil, "%08x/%s/%d", rand.Uint32(), tag, k), []byte(tag+strings.Repeat("-", 30)))
				}
				if _, err := s.Apply(&b); err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
				// Printed only once acknowledged.
				mu.Lock()
				fmt.Fprintf(w, "ack %s\n", tag)
				w.Flush()
				mu.Unlock()
			}
		})
	}
	wg.Wait()
}
