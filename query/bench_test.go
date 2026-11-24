package query

import (
	"fmt"
	"testing"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/lang"
	"github.com/sandgorgon/polydb/value"
)

// BenchmarkQueries times common query shapes on 20,000 people and 2,000
// orders, with the planner and, for single kinds, without it (joins
// without it take minutes).
func BenchmarkQueries(b *testing.B) {
	s := newSession(b)
	s.run(`
define kind P { email: text unique, age: int, city: text, bio: text };
define index p_age on P(age);
define index p_city_age on P(city, age);
define text index p_bio on P(bio);
define kind O { owner: ref<P>, total: int };
define index o_owner on O(owner);
`)
	words := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta"}
	err := s.db.Update(func(tx *engine.Tx) error {
		for i := range 20000 {
			id, err := tx.Create("P", engine.Fields{
				"email": value.MustText(fmt.Sprintf("e%d", i)),
				"age":   value.Int(int64(i % 90)),
				"city":  value.MustText(fmt.Sprintf("c%d", i%50)),
				"bio":   value.MustText(words[i%8] + " " + words[(i/8)%8] + fmt.Sprintf(" w%d", i%1000)),
			})
			if err != nil {
				return err
			}
			if i%10 == 0 {
				if _, err := tx.Create("O", engine.Fields{"owner": value.Ref{Target: id}, "total": value.Int(int64(i))}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	for _, c := range []struct {
		name, q string
		plain   bool
	}{
		{"unique", `find P p | where p.email = "e777" | show p.age`, true},
		{"equal", `find P p | where p.age = 7 | show count()`, true},
		{"range", `find P p | where p.age >= 85 | show count()`, true},
		{"two_fields", `find P p | where p.city = "c3" and p.age < 10 | show count()`, true},
		{"search", `find P p | where search(p.bio, "w77 alpha") | show count()`, true},
		{"top", `find P p | sort p.age desc, p.email | limit 10 | show p.email`, true},
		{"scan", `find P p | where p.email ends with "77" | show count()`, true},
		{"join_one", `find P p, O o | where o.owner = p and p.email = "e7770" | show o.total`, false},
		{"join_many", `find P p, O o | where o.owner = p and p.age = 30 | show count()`, false},
	} {
		st, err := lang.ParseStatement(c.q)
		if err != nil {
			b.Fatal(err)
		}
		for _, noPlanner := range []bool{false, true} {
			if noPlanner && !c.plain {
				continue
			}
			name := c.name
			if noPlanner {
				name += "/plain"
			}
			b.Run(name, func(b *testing.B) {
				for b.Loop() {
					tx := s.db.Begin(engine.TxOptions{User: "bench"})
					_, err := Exec(&Context{Tx: tx, NoPlanner: noPlanner}, st)
					tx.Rollback()
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
