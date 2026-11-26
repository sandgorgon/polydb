package transfer_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/sandgorgon/polydb/internal/engine"
	"github.com/sandgorgon/polydb/internal/security"
	"github.com/sandgorgon/polydb/internal/session"
	"github.com/sandgorgon/polydb/internal/transfer"
	"github.com/sandgorgon/polydb/value"
)

// An import that fails partway, with batches committing in parallel,
// keeps the batches that committed, and its error says how many entities
// that is: exactly what the database then holds.
func TestImportFailsPartway(t *testing.T) {
	ctx := context.Background()
	eng, err := engine.Open(engine.Options{HistorySweep: -1, StatsRefresh: -1, PasswordIterations: security.MinIterations})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	src, _ := eng.CreateDatabase("src")
	dst, _ := eng.CreateDatabase("dst")
	s := session.New(eng, security.AdminUser)
	defer s.Close()
	if _, err := s.Exec(ctx, `use src; define kind P { u: int unique };
		use dst; define kind P { u: int unique }; create P { u: 30000 }`, nil); err != nil {
		t.Fatal(err)
	}
	const n = 50000
	for i := 0; i < n; i += 5000 {
		if err := src.Update(func(tx *engine.Tx) error {
			for j := i; j < i+5000; j++ {
				if _, err := tx.Create("P", engine.Fields{"u": value.Int(j)}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if _, err := transfer.ExportDatabase(ctx, src, &buf, transfer.Native); err != nil {
		t.Fatal(err)
	}
	st, err := transfer.ImportDatabase(ctx, dst, &buf, transfer.ImportOptions{})
	if err == nil || engine.CodeOf(err) != engine.CodeConstraint && !strings.Contains(err.Error(), "unique") {
		t.Fatalf("import: %v", err)
	}
	var held int64
	dst.View(func(tx *engine.Tx) error {
		c, err := tx.CountEntities("P")
		held = int64(c)
		return err
	})
	if st.Entities != held-1 || st.Entities >= n {
		t.Errorf("the import says %d entities; the database holds %d besides its own", st.Entities, held-1)
	}
	if !strings.Contains(err.Error(), "after ") {
		t.Errorf("the error doesn't say how far it got: %v", err)
	}
}
