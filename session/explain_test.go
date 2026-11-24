package session

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/sandgorgon/polydb/security"
)

// times hides durations, which change from run to run.
var times = regexp.MustCompile(`\d+(\.\d+)?(ns|µs|ms|s)\b`)

// TestExplainPlans checks what explain and explain analyze show for
// joins through an index, early stops and indexes still building.
func TestExplainPlans(t *testing.T) {
	s := newScriptSession(t, false)
	eng := s.eng
	bigKind(t, eng, 3000)
	mustExec(t, eng, "test", `
define kind Pet { name: text, owner: ref<P> };
define index pet_owner on Pet(owner);
define index p_name on P(name);
find P p | where p.name = "n00007" | bind p as @o;
create Pet { name: "rex", owner: @o };
`)
	var b strings.Builder
	for i := range 200 {
		fmt.Fprintf(&b, "create Pet { name: \"pet%d\", owner: @o2 };\n", i)
	}
	mustExec(t, eng, "test", `find P p | where p.name = "n00008" | bind p as @o2;`+b.String())
	db, _ := eng.Database("test")
	if err := db.WaitIndexes(context.Background()); err != nil {
		t.Fatal(err)
	}
	explain := func(q string) string {
		t.Helper()
		return times.ReplaceAllString(rows(t, eng, security.AdminUser, "test", "explain "+q), "T")
	}

	// p is found by name, then each p's pets through pet_owner.
	got := explain(`analyze find P p, Pet x | where x.owner = p and p.name = "n00007" | show x.name`)
	want := `"find" | "p: index p_name for p.name = \"n00007\"" | 1 | 1 | null
"find" | "x: index pet_owner for x.owner = p, for each p" | "10 per row" | 1 | null
"source" | "the rows the source produced" | null | 1 | T
"where" | "where x.owner = p and p.name = \"n00007\" (checked at the source)" | null | null | null
"show" | "show x.name" | null | 1 | T
"total" | "" | null | 1 | T`
	if got != want {
		t.Errorf("join:\n%s\nwant\n%s", got, want)
	}

	// A big range sorted by an indexed field stops early.
	got = explain(`analyze find P p | where p.name > "n0" | sort p.name desc | limit 3 | show p.name`)
	if !strings.Contains(got, `"top" | "read p_name in descending order, stopping after 3 rows" | 3`) ||
		!strings.Contains(got, `"find" | "p: index p_name for p.name > \"n0\"" | 1500 | 3 | null`) {
		t.Errorf("top:\n%s", got)
	}

	// Inside the transaction that defines it, a big kind's index is still
	// building, so a scan answers and explain says why.
	got = explain(`find P p | where p.age = 3 | show p.name`)
	if !strings.Contains(got, `"find" | "p: scan kind P" | 3000`) {
		t.Errorf("before the index:\n%s", got)
	}
	got = times.ReplaceAllString(rows(t, eng, security.AdminUser, "test",
		"begin; define index p_age on P(age); explain find P p | where p.age = 3 | show p.name"), "T")
	if !strings.Contains(got, `"find" | "p: scan kind P" | 3000`) ||
		!strings.Contains(got, `"warning" | "index p_age is still building (0 entities indexed), so it isn't used yet"`) {
		t.Errorf("building:\n%s", got)
	}
}
